package approval

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// AllowAllClassifier approves every tool call unconditionally.
type AllowAllClassifier struct{}

var _ ApprovalClassifier = (*AllowAllClassifier)(nil)

// Name returns the classifier identifier.
func (AllowAllClassifier) Name() string { return "allow_all" }

// Classify returns Allow for every call.
func (AllowAllClassifier) Classify(_ context.Context, _ tools.ToolCall) Classification {
	slog.Info("approval.classify.allow_all")
	return Allow
}

// DenyAllClassifier rejects every tool call unconditionally.
type DenyAllClassifier struct{}

var _ ApprovalClassifier = (*DenyAllClassifier)(nil)

// Name returns the classifier identifier.
func (DenyAllClassifier) Name() string { return "deny_all" }

// Classify returns Deny for every call.
func (DenyAllClassifier) Classify(_ context.Context, _ tools.ToolCall) Classification {
	slog.Info("approval.classify.deny_all")
	return Deny
}

// StaticClassifier decides by an allowlist and denylist of tool names. Deny
// wins over allow: a tool in the denylist is denied even if it is also listed
// in the allowlist. Tools matching neither list are denied by default, matching
// the deny-first philosophy.
type StaticClassifier struct {
	allowlist map[string]struct{}
	denylist  map[string]struct{}
}

var _ ApprovalClassifier = (*StaticClassifier)(nil)

// NewStaticClassifier builds a StaticClassifier from the given allow and deny
// tool-name lists. Duplicates are ignored.
func NewStaticClassifier(allow, deny []string) *StaticClassifier {
	return &StaticClassifier{
		allowlist: toSet(allow),
		denylist:  toSet(deny),
	}
}

// Name returns the classifier identifier.
func (c *StaticClassifier) Name() string { return "static" }

// Classify denies a call when its tool is on the denylist, allows it when on
// the allowlist, and denies it otherwise.
func (c *StaticClassifier) Classify(_ context.Context, call tools.ToolCall) Classification {
	if _, denied := c.denylist[call.Name]; denied {
		slog.Info("approval.classify.static", "tool", call.Name, "decision", "deny")
		return Deny
	}
	if _, allowed := c.allowlist[call.Name]; allowed {
		slog.Info("approval.classify.static", "tool", call.Name, "decision", "allow")
		return Allow
	}
	slog.Info("approval.classify.static", "tool", call.Name, "decision", "deny_by_default")
	return Deny
}

// SafetyPolicyClassifier is the sensible deny-first policy. It carries a
// configured set of dangerous/forbidden tool names and denies any call whose
// tool is in that set; every other tool is allowed.
type SafetyPolicyClassifier struct {
	forbidden map[string]struct{}
}

var _ ApprovalClassifier = (*SafetyPolicyClassifier)(nil)

// NewSafetyPolicyClassifier builds a SafetyPolicyClassifier that denies the
// given dangerous tool names and allows everything else.
func NewSafetyPolicyClassifier(forbidden []string) *SafetyPolicyClassifier {
	return &SafetyPolicyClassifier{forbidden: toSet(forbidden)}
}

// Name returns the classifier identifier.
func (c *SafetyPolicyClassifier) Name() string { return "safety_policy" }

// Classify denies calls targeting a forbidden tool and allows all others.
func (c *SafetyPolicyClassifier) Classify(_ context.Context, call tools.ToolCall) Classification {
	if _, bad := c.forbidden[call.Name]; bad {
		slog.Info("approval.classify.safety_policy", "tool", call.Name, "decision", "deny")
		return Deny
	}
	slog.Info("approval.classify.safety_policy", "tool", call.Name, "decision", "allow")
	return Allow
}

// toSet converts a string slice into a set for O(1) lookups.
func toSet(items []string) map[string]struct{} {
	set := make(map[string]struct{}, len(items))
	for _, item := range items {
		set[item] = struct{}{}
	}
	return set
}
