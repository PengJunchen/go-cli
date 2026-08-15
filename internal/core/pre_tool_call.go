package core

import "sync"

// ToolInterceptor is a callback invoked synchronously before a tool executes.
// If it returns a non-nil error, the tool call is cancelled (skipped). The
// interceptor receives the tool name, call ID, and arguments.
//
// Interceptors are registered globally via RegisterToolInterceptor or
// LoopAgent.SetToolInterceptor and are executed by PreToolCallEvent.IsCancelled
// on its first invocation — the exact point where loop.go checks whether to
// proceed with executeTool. This closes the ARCH-1 gap: the PreToolCall event
// is sent asynchronously via EventStream (notification only), but the actual
// interception happens synchronously inside IsCancelled, before the tool runs.
type ToolInterceptor func(toolName, toolCallID string, args map[string]any) error

// Global interceptor registry. Interceptors are called synchronously by
// PreToolCallEvent.IsCancelled() on its first invocation, before the tool
// executes.
var (
	globalInterceptors   []ToolInterceptor
	globalInterceptorsMu sync.RWMutex
)

// RegisterToolInterceptor appends a ToolInterceptor to the global registry.
// Interceptors run in registration order; the first one to return an error
// cancels the tool call.
func RegisterToolInterceptor(f ToolInterceptor) {
	if f == nil {
		return
	}
	globalInterceptorsMu.Lock()
	globalInterceptors = append(globalInterceptors, f)
	globalInterceptorsMu.Unlock()
}

// ClearToolInterceptors removes all registered ToolInterceptors. Tests should
// call this in cleanup to avoid leaking interceptors across test cases.
func ClearToolInterceptors() {
	globalInterceptorsMu.Lock()
	globalInterceptors = nil
	globalInterceptorsMu.Unlock()
}

// PreToolCallEvent is emitted before each tool execution, allowing external
// interceptors to cancel the tool call by calling Cancel.
type PreToolCallEvent struct {
	// ToolName is the name of the tool being called.
	ToolName string
	// ToolCallID is the unique ID of the tool call.
	ToolCallID string
	// Args holds the arguments passed to the tool.
	Args map[string]any
	// cancelled records whether the tool call was cancelled by an interceptor.
	cancelled bool
	// interceptorsRun ensures registered ToolInterceptors fire exactly once
	// per event. It is set to true after the first IsCancelled call (or by
	// Cancel), so subsequent calls return the cached result.
	interceptorsRun bool
	// mu protects cancelled and interceptorsRun from concurrent access.
	mu sync.Mutex
}

// Cancel marks the tool call as cancelled. It is safe to call from a different
// goroutine than the one that emitted the event.
func (e *PreToolCallEvent) Cancel() {
	e.mu.Lock()
	e.cancelled = true
	e.mu.Unlock()
}

// IsCancelled returns whether the tool call was cancelled.
//
// On the first call it synchronously runs all registered ToolInterceptors
// (unless the event was already cancelled via Cancel). If any interceptor
// returns an error, the event is marked cancelled. Subsequent calls return
// the cached result without re-running interceptors.
//
// This is the synchronous interception point: loop.go calls IsCancelled
// before executeTool, so interceptors effectively gate tool execution.
func (e *PreToolCallEvent) IsCancelled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	// If already cancelled (e.g. via Cancel()), skip interceptors.
	if e.cancelled {
		return true
	}
	// Run interceptors exactly once.
	if e.interceptorsRun {
		return false
	}
	e.interceptorsRun = true

	// Snapshot the interceptor list under the global read lock so that
	// registration during iteration does not affect this call.
	globalInterceptorsMu.RLock()
	interceptors := make([]ToolInterceptor, len(globalInterceptors))
	copy(interceptors, globalInterceptors)
	globalInterceptorsMu.RUnlock()

	for _, f := range interceptors {
		if err := f(e.ToolName, e.ToolCallID, e.Args); err != nil {
			e.cancelled = true
			break
		}
	}

	return e.cancelled
}
