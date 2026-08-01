// Package approval implements the deny-first tool-approval system. It owns a
// classifier interface (with four concrete policies), a decision store, and an
// ApprovalMiddleware that enforces decisions before a tool call is executed.
package approval

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// Classification enumerates the outcome of classifying a proposed tool call.
// It is distinct from core.Classification: Phase 3 defines Allow/Deny/Ask where
// Ask represents an approval prompt that the middleware resolves through its
// configured policy.
type Classification int

const (
	// Allow permits the tool call to execute.
	Allow Classification = iota
	// Deny rejects the tool call outright without executing it.
	Deny
	// Ask prompts a human (or an auto-approval policy) to decide the call.
	Ask
)

// String returns the stable lowercase name of the classification. It is used
// for span attributes and log fields so exported telemetry is reproducible.
func (c Classification) String() string {
	switch c {
	case Deny:
		return "deny"
	case Ask:
		return "ask"
	default:
		return "allow"
	}
}

// ApprovalClassifier decides whether a proposed tool call may run. It is pure:
// it returns a Classification and does not itself emit spans (the middleware
// owns the approval.decision span so a decision is recorded exactly once).
type ApprovalClassifier interface {
	// Classify returns the classification for the given tool call.
	Classify(ctx context.Context, call tools.ToolCall) Classification
	// Name returns the classifier identifier.
	Name() string
}

// classificationString maps a Classification to its loggable form.
func classificationString(c Classification) string {
	slog.Info("approval.classification.string", "classification", int(c))
	return c.String()
}
