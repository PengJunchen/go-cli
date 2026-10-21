package approval

import "context"

// InteractiveDecision is a user's verdict for one pending tool call.
type InteractiveDecision int

const (
	// InteractiveDeny rejects a tool call.
	InteractiveDeny InteractiveDecision = iota
	// InteractiveAllow allows a tool call once.
	InteractiveAllow
	// InteractiveAlwaysAllow allows a call and remembers its pattern.
	InteractiveAlwaysAllow
)

// InteractiveRequest carries a UI-independent approval request. ResponseCh
// should be buffered so the UI can finish after a canceled waiter leaves.
type InteractiveRequest struct {
	RequestID   string
	Context     context.Context
	ToolName    string
	Args        map[string]any
	DiffPreview string
	ResponseCh  chan InteractiveResponse
}

// InteractiveResponse returns the user's verdict and optional remembered pattern.
type InteractiveResponse struct {
	Decision InteractiveDecision
	Pattern  string
}
