package approval

import (
	"context"
	"log/slog"
	"time"

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

// AuditClassifier wraps an ApprovalClassifier and records each classification
// decision to an AuditTrail. It is transparent: the wrapped classifier's
// decision is returned unchanged. If the AuditTrail is nil or Record fails,
// the decision is still returned — auditing must never block or alter approval.
type AuditClassifier struct {
	inner   ApprovalClassifier
	audit   *AuditTrail
	mode    PermissionMode
	session string
}

var _ ApprovalClassifier = (*AuditClassifier)(nil)

// NewAuditClassifier wraps inner with an audit-recording decorator. The mode
// and session are recorded as metadata on each audit entry.
func NewAuditClassifier(inner ApprovalClassifier, audit *AuditTrail, mode PermissionMode, session string) *AuditClassifier {
	return &AuditClassifier{inner: inner, audit: audit, mode: mode, session: session}
}

// Name returns the wrapped classifier's name.
func (c *AuditClassifier) Name() string { return c.inner.Name() }

// Classify delegates to the wrapped classifier and records the decision. Audit
// failures are logged as warnings but never propagated.
func (c *AuditClassifier) Classify(ctx context.Context, call tools.ToolCall) Classification {
	result := c.inner.Classify(ctx, call)
	if c.audit != nil {
		entry := AuditEntry{
			Timestamp:      time.Now().UTC(),
			Tool:           call.Name,
			ArgsSummary:    summarizeArgs(call.Args),
			Decision:       result.String(),
			Classifier:     c.inner.Name(),
			PermissionMode: c.mode.String(),
			SessionID:      c.session,
		}
		if err := c.audit.Record(entry); err != nil {
			slog.Warn("approval.audit.record_failed", "err", err)
		}
	}
	return result
}

// AuditResolver wraps a PermissionModeResolver so that every classifier
// returned by Resolve is itself wrapped with an AuditClassifier. This keeps
// audit recording in effect when the ApprovalMiddleware selects its classifier
// dynamically via the resolver path (e.g. TUI interactive mode), instead of the
// statically bound classifier. When the AuditTrail is nil the inner classifier
// is returned unchanged.
type AuditResolver struct {
	inner   PermissionModeResolver
	audit   *AuditTrail
	session string
}

var _ PermissionModeResolver = (*AuditResolver)(nil)

// NewAuditResolver wraps inner so each resolved classifier records audit
// entries. The session is recorded as metadata on each audit entry.
func NewAuditResolver(inner PermissionModeResolver, audit *AuditTrail, session string) *AuditResolver {
	return &AuditResolver{inner: inner, audit: audit, session: session}
}

// Name returns the wrapped resolver's identifier.
func (r *AuditResolver) Name() string { return r.inner.Name() }

// Resolve delegates to the wrapped resolver and decorates the returned
// classifier with an AuditClassifier when an AuditTrail is configured.
func (r *AuditResolver) Resolve(mode PermissionMode) ApprovalClassifier {
	inner := r.inner.Resolve(mode)
	if r.audit == nil {
		return inner
	}
	return NewAuditClassifier(inner, r.audit, mode, r.session)
}
