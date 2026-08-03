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
// slice, so no locking is required for the results.
func executeToolsParallel(ctx context.Context, tr tools.ToolRegistry, calls []llm.ToolCall) []ParallelToolResult {
	results := make([]ParallelToolResult, len(calls))
	if len(calls) == 0 {
		return results
	}

	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		go func(idx int, call llm.ToolCall) {
			defer wg.Done()
			slog.Info("core.loop.tool_call_parallel",
				"tool", call.Name,
				"index", idx,
				"call_id", call.ID,
			)

			output, err := executeSingleTool(ctx, tr, toToolsCall(call))
			results[idx] = ParallelToolResult{
				ID:     call.ID,
				Name:   call.Name,
				Output: output,
				Err:    err,
			}
		}(i, tc)
	}
	wg.Wait()

	slog.Info("core.loop.parallel_complete", "count", len(calls))
	return results
}

// executeSingleTool looks up the tool in the registry and runs it, returning
// the output string. It is shared by both sequential and parallel execution
// paths.
func executeSingleTool(ctx context.Context, tr tools.ToolRegistry, call tools.ToolCall) (string, error) {
	if tr == nil {
		return "", errNoTools
	}
	def, err := tr.Get(ctx, call.Name)
	if err != nil {
		return "", err
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
