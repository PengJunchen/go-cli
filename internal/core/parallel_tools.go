package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// errNoTools reports that a LoopAgent has no tool registry wired up.
var errNoTools = errors.New("core: agent loop has no tool registry")
var errToolMissingReceipt = toolMissingReceiptError{}

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
	// IsError marks the result as an error for structured detection. It is
	// true when the tool returned a Go error or when ToolResult.IsError was
	// set by the tool implementation.
	IsError bool
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
		toolCall := toToolsCall(tc)
		workerDone := registerRunWorker(ctx, tc.ID, tc.Name)
		var workerDoneOnce sync.Once
		finishWorker := func() { workerDoneOnce.Do(workerDone) }
		wg.Add(1)
		go func(idx int, call llm.ToolCall, converted tools.ToolCall, done func()) {
			defer wg.Done()
			defer close(dones[idx])
			defer func() {
				if recover() != nil {
					markRunOutcomeUnknown(ctx)
					results[idx] = ParallelToolResult{ID: call.ID, Name: call.Name, Err: toolDispatchPanicError{}, IsError: true}
					done()
				}
			}()
			slog.Info("core.loop.tool_call_parallel",
				"tool", call.Name,
				"index", idx,
				"call_id", call.ID,
			)

			output, isErr, err := executeSingleToolRegistered(ctx, tr, converted, es, done)
			results[idx] = ParallelToolResult{
				ID:      call.ID,
				Name:    call.Name,
				Output:  output,
				Err:     err,
				IsError: isErr || err != nil,
			}
		}(i, tc, toolCall, finishWorker)
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
// the output string and an isError flag propagated from ToolResult.IsError.
// It is shared by both sequential and parallel execution paths. When the tool
// implements tools.StreamingBashTool and es is non-nil, it uses
// ExecuteStreaming to push output lines in real time.
func executeSingleTool(ctx context.Context, tr tools.ToolRegistry, call tools.ToolCall, es EventStream) (string, bool, error) {
	return executeSingleToolRegistered(ctx, tr, call, es, registerRunWorker(ctx, call.ID, call.Name))
}

func executeSingleToolRegistered(ctx context.Context, tr tools.ToolRegistry, call tools.ToolCall, es EventStream, workerDone func()) (output string, isError bool, err error) {
	if workerDone == nil {
		workerDone = func() {}
	}
	dispatched := false
	defer func() {
		if recovered := recover(); recovered != nil {
			if dispatched {
				_ = recovered
				err = toolDispatchPanicError{}
				isError = true
				markRunOutcomeUnknown(ctx)
			} else {
				_ = recovered
				err = errors.New("core: tool lookup panicked")
			}
			output = ""
		}
		if dispatched && err != nil {
			var pending tools.PendingExecutionError
			if errors.As(err, &pending) {
				registerRunPendingResource(ctx, call.ID, call.Name, pending.ExecutionDone())
				if pending.OutcomeUnknown() {
					markRunOutcomeUnknown(ctx)
				}
			} else {
				var unknown runOutcomeUnknown
				explicitOutcome := errors.As(err, &unknown)
				if explicitOutcome && unknown.OutcomeUnknown() {
					markRunOutcomeUnknown(ctx)
				} else if !explicitOutcome && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					err = toolOutcomeUnknownError{cause: err}
					markRunOutcomeUnknown(ctx)
				}
			}
		}
		workerDone()
	}()
	if tr == nil {
		return "", false, errNoTools
	}
	def, err := tr.Get(ctx, call.Name)
	if err != nil {
		return "", false, err
	}
	// Check if the tool supports streaming output.
	if st, ok := def.(tools.StreamingBashTool); ok && es != nil {
		sink := &eventStreamSink{es: es, coordinates: agentEventCoordinatesFromContext(ctx)}
		dispatched = true
		result, err := st.ExecuteStreaming(ctx, call, sink)
		if err != nil {
			return "", false, err
		}
		if result == nil {
			return "", false, errToolMissingReceipt
		}
		return result.Output, result.IsError, nil
	}
	dispatched = true
	result, err := def.Execute(ctx, call)
	if err != nil {
		return "", false, err
	}
	if result == nil {
		return "", false, errToolMissingReceipt
	}
	return result.Output, result.IsError, nil
}

type toolDispatchPanicError struct{}

func (toolDispatchPanicError) Error() string { return "core: tool execution panicked after dispatch" }

func (toolDispatchPanicError) OutcomeUnknown() bool { return true }

type toolOutcomeUnknownError struct{ cause error }

func (e toolOutcomeUnknownError) Error() string        { return "core: tool execution outcome unknown" }
func (e toolOutcomeUnknownError) OutcomeUnknown() bool { return true }
func (e toolOutcomeUnknownError) Unwrap() error        { return e.cause }

type toolMissingReceiptError struct{}

func (toolMissingReceiptError) Error() string        { return "core: tool execution returned no receipt" }
func (toolMissingReceiptError) OutcomeUnknown() bool { return true }

// errUnknownToolCallID is returned by matchToolResultsByID when a result's
// ToolCallID does not match any of the input calls.
var errUnknownToolCallID = errors.New("core: tool result has unknown ToolCallID")

// matchToolResultsByID builds a ToolCallID-to-index map from calls and
// reorders results to match the call order by ID. This avoids the positional-
// index assumption that breaks when parallel results arrive in a different
// order. A result whose ToolCallID is not present in calls produces
// errUnknownToolCallID.
func matchToolResultsByID(calls []llm.ToolCall, results []ParallelToolResult) ([]ParallelToolResult, error) {
	if len(results) == 0 {
		return results, nil
	}

	indexByID := make(map[string]int, len(calls))
	for i, tc := range calls {
		indexByID[tc.ID] = i
	}

	matched := make([]ParallelToolResult, len(calls))
	for _, res := range results {
		idx, ok := indexByID[res.ID]
		if !ok {
			return nil, fmt.Errorf("%w: %s", errUnknownToolCallID, res.ID)
		}
		matched[idx] = res
	}
	return matched, nil
}
