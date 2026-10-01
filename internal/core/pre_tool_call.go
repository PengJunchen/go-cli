package core

import (
	"context"
	"errors"
	"sync"
)

// ToolInterceptor is a callback invoked synchronously before a tool executes.
// If it returns a non-nil error, the tool call is canceled (skipped). The
// interceptor receives the tool name, call ID, and arguments.
//
// Interceptors are registered globally via RegisterToolInterceptor or on a
// specific LoopAgent via LoopAgent.SetToolInterceptor. They are executed by
// PreToolCallEvent.IsCancelled on its first invocation — the exact point where
// loop.go checks whether to proceed with executeTool. This closes the ARCH-1
// gap: the PreToolCall event is sent asynchronously via EventStream
// (notification only), but the actual interception happens synchronously
// inside IsCancelled, before the tool runs.
type ToolInterceptor func(toolName, toolCallID string, args map[string]any) error

// ContextToolInterceptor is the context-aware variant of ToolInterceptor.
// LoopAgent-scoped callers should use this when a gate or observer must honor
// cancellation and trace context from the active run.
type ContextToolInterceptor func(ctx context.Context, toolName, toolCallID string, args map[string]any) error

// SafeToolInterceptionDenial marks an interceptor denial with a fixed public
// message that is safe to place in a tool receipt. Implementations must not
// include command text, arguments, credentials, stderr, or other runtime data.
type SafeToolInterceptionDenial interface {
	error
	SafeToolInterceptionMessage() string
}

// SafeToolInterceptionMessage returns the fixed public message for a typed
// safe denial. Arbitrary interceptor errors are intentionally not exposed.
func SafeToolInterceptionMessage(err error) (string, bool) {
	var denial SafeToolInterceptionDenial
	if !errors.As(err, &denial) || denial == nil {
		return "", false
	}
	message := denial.SafeToolInterceptionMessage()
	if message == "" {
		return "", false
	}
	return message, true
}

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
	ctx  context.Context
	// interceptors contains the LoopAgent-scoped interceptors for this event.
	interceptors []ToolInterceptor
	// contextInterceptors contains the LoopAgent-scoped context-aware interceptors.
	contextInterceptors []ContextToolInterceptor
	cancellationErr     error
	// canceled records whether the tool call was canceled by an interceptor.
	canceled bool
	// interceptorsRun ensures registered ToolInterceptors fire exactly once
	// per event. It is set to true after the first IsCancelled call (or by
	// Cancel), so subsequent calls return the cached result.
	interceptorsRun bool
	// mu protects canceled and interceptorsRun from concurrent access.
	mu sync.Mutex
}

// Cancel marks the tool call as canceled. It is safe to call from a different
// goroutine than the one that emitted the event.
func (e *PreToolCallEvent) Cancel() {
	e.mu.Lock()
	e.canceled = true
	e.mu.Unlock()
}

// CancellationError returns the first interceptor error that canceled this
// event, if any. It is available to controlled callers that can render a safe
// diagnostic; callers must not expose arbitrary interceptor error strings.
func (e *PreToolCallEvent) CancellationError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancellationErr
}

// IsCancelled returns whether the tool call was canceled.
//
// On the first call it synchronously runs all registered ToolInterceptors
// (unless the event was already canceled via Cancel). If any interceptor
// returns an error, the event is marked canceled. Subsequent calls return
// the cached result without re-running interceptors.
//
// This is the synchronous interception point: loop.go calls IsCancelled
// before executeTool, so interceptors effectively gate tool execution.
func (e *PreToolCallEvent) IsCancelled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	// If already canceled (e.g. via Cancel()), skip interceptors.
	if e.canceled {
		return true
	}
	// Run interceptors exactly once.
	if e.interceptorsRun {
		return false
	}
	e.interceptorsRun = true

	// Loop-scoped interceptors are snapshotted when the event is created, so
	// changing a LoopAgent's configuration cannot affect an event in progress.
	for _, f := range e.interceptors {
		if err := f(e.ToolName, e.ToolCallID, e.Args); err != nil {
			e.canceled = true
			e.cancellationErr = err
			break
		}
	}
	if e.canceled {
		return true
	}
	if e.ctx == nil {
		e.ctx = context.Background()
	}
	for _, f := range e.contextInterceptors {
		if err := f(e.ctx, e.ToolName, e.ToolCallID, e.Args); err != nil {
			e.canceled = true
			e.cancellationErr = err
			break
		}
	}
	if e.canceled {
		return true
	}

	// Snapshot the global interceptor list under the read lock so that
	// registration during iteration does not affect this call.
	globalInterceptorsMu.RLock()
	interceptors := make([]ToolInterceptor, len(globalInterceptors))
	copy(interceptors, globalInterceptors)
	globalInterceptorsMu.RUnlock()

	for _, f := range interceptors {
		if err := f(e.ToolName, e.ToolCallID, e.Args); err != nil {
			e.canceled = true
			e.cancellationErr = err
			break
		}
	}

	return e.canceled
}
