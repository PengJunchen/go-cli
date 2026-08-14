package core

import "sync"

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
	// mu protects cancelled from concurrent access.
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
func (e *PreToolCallEvent) IsCancelled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelled
}
