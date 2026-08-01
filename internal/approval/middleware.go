package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// ErrToolDenied is returned by the middleware when a tool call is refused by
// the deny-first approval policy. The wrapped tool executor is never invoked.
var ErrToolDenied = errors.New("tool call denied by approval policy")

// approvalOptions configures how Ask classifications are resolved.
type approvalOptions struct {
	autoApprove bool
}

// Option configures an ApprovalMiddleware.
type Option func(*approvalOptions)

// WithAutoApprove configures how an Ask decision is resolved. When true, an Ask
// decision auto-approves and the call runs; when false (the default), an Ask
// decision is treated as a denial and the call is refused. This makes denial the
// safe default unless an operator opts into auto-approval.
func WithAutoApprove(auto bool) Option {
	return func(o *approvalOptions) { o.autoApprove = auto }
}

// ApprovalMiddleware is a deny-first core.ToolMiddleware. For each call it
// consults the in-session cache, then the cross-session ApprovalStore, then the
// classifier. Any Deny (or an Ask resolved to deny) refuses the call without
// invoking the wrapped executor. It emits an approval.decision span recording
// the outcome so telemetry is reproducible.
type ApprovalMiddleware struct {
	classifier ApprovalClassifier
	store      ApprovalStore
	opts       approvalOptions

	// session is the per-middleware cache keyed by "tool_name:args_hash". Values
	// shared across calls within the same process are the "session" cache.
	sessionMu sync.RWMutex
	session   map[string]Classification
}

var _ core.ToolMiddleware = (*ApprovalMiddleware)(nil)

// NewApprovalMiddleware builds an ApprovalMiddleware from the given classifier
// and store, holding both by interface.
func NewApprovalMiddleware(classifier ApprovalClassifier, store ApprovalStore, opts ...Option) *ApprovalMiddleware {
	options := approvalOptions{}
	for _, opt := range opts {
		opt(&options)
	}
	if classifier == nil {
		classifier = &AllowAllClassifier{}
	}
	if store == nil {
		store = NewInMemoryApprovalStore()
	}
	return &ApprovalMiddleware{
		classifier: classifier,
		store:      store,
		opts:       options,
		session:    make(map[string]Classification),
	}
}

// Name returns the middleware identifier.
func (m *ApprovalMiddleware) Name() string { return "approval" }

// sessionKey builds a stable decision key from the tool name and canonical
// (sorted-key) JSON of its arguments. Identical arguments hash identically.
func sessionKey(call tools.ToolCall) (string, error) {
	argsBytes, err := json.Marshal(call.Args)
	if err != nil {
		return "", fmt.Errorf("marshal tool args: %w", err)
	}
	sum := sha256.Sum256(argsBytes)
	return call.Name + ":" + hex.EncodeToString(sum[:]), nil
}

// WrapToolCall returns an approval gate around the next tool executor.
func (m *ApprovalMiddleware) WrapToolCall(next func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)) func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		key, err := sessionKey(call)
		if err != nil {
			slog.Warn("approval.session_key", "tool", call.Name, "error", err)
			return nil, err
		}

		classification, cached := m.decide(ctx, key, call)

		if classification == Deny {
			slog.Info("approval.deny", "tool", call.Name, "cached", cached)
			return nil, ErrToolDenied
		}

		return next(ctx, call)
	}
}

// decide resolves the classification for a call, consulting cache and store
// before the classifier, and records an approval.decision span.
func (m *ApprovalMiddleware) decide(ctx context.Context, key string, call tools.ToolCall) (Classification, bool) {
	span, _ := tracing.SpanFromContext(ctx, "approval.decision", tracing.SpanKindInternal)
	defer span.End()

	// In-session cache first.
	if c, ok := m.sessionLoad(key); ok {
		return m.record(span, call, c, true), true
	}

	// Cross-session store next.
	if c, ok, err := m.store.Get(ctx, key); err == nil && ok {
		m.sessionStore(key, c)
		return m.record(span, call, c, true), true
	} else if err != nil {
		slog.Warn("approval.store_get", "key", key, "error", err)
	}

	// Classifier decides; Ask is resolved by the configured policy.
	c := m.classifier.Classify(ctx, call)
	if c == Ask {
		if m.opts.autoApprove {
			c = Allow
		} else {
			c = Deny
		}
	}

	m.sessionStore(key, c)
	if err := m.store.Set(ctx, key, c); err != nil {
		slog.Warn("approval.store_set", "key", key, "error", err)
	}
	return m.record(span, call, c, false), false
}

// record attaches the decision attributes to the span and returns the decision.
func (m *ApprovalMiddleware) record(span tracing.TraceSpan, call tools.ToolCall, c Classification, cached bool) Classification {
	span.SetAttributes(
		tracing.Attribute{Key: "classifier", Value: m.classifier.Name()},
		tracing.Attribute{Key: "classification", Value: classificationString(c)},
		tracing.Attribute{Key: "tool_name", Value: call.Name},
		tracing.Attribute{Key: "cached", Value: cached},
	)
	if c == Deny {
		span.SetStatus(tracing.SpanStatusError, "denied by approval policy")
	}
	return c
}

func (m *ApprovalMiddleware) sessionLoad(key string) (Classification, bool) {
	m.sessionMu.RLock()
	defer m.sessionMu.RUnlock()
	c, ok := m.session[key]
	return c, ok
}

func (m *ApprovalMiddleware) sessionStore(key string, c Classification) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	m.session[key] = c
}
