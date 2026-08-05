package core

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// SubagentTask describes a unit of work delegated to a sub-agent.
type SubagentTask struct {
	// ID uniquely identifies the task and names the spawned sub-agent.
	ID string
	// Prompt is the instruction the sub-agent executes.
	Prompt string
	// SystemPrompt is an explicit system prompt for the sub-agent. When empty,
	// Role (if set) selects a role template; otherwise the default prompt is
	// used.
	SystemPrompt string
	// Role optionally selects a role template (researcher, implementer,
	// reviewer, tester) when SystemPrompt is empty.
	Role string
	// Tools lists the tool names available to the sub-agent.
	Tools []string
	// Model optionally overrides the LLM model used by the sub-agent. When
	// empty, the sub-agent inherits the parent model.
	Model string
	// MaxTurns bounds the sub-agent turn loop. Zero leaves it unset.
	MaxTurns int
}

// SubagentResult is the outcome of dispatching a SubagentTask.
type SubagentResult struct {
	// TaskID links the result back to the originating task.
	TaskID string
	// Content is the final message produced by the sub-agent.
	Content string
	// Error carries any error the sub-agent reported.
	Error error
	// Duration is how long the dispatch took end to end.
	Duration time.Duration
}

// SubagentDispatcher manages sub-agent lifecycle and task dispatch.
type SubagentDispatcher interface {
	// Dispatch creates a sub-agent for the task, runs it to completion, and
	// returns the result.
	Dispatch(ctx context.Context, task SubagentTask) (SubagentResult, error)
	// ParallelDispatch dispatches all tasks concurrently and returns results
	// in input order. The first error encountered is returned (if any),
	// though all tasks are still attempted.
	ParallelDispatch(ctx context.Context, tasks []SubagentTask) ([]SubagentResult, error)
	// ListRunning returns the tasks currently in flight.
	ListRunning() []SubagentTask
}

// DefaultSubagentDispatcher is the default SubagentDispatcher. It uses a
// SubAgentFactory to spawn sub-agents, tracks in-flight tasks, and emits
// slog.Debug tracing around dispatch lifecycle events.
type DefaultSubagentDispatcher struct {
	factory SubAgentFactory

	mu      sync.Mutex
	running map[string]SubagentTask
}

var _ SubagentDispatcher = (*DefaultSubagentDispatcher)(nil)

// NewDefaultSubagentDispatcher builds a dispatcher backed by factory. A nil
// factory falls back to the process-wide default SubAgentFactory.
func NewDefaultSubagentDispatcher(factory SubAgentFactory) *DefaultSubagentDispatcher {
	if factory == nil {
		factory = GetSubAgentFactory()
	}
	return &DefaultSubagentDispatcher{
		factory: factory,
		running: make(map[string]SubagentTask),
	}
}

// Dispatch creates a sub-agent for task, streams its events to a drain, waits
// for the final message, and returns the result. The task is tracked as
// running for the duration of the dispatch.
func (d *DefaultSubagentDispatcher) Dispatch(ctx context.Context, task SubagentTask) (SubagentResult, error) {
	start := time.Now()
	slog.Debug("core.subagent_dispatcher.dispatch",
		"task_id", task.ID,
		"prompt_len", len(task.Prompt),
		"tools", len(task.Tools),
		"max_turns", task.MaxTurns,
	)

	config := SubAgentConfig{
		Name:         task.ID,
		SystemPrompt: resolveSubAgentSystemPrompt(task),
		Tools:        task.Tools,
		Model:        task.Model,
		MaxTurns:     task.MaxTurns,
	}
	sub, err := d.factory.Create(ctx, task.ID, config)
	if err != nil {
		res := SubagentResult{TaskID: task.ID, Error: err, Duration: time.Since(start)}
		slog.Debug("core.subagent_dispatcher.create_failed", "task_id", task.ID, "error", err)
		return res, err
	}

	d.mu.Lock()
	d.running[task.ID] = task
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.running, task.ID)
		d.mu.Unlock()
	}()

	evCh, err := sub.Run(ctx, task.Prompt)
	if err != nil {
		res := SubagentResult{TaskID: task.ID, Error: err, Duration: time.Since(start)}
		slog.Debug("core.subagent_dispatcher.run_failed", "task_id", task.ID, "error", err)
		return res, err
	}

	// Drain the event stream so the sub-agent is never blocked publishing
	// events while we wait for the final message.
	go func() {
		for range evCh {
		}
	}()

	final, waitErr := sub.Wait(ctx)
	duration := time.Since(start)

	slog.Debug("core.subagent_dispatcher.complete",
		"task_id", task.ID,
		"duration", duration,
		"error", waitErr != nil,
	)

	return SubagentResult{
		TaskID:   task.ID,
		Content:  final.Content,
		Error:    waitErr,
		Duration: duration,
	}, waitErr
}

// ListRunning returns a snapshot of the currently in-flight tasks.
func (d *DefaultSubagentDispatcher) ListRunning() []SubagentTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]SubagentTask, 0, len(d.running))
	for _, t := range d.running {
		out = append(out, t)
	}
	return out
}

// ParallelDispatch dispatches all tasks concurrently using sync.WaitGroup,
// collects results preserving input order, and returns the first error
// encountered (if any). All tasks are attempted regardless of individual
// failures.
func (d *DefaultSubagentDispatcher) ParallelDispatch(ctx context.Context, tasks []SubagentTask) ([]SubagentResult, error) {
	return nil, nil
}

// subagentDispatcherAdapter bridges a core.SubagentDispatcher to the
// tools.SubagentDispatcher contract. It exists because the tools package
// cannot import core (core already depends on tools), so the tool defines its
// own consumer-side contract that this adapter satisfies.
type subagentDispatcherAdapter struct {
	d SubagentDispatcher
}

var _ tools.SubagentDispatcher = (*subagentDispatcherAdapter)(nil)

// AdaptSubagentDispatcher wraps a core.SubagentDispatcher so it can back a
// tools.SubagentTool.
func AdaptSubagentDispatcher(d SubagentDispatcher) tools.SubagentDispatcher {
	return &subagentDispatcherAdapter{d: d}
}

// Dispatch converts the tools-level task into a core task, delegates, and
// converts the result back.
func (a *subagentDispatcherAdapter) Dispatch(ctx context.Context, task tools.SubagentTask) (tools.SubagentResult, error) {
	res, err := a.d.Dispatch(ctx, SubagentTask{
		ID:           task.ID,
		Prompt:       task.Prompt,
		SystemPrompt: task.SystemPrompt,
		Role:         task.Role,
		Tools:        task.Tools,
		Model:        task.Model,
		MaxTurns:     task.MaxTurns,
	})
	return tools.SubagentResult{
		TaskID:   res.TaskID,
		Content:  res.Content,
		Error:    res.Error,
		Duration: res.Duration,
	}, err
}

// ParallelDispatch converts the tools-level tasks into core tasks, delegates
// concurrently, and converts the results back.
func (a *subagentDispatcherAdapter) ParallelDispatch(ctx context.Context, tasks []tools.SubagentTask) ([]tools.SubagentResult, error) {
	coreTasks := make([]SubagentTask, len(tasks))
	for i, t := range tasks {
		coreTasks[i] = SubagentTask{
			ID:           t.ID,
			Prompt:       t.Prompt,
			SystemPrompt: t.SystemPrompt,
			Role:         t.Role,
			Tools:        t.Tools,
			Model:        t.Model,
			MaxTurns:     t.MaxTurns,
		}
	}
	results, err := a.d.ParallelDispatch(ctx, coreTasks)
	if err != nil {
		return nil, err
	}
	out := make([]tools.SubagentResult, len(results))
	for i, r := range results {
		out[i] = tools.SubagentResult{
			TaskID:   r.TaskID,
			Content:  r.Content,
			Error:    r.Error,
			Duration: r.Duration,
		}
	}
	return out, nil
}

// ListRunning converts the core running tasks to the tools-level type.
func (a *subagentDispatcherAdapter) ListRunning() []tools.SubagentTask {
	tasks := a.d.ListRunning()
	out := make([]tools.SubagentTask, len(tasks))
	for i, t := range tasks {
		out[i] = tools.SubagentTask{
			ID:           t.ID,
			Prompt:       t.Prompt,
			SystemPrompt: t.SystemPrompt,
			Role:         t.Role,
			Tools:        t.Tools,
			Model:        t.Model,
			MaxTurns:     t.MaxTurns,
		}
	}
	return out
}

// NewSubagentTool builds a tools.SubagentTool backed by a core
// SubagentDispatcher. This is the integration seam for callers that have a
// core dispatcher and want a registered tool.
func NewSubagentTool(d SubagentDispatcher) *tools.SubagentTool {
	return tools.NewSubagentTool(AdaptSubagentDispatcher(d))
}
