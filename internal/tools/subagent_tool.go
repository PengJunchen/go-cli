package tools //exempt:scan009

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// requestIDCounter guarantees unique request ids across concurrent callers.
var requestIDCounter atomic.Uint64

// SubagentTask describes a unit of work delegated to a sub-agent. This is the
// tools-package consumer-side contract; it mirrors core.SubagentTask. A local
// copy is required because the tools package cannot import core (core already
// depends on tools), so the tool depends on its own interface and the core
// package provides an adapter (core.AdaptSubagentDispatcher).
type SubagentTask struct {
	// ID uniquely identifies the task and names the spawned sub-agent.
	ID string
	// Prompt is the instruction the sub-agent executes.
	Prompt string
	// SystemPrompt is an explicit system prompt for the sub-agent. When empty,
	// Role (if set) selects a role template; otherwise the dispatcher applies a
	// default. system_prompt takes precedence over role.
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

// SubagentDispatcher manages sub-agent lifecycle and task dispatch. It is the
// tools-package consumer-side contract; core.DefaultSubagentDispatcher plus
// core.AdaptSubagentDispatcher satisfy it.
//
//exempt:scan012 // consumer-side interface; default impl in core package
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

// SubagentTool is a ToolDefinition that delegates a prompt to a sub-agent via
// a SubagentDispatcher.
type SubagentTool struct {
	dispatcher SubagentDispatcher
}

var _ ToolDefinition = (*SubagentTool)(nil)
var _ Parameterized = (*SubagentTool)(nil)
var _ PromptGuideliner = (*SubagentTool)(nil)

// NewSubagentTool builds a SubagentTool backed by dispatcher.
func NewSubagentTool(dispatcher SubagentDispatcher) *SubagentTool {
	return &SubagentTool{dispatcher: dispatcher}
}

// Name returns the tool name.
func (t *SubagentTool) Name() string { return "dispatch_subagent" }

// Description returns guidance for the model on when and how to delegate work
// to a sub-agent.
func (t *SubagentTool) Description() string {
	return "dispatch_subagent: delegates a task to a focused sub-agent and returns its final answer.\n" +
		"\n" +
		"Use this tool to delegate a task to a focused sub-agent when the work is self-contained (e.g. research, implementation, review, or testing).\n" +
		"\n" +
		"Args:\n" +
		"- prompt (string, required): a clear, specific task description. Write it so the sub-agent can execute without extra context.\n" +
		"- id (string, optional): a unique task identifier; generated when omitted.\n" +
		"- system_prompt (string, optional): an explicit system prompt for the sub-agent. Takes precedence over role.\n" +
		"- role (string, optional): one of researcher, implementer, reviewer, tester. Selects a built-in role template when system_prompt is empty.\n" +
		"- tools ([]string, optional): tool names available to the sub-agent.\n" +
		"- model (string, optional): model name to override the sub-agent's LLM. When omitted, the sub-agent inherits the parent model.\n" +
		"- max_turns (int, optional): bound on the sub-agent turn loop.\n" +
		"\n" +
		"The sub-agent will return its result as content."
}

// Parameters returns the OpenAI-compatible JSON Schema describing the tool's
// input parameters.
func (t *SubagentTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"prompt": map[string]any{
				"type":        "string",
				"description": "A clear, specific task description. Write it so the sub-agent can execute without extra context.",
			},
			"role": map[string]any{
				"type":        "string",
				"enum":        []string{"researcher", "implementer", "reviewer", "tester"},
				"description": "Selects a built-in role template when system_prompt is empty.",
			},
			"tools": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": "Tool names available to the sub-agent. When omitted, the role-based whitelist is used.",
			},
			"model": map[string]any{
				"type":        "string",
				"description": "Model name to override the sub-agent's LLM. When omitted, inherits the parent model.",
			},
			"max_turns": map[string]any{
				"type":        "integer",
				"description": "Bound on the sub-agent turn loop. Zero leaves it unset.",
			},
			"parallel": map[string]any{
				"type":        "boolean",
				"description": "When true, dispatch the tasks array concurrently instead of a single prompt.",
			},
			"tasks": map[string]any{
				"type":        "array",
				"description": "Array of tasks for parallel mode. Each task has prompt, role, tools, model, and max_turns fields.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt": map[string]any{
							"type":        "string",
							"description": "Task description for this parallel sub-task.",
						},
						"role": map[string]any{
							"type":        "string",
							"enum":        []string{"researcher", "implementer", "reviewer", "tester"},
							"description": "Role for this parallel sub-task.",
						},
						"tools": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "Tools for this parallel sub-task.",
						},
						"model": map[string]any{
							"type":        "string",
							"description": "Model override for this parallel sub-task.",
						},
						"max_turns": map[string]any{
							"type":        "integer",
							"description": "Turn limit for this parallel sub-task.",
						},
					},
				},
			},
			"system_prompt": map[string]any{
				"type":        "string",
				"description": "An explicit system prompt for the sub-agent. Takes precedence over role.",
			},
			"id": map[string]any{
				"type":        "string",
				"description": "A unique task identifier; generated when omitted.",
			},
		},
		"required":             []string{"prompt"},
		"additionalProperties": false,
	}
}

// PromptGuidelines returns usage hints injected into the system prompt.
func (t *SubagentTool) PromptGuidelines() []string {
	return []string{
		"Use dispatch_subagent to delegate self-contained tasks to a focused sub-agent (roles: researcher, implementer, reviewer, tester)",
		"Set parallel=true with a tasks array to run independent sub-tasks concurrently and aggregate their results",
		"Avoid dispatching trivial tasks that a single direct tool call can handle",
	}
}

// Execute parses a sub-agent task from call.Args, dispatches it via the
// dispatcher, and returns the sub-agent's final answer. When the parallel
// parameter is true or a tasks array is provided, it dispatches all tasks
// concurrently via ParallelDispatch and returns a structured summary.
func (t *SubagentTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	parallel, _ := call.Args["parallel"].(bool) //nolint:errcheck
	tasksRaw := call.Args["tasks"]

	// Parallel mode: dispatch multiple tasks concurrently.
	if parallel || tasksRaw != nil {
		return t.executeParallel(ctx, call, tasksRaw)
	}

	prompt, ok := call.Args["prompt"].(string)
	if !ok || prompt == "" {
		return nil, errors.New("dispatch_subagent: missing string argument 'prompt'")
	}

	id, _ := call.Args["id"].(string) //nolint:errcheck
	if id == "" {
		id = nextRequestID("task")
	}

	// system_prompt is optional and takes precedence over role. role selects a
	// built-in role template (resolved by the dispatcher) when system_prompt is
	// empty. Both are passed through unchanged; the dispatcher applies the
	// default when neither is set.
	systemPrompt, _ := call.Args["system_prompt"].(string) //nolint:errcheck
	role, _ := call.Args["role"].(string)                  //nolint:errcheck
	model, _ := call.Args["model"].(string)                //nolint:errcheck

	task := SubagentTask{
		ID:           id,
		Prompt:       prompt,
		SystemPrompt: systemPrompt,
		Role:         role,
		Tools:        toStringSlice(call.Args["tools"]),
		Model:        model,
		MaxTurns:     toInt(call.Args["max_turns"]),
	}

	slog.Debug("tools.subagent.execute",
		"task_id", task.ID,
		"prompt_len", len(task.Prompt),
		"tools", len(task.Tools),
		"max_turns", task.MaxTurns,
	)

	res, err := t.dispatcher.Dispatch(ctx, task)
	if err != nil {
		return nil, fmt.Errorf("dispatch_subagent: %w", err)
	}

	metadata := map[string]any{
		"task_id":  res.TaskID,
		"duration": res.Duration,
	}
	if res.Error != nil {
		metadata["error"] = res.Error.Error()
		return &ToolResult{Output: res.Content, Metadata: metadata}, fmt.Errorf("dispatch_subagent: %w", res.Error)
	}

	return &ToolResult{Output: res.Content, Metadata: metadata}, nil
}

// executeParallel builds tasks from the tasks array (or a single prompt in
// parallel mode), dispatches them concurrently, and returns a structured
// summary of the results.
func (t *SubagentTool) executeParallel(ctx context.Context, call ToolCall, tasksRaw any) (*ToolResult, error) {
	tasks := buildParallelTasks(call, tasksRaw)
	if len(tasks) == 0 {
		return nil, errors.New("dispatch_subagent: parallel mode requires at least one task")
	}

	slog.Debug("tools.subagent.execute_parallel",
		"task_count", len(tasks),
	)

	results, err := t.dispatcher.ParallelDispatch(ctx, tasks)
	if err != nil {
		return nil, fmt.Errorf("dispatch_subagent: parallel dispatch: %w", err)
	}

	// Format results as structured text.
	var sb strings.Builder
	var firstErr error
	totalDuration := time.Duration(0)
	for i, res := range results {
		role := tasks[i].Role
		if role == "" {
			role = "default"
		}
		fmt.Fprintf(&sb, "Task %d (%s): ", i+1, role) //nolint:errcheck
		if res.Error != nil {
			sb.WriteString(res.Error.Error())
			if firstErr == nil {
				firstErr = res.Error
			}
		} else {
			sb.WriteString(res.Content)
		}
		sb.WriteString("\n")
		totalDuration += res.Duration
	}

	metadata := map[string]any{
		"task_count": len(results),
		"duration":   totalDuration,
		"parallel":   true,
	}
	if firstErr != nil {
		metadata["first_error"] = firstErr.Error()
	}

	output := strings.TrimRight(sb.String(), "\n")
	return &ToolResult{Output: output, Metadata: metadata}, nil
}

// buildParallelTasks constructs a slice of SubagentTask from the tasks array
// in the tool call. When parallel is true but no tasks array is provided, it
// builds a single task from the top-level prompt.
func buildParallelTasks(call ToolCall, tasksRaw any) []SubagentTask {
	// If a tasks array is provided, build tasks from it.
	if taskMaps, ok := toAnySlice(tasksRaw); ok && len(taskMaps) > 0 {
		tasks := make([]SubagentTask, 0, len(taskMaps))
		for i, tm := range taskMaps {
			m, ok := tm.(map[string]any)
			if !ok || m == nil {
				continue
			}
			id, ok := m["id"].(string)
			if !ok || id == "" {
				id = nextRequestID(fmt.Sprintf("task-%d", i+1))
			}
			tasks = append(tasks, SubagentTask{
				ID:           id,
				Prompt:       getString(m, "prompt"),
				SystemPrompt: getString(m, "system_prompt"),
				Role:         getString(m, "role"),
				Tools:        toStringSlice(m["tools"]),
				Model:        getString(m, "model"),
				MaxTurns:     toInt(m["max_turns"]),
			})
		}
		return tasks
	}

	// Parallel mode without tasks array: build a single task from top-level
	// args so the caller can use parallel=true with a single prompt.
	prompt, ok := call.Args["prompt"].(string)
	if !ok || prompt == "" {
		return nil
	}
	id, ok := call.Args["id"].(string)
	if !ok || id == "" {
		id = nextRequestID("task")
	}
	return []SubagentTask{{
		ID:           id,
		Prompt:       prompt,
		SystemPrompt: getString(call.Args, "system_prompt"),
		Role:         getString(call.Args, "role"),
		Tools:        toStringSlice(call.Args["tools"]),
		Model:        getString(call.Args, "model"),
		MaxTurns:     toInt(call.Args["max_turns"]),
	}}
}

// getString safely extracts a string value from a map[string]any.
func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, ok := m[key].(string)
	if !ok {
		return ""
	}
	return s
}

// toAnySlice coerces a value into a []any, accepting []any and []map[string]any.
func toAnySlice(v any) ([]any, bool) {
	switch s := v.(type) {
	case []any:
		return s, true
	case []map[string]any:
		out := make([]any, len(s))
		for i, m := range s {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// nextRequestID generates a unique-ish identifier for requests that do not
// supply their own id. It combines a high-resolution timestamp with a process
// counter so concurrent callers do not collide.
func nextRequestID(prefix string) string {
	requestIDCounter.Add(1)
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), requestIDCounter.Load())
}

// toStringSlice coerces a tool-call argument into a []string. It accepts
// []string and []any (the shape JSON-decoded arrays take).
func toStringSlice(v any) []string {
	switch s := v.(type) {
	case []string:
		return append([]string(nil), s...)
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			if str, ok := item.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// toInt coerces a numeric tool-call argument into an int, accepting the common
// JSON-decoded numeric types.
func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
