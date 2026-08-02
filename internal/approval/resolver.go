package approval

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// PermissionModeResolver maps a PermissionMode to the ApprovalClassifier that
// enforces it. The ApprovalMiddleware consults it to switch policy dynamically.
type PermissionModeResolver interface {
	// Resolve returns the classifier that implements the given mode.
	Resolve(mode PermissionMode) ApprovalClassifier
	// Name returns the resolver identifier.
	Name() string
}

// DefaultPermissionModeResolver is the default resolver. It maps the four
// PermissionModes onto the concrete classifiers already present in the
// approval package.
type DefaultPermissionModeResolver struct{}

var _ PermissionModeResolver = (*DefaultPermissionModeResolver)(nil)

// NewDefaultPermissionModeResolver builds a DefaultPermissionModeResolver.
func NewDefaultPermissionModeResolver() PermissionModeResolver {
	return &DefaultPermissionModeResolver{}
}

// Name returns the resolver identifier.
func (r *DefaultPermissionModeResolver) Name() string { return "permission_mode" }

// Resolve maps the mode to its enforcing classifier.
func (r *DefaultPermissionModeResolver) Resolve(mode PermissionMode) ApprovalClassifier {
	switch mode {
	case PermissionPlan:
		return NewPlanClassifier()
	case PermissionAuto:
		return NewAutoClassifier(nil, nil)
	case PermissionAutoFull:
		return &AllowAllClassifier{}
	default:
		return NewSafetyPolicyClassifier(nil)
	}
}

// PlanClassifier is the plan-mode policy. Every tool call is held for plan
// confirmation: it returns Ask so the middleware resolves it against its
// auto-approve flag (Ask with auto-approve runs the call, otherwise it is
// refused). This expresses "needs confirmation before execution".
type PlanClassifier struct{}

var _ ApprovalClassifier = (*PlanClassifier)(nil)

// NewPlanClassifier builds a PlanClassifier that asks for every call.
func NewPlanClassifier() *PlanClassifier {
	return &PlanClassifier{}
}

// Name returns the classifier identifier.
func (PlanClassifier) Name() string { return "plan-classifier" }

// Classify returns Ask for every call, holding it for confirmation.
func (PlanClassifier) Classify(_ context.Context, _ tools.ToolCall) Classification {
	slog.Info("approval.classify.plan")
	return Ask
}

// AutoClassifier is the auto-mode policy. Safe tools are auto-allowed while
// dangerous tools require confirmation (Ask). Any tool not classified safe or
// dangerous is allowed by default (deny-first only for the dangerous set).
type AutoClassifier struct {
	safe      map[string]struct{}
	dangerous map[string]struct{}
}

var _ ApprovalClassifier = (*AutoClassifier)(nil)

// NewAutoClassifier builds an AutoClassifier from the given safe and dangerous
// tool-name lists. Byte-identical lists are stored by value so each produced
// classifier is independent. A nil list means "empty".
func NewAutoClassifier(safe, dangerous []string) *AutoClassifier {
	return &AutoClassifier{
		safe:      toSet(safe),
		dangerous: toSet(dangerous),
	}
}

// Name returns the classifier identifier.
func (c *AutoClassifier) Name() string { return "auto-classifier" }

// Classify auto-allows safe tools, asks for dangerous tools and allows every
// other tool.
func (c *AutoClassifier) Classify(_ context.Context, call tools.ToolCall) Classification {
	if _, bad := c.dangerous[call.Name]; bad {
		slog.Info("approval.classify.auto", "tool", call.Name, "decision", "ask")
		return Ask
	}
	if _, ok := c.safe[call.Name]; ok {
		slog.Info("approval.classify.auto", "tool", call.Name, "decision", "allow")
		return Allow
	}
	slog.Info("approval.classify.auto", "tool", call.Name, "decision", "allow_default")
	return Allow
}
