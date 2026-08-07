package core

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// errNoTools reports that a LoopAgent has no tool registry wired up.
var errNoTools = errors.New("core: agent loop has no tool registry")

// ExecutionMode controls how multiple tool calls are executed when the model
// returns more than one tool call in a single response.
type ExecutionMode int

const (
	// ExecutionModeSequential runs tool calls one at a time (default).
	ExecutionModeSequential ExecutionMode = iota
	// ExecutionModeParallel runs all tool calls concurrently.
	ExecutionModeParallel
)

// WithExecutionMode sets the tool execution mode for the LoopAgent.
func WithExecutionMode(mode ExecutionMode) LoopOption {
	return func(c *loopConfig) { c.executionMode = mode }
}

// ParallelToolResult holds the outcome of a single tool call executed in
// parallel. Results are returned in the same order as the input calls.
type ParallelToolResult struct {
	// ID is the tool call identifier from the model.
	ID string
	// Name is the name of the tool that was invoked.
	Name string
	// Output is the textual result produced by the tool.
	Output string
	// Err is the execution error, if any.
	Err error
}

// executeToolsParallel runs multiple tool calls concurrently against the
// registry. Results are returned in the same order as the input calls. It is
// thread-safe: each goroutine writes to its own index in a pre-allocated
// slice, so no locking is required for the results. When es is non-nil,
// streaming tools push output lines through the EventStream in real time.
//
// When ctx is canceled before all tools complete, executeToolsParallel
// returns a snapshot of results: completed tools have their real output,
// while incomplete tools are marked with Err set to ctx.Err(). The returned
// error is non-nil when the context was canceled.
func executeToolsParallel(ctx context.Context, tr tools.ToolRegistry, calls []llm.ToolCall, es EventStream) ([]ParallelToolResult, error) {
	if len(calls) == 0 {
		return nil, nil
	}

	results := make([]ParallelToolResult, len(calls))
	dones := make([]chan struct{}, len(calls))
	for i := range dones {
		dones[i] = make(chan struct{})
	}

	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		go func(idx int, call llm.ToolCall) {
			defer wg.Done()
			defer close(dones[idx])
			slog.Info("core.loop.tool_call_parallel",
				"tool", call.Name,
				"index", idx,
				"call_id", call.ID,
			)

			output, err := executeSingleTool(ctx, tr, toToolsCall(call), es)
			results[idx] = ParallelToolResult{
				ID:     call.ID,
				Name:   call.Name,
				Output: output,
				Err:    err,
			}
		}(i, tc)
	}

	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
		// All completed normally.
	case <-ctx.Done():
		// Context canceled - build a safe snapshot of completed results.
		// For completed tools (done channel closed), the result is safe to
		// read because the write happened-before the channel close. For
		// incomplete tools, mark as canceled. A separate slice is used so
		// that still-running goroutines writing to the original results
		// slice do not race with the caller.
		snapshot := make([]ParallelToolResult, len(calls))
		for i, done := range dones {
			select {
			case <-done:
				snapshot[i] = results[i]
			default:
				snapshot[i] = ParallelToolResult{
					ID:   calls[i].ID,
					Name: calls[i].Name,
					Err:  ctx.Err(),
				}
			}
		}
		slog.Info("core.loop.parallel_canceled", "count", len(calls))
		return snapshot, ctx.Err()
	}

	slog.Info("core.loop.parallel_complete", "count", len(calls))
	return results, nil
}

// executeSingleTool looks up the tool in the registry and runs it, returning
// the output string. It is shared by both sequential and parallel execution
// paths. When the tool implements tools.StreamingBashTool and es is non-nil,
// it uses ExecuteStreaming to push output lines in real time.
func executeSingleTool(ctx context.Context, tr tools.ToolRegistry, call tools.ToolCall, es EventStream) (string, error) {
	if tr == nil {
		return "", errNoTools
	}
	def, err := tr.Get(ctx, call.Name)
	if err != nil {
		return "", err
	}
	// Check if the tool supports streaming output.
	if st, ok := def.(tools.StreamingBashTool); ok && es != nil {
		sink := &eventStreamSink{es: es}
		result, err := st.ExecuteStreaming(ctx, call, sink)
		if err != nil {
			return "", err
		}
		if result == nil {
			return "", nil
		}
		return result.Output, nil
	}
	result, err := def.Execute(ctx, call)
	if err != nil {
		return "", err
	}
	if result == nil {
		return "", nil
	}
	return result.Output, nil
}
