package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// Tools lists the tool names available to the sub-agent.
	Tools []string
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
type SubagentDispatcher interface {
	// Dispatch creates a sub-agent for the task, runs it to completion, and
	// returns the result.
	Dispatch(ctx context.Context, task SubagentTask) (SubagentResult, error)
	// ListRunning returns the tasks currently in flight.
	ListRunning() []SubagentTask
}

// SubagentTool is a ToolDefinition that delegates a prompt to a sub-agent via
// a SubagentDispatcher.
type SubagentTool struct {
	dispatcher SubagentDispatcher
}

var _ ToolDefinition = (*SubagentTool)(nil)

// NewSubagentTool builds a SubagentTool backed by dispatcher.
func NewSubagentTool(dispatcher SubagentDispatcher) *SubagentTool {
	return &SubagentTool{dispatcher: dispatcher}
}

// Name returns the tool name.
func (t *SubagentTool) Name() string { return "dispatch_subagent" }

// Description returns a brief description of the tool.
func (t *SubagentTool) Description() string {
	return "dispatch_subagent: delegates a prompt to a sub-agent and returns its final answer. Args: prompt (string, required), id (string, optional), tools ([]string, optional), max_turns (int, optional)."
}

// Execute parses a sub-agent task from call.Args, dispatches it via the
// dispatcher, and returns the sub-agent's final answer.
func (t *SubagentTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	prompt, ok := call.Args["prompt"].(string)
	if !ok || prompt == "" {
		return nil, errors.New("dispatch_subagent: missing string argument 'prompt'")
	}

	id, _ := call.Args["id"].(string)
	if id == "" {
		id = nextRequestID("task")
	}

	task := SubagentTask{
		ID:       id,
		Prompt:   prompt,
		Tools:    toStringSlice(call.Args["tools"]),
		MaxTurns: toInt(call.Args["max_turns"]),
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
