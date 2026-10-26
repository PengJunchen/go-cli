package tui

import "github.com/pengjunchen/go-cli/internal/approval"

// ApprovalDecision is the user verdict carried by the shared approval protocol.
type ApprovalDecision = approval.InteractiveDecision

const (
	// ApprovalDeny rejects the tool call.
	ApprovalDeny = approval.InteractiveDeny
	// ApprovalAllow permits one call.
	ApprovalAllow = approval.InteractiveAllow
	// ApprovalAlwaysAllow permits and persists a matching decision.
	ApprovalAlwaysAllow = approval.InteractiveAlwaysAllow
)

// ApprovalRequest carries an interactive approval independent of its renderer.
type ApprovalRequest = approval.InteractiveRequest

// ApprovalResponse carries the decision to the waiting middleware.
type ApprovalResponse = approval.InteractiveResponse
