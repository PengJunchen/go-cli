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

// ErrInvalidPermissionMode indicates that the active policy contains an
// unknown mode value. Unknown modes are never interpreted as a permissive
// fallback.
var ErrInvalidPermissionMode = errors.New("approval: invalid permission mode")

// approvalOptions configures how Ask classifications are resolved.
type approvalOptions struct {
	autoApprove            bool
	permissionModeResolver PermissionModeResolver
	permissionMode         PermissionMode
	permissionModeSet      bool
	callback               ApprovalCallback
	cache                  *ApprovalCache
	dispatchFence          *tools.DispatchFence
	planModeController     core.PlanModeController
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

// WithPermissionModeResolver wires a PermissionModeResolver into the middleware
// so the effective classifier is chosen dynamically from the current mode via
// resolver.Resolve(mode).Classify(...) instead of the statically bound
// classifier. Backward compatible: when unset, the direct classifier path is
// used.
func WithPermissionModeResolver(r PermissionModeResolver) Option {
	return func(o *approvalOptions) { o.permissionModeResolver = r }
}

// WithPermissionMode sets the current PermissionMode resolved by the
// permission-mode resolver. It is only meaningful when a resolver is also
// wired. When unset, the resolver's Default mode is used.
func WithPermissionMode(mode PermissionMode) Option {
	return func(o *approvalOptions) {
		o.permissionMode = mode
		o.permissionModeSet = true
	}
}

// WithCallback wires an ApprovalCallback so that Ask classifications are
// resolved via interactive y/n/a confirmation instead of the auto-approve
// policy. When unset, Ask falls back to the autoApprove flag.
func WithCallback(cb ApprovalCallback) Option {
	return func(o *approvalOptions) { o.callback = cb }
}

// WithCache wires an ApprovalCache that persists always-allow decisions from
// the callback. When a callback is also wired, the cache is consulted before
// prompting so repeated identical calls skip the interactive confirmation.
func WithCache(cache *ApprovalCache) Option {
	return func(o *approvalOptions) { o.cache = cache }
}

// WithDispatchFence binds approval policy publication to the registry's
// prepared-dispatch fence. The same fence must be passed to the Plan Mode
// controller and the middleware registry.
func WithDispatchFence(fence *tools.DispatchFence) Option {
	return func(o *approvalOptions) { o.dispatchFence = fence }
}

// WithPlanModeController provides the controller whose state participates in
// prepared approval decisions. Prepared policy rejects a controller that does
// not expose the exact same dispatch fence.
func WithPlanModeController(controller core.PlanModeController) Option {
	return func(o *approvalOptions) { o.planModeController = controller }
}

// ApprovalMiddleware is a deny-first core.ToolMiddleware. For each call it
// consults the in-session cache, then the cross-session ApprovalStore, then the
// effective classifier. The effective classifier is either the statically bound
// one or, when a PermissionModeResolver is wired, resolver.Resolve(mode). Any
// Deny (or an Ask resolved to deny) refuses the call without invoking the
// wrapped executor. It emits an approval.decision span recording the outcome so
// telemetry is reproducible.
type ApprovalMiddleware struct {
	classifier          ApprovalClassifier
	resolver            PermissionModeResolver
	store               ApprovalStore
	opts                approvalOptions
	fence               *tools.DispatchFence
	planMode            core.PlanModeController
	preparedStore       *InMemoryApprovalStore
	preparedClassifiers map[PermissionMode]ApprovalClassifier
	initErr             error

	// session is the per-middleware cache keyed by "mode:tool_name:args_hash".
	// Values shared across calls within the same process are the "session"
	// cache. sessionMu guards both session and mode (mode may be switched at
	// runtime via SetPermissionMode).
	sessionMu   sync.RWMutex
	session     map[string]Classification
	mode        PermissionMode
	policyEpoch uint64
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
		classifier = &DenyAllClassifier{}
	}
	if store == nil {
		store = NewInMemoryApprovalStore()
	}
	mode := options.permissionMode
	if !options.permissionModeSet {
		mode = PermissionDefault
	}
	preparedClassifiers := make(map[PermissionMode]ApprovalClassifier, 4)
	if options.permissionModeResolver != nil {
		for _, candidate := range []PermissionMode{PermissionDefault, PermissionPlan, PermissionAuto, PermissionAutoFull} {
			preparedClassifiers[candidate] = options.permissionModeResolver.Resolve(candidate)
		}
	} else {
		preparedClassifiers[PermissionDefault] = classifier
	}
	middleware := &ApprovalMiddleware{
		classifier:          classifier,
		resolver:            options.permissionModeResolver,
		mode:                mode,
		store:               store,
		opts:                options,
		session:             make(map[string]Classification),
		fence:               options.dispatchFence,
		planMode:            options.planModeController,
		preparedClassifiers: preparedClassifiers,
	}
	if options.cache != nil && options.dispatchFence != nil {
		middleware.initErr = options.cache.bindFence(options.dispatchFence)
	}
	if preparedStore, ok := store.(*InMemoryApprovalStore); ok {
		middleware.preparedStore = preparedStore
		if options.dispatchFence != nil && middleware.initErr == nil {
			middleware.initErr = preparedStore.bindFence(options.dispatchFence)
		}
	}
	return middleware
}

// Name returns the middleware identifier.
func (m *ApprovalMiddleware) Name() string { return "approval" }

// sessionKey builds a stable decision key from the current permission mode,
// the tool name, and canonical (sorted-key) JSON of its arguments. Including
// the permission mode prevents an Allow cached under one mode (e.g. AutoFull)
// from being reused after a switch to a stricter mode (e.g. Default).
func sessionKey(call tools.ToolCall, mode PermissionMode) (string, error) {
	argsBytes, err := json.Marshal(call.Args)
	if err != nil {
		return "", fmt.Errorf("marshal tool args: %w", err)
	}
	sum := sha256.Sum256(argsBytes)
	return mode.String() + ":" + call.Name + ":" + hex.EncodeToString(sum[:]), nil
}

func validPermissionMode(mode PermissionMode) bool {
	switch mode {
	case PermissionDefault, PermissionPlan, PermissionAuto, PermissionAutoFull:
		return true
	default:
		return false
	}
}

// WrapToolCall returns an approval gate around the next tool executor.
func (m *ApprovalMiddleware) WrapToolCall(next func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)) func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		mode := m.currentMode()
		if !validPermissionMode(mode) {
			return nil, fmt.Errorf("%w: %d", ErrInvalidPermissionMode, mode)
		}
		key, err := sessionKey(call, mode)
		if err != nil {
			slog.Warn("approval.session_key", "tool", call.Name, "error", err)
			return nil, err
		}

		classification, cached := m.decide(ctx, key, call)

		if classification != Allow {
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

	// Classifier decides; Ask is resolved by the configured policy. When a
	// PermissionModeResolver is wired, the classifier is chosen from the mode.
	classifier := m.effectiveClassifier()
	c := classifier.Classify(ctx, call)
	cacheDecision := true
	if c == Ask {
		c, cacheDecision = m.resolveAsk(ctx, key, call)
	}

	if !cacheDecision {
		return m.record(span, call, c, false), false
	}

	m.sessionStore(key, c)
	if err := m.store.Set(ctx, key, c); err != nil {
		slog.Warn("approval.store_set", "key", key, "error", err)
		// Invalidate the session cache entry so that a subsequent call
		// re-classifies and re-attempts persistence instead of trusting a
		// value that was never durably stored.
		m.sessionDelete(key)
	}
	return m.record(span, call, c, false), false
}

// resolveAsk resolves an Ask classification and reports whether the decision
// may be reused for later calls. A callback's Allow or Deny applies only to the
// current call; AlwaysAllow and policy decisions retain their reusable scope.
// When a callback is wired, the always-allow cache is checked before prompting.
func (m *ApprovalMiddleware) resolveAsk(ctx context.Context, key string, call tools.ToolCall) (Classification, bool) {
	if m.opts.callback == nil {
		if m.opts.autoApprove {
			return Allow, true
		}
		return Deny, true
	}

	if m.opts.cache != nil {
		if allowed, ok := m.opts.cache.Get(key); ok && allowed {
			return Allow, true
		}
	}

	result, err := m.opts.callback.RequestApproval(ctx, call.Name, call.Args)
	if err != nil {
		return Deny, false
	}
	switch result {
	case ApprovalAllow:
		// Allow is a one-call decision. It must not enter the session or
		// store cache below, so an identical later call asks again.
		return Allow, false
	case ApprovalAlwaysAllow:
		if m.opts.cache != nil {
			m.opts.cache.Set(key)
		}
		return Allow, true
	default:
		return Deny, false
	}
}

// effectiveClassifier returns the classifier to consult for the current call.
// When a PermissionModeResolver is wired it resolves the current mode,
// otherwise it returns the statically bound classifier.
func (m *ApprovalMiddleware) effectiveClassifier() ApprovalClassifier {
	if m.resolver != nil {
		return m.resolver.Resolve(m.currentMode())
	}
	return m.classifier
}

// currentMode returns the permission mode used for decision telemetry. When no
// resolver is wired the mode defaults to PermissionDefault for the attribute.
// The mode is read under the session lock because SetPermissionMode may update
// it concurrently.
func (m *ApprovalMiddleware) currentMode() PermissionMode {
	if m.resolver == nil {
		return PermissionDefault
	}
	m.sessionMu.RLock()
	defer m.sessionMu.RUnlock()
	return m.mode
}

// SetPermissionMode switches the effective permission mode and clears the
// in-session cache so decisions cached under the previous mode are not reused.
// This prevents an Allow cached in AutoFull mode from being applied in Default
// mode after a switch.
func (m *ApprovalMiddleware) SetPermissionMode(mode PermissionMode) {
	update := func() {
		m.sessionMu.Lock()
		defer m.sessionMu.Unlock()
		m.mode = mode
		m.policyEpoch++
		m.session = make(map[string]Classification)
	}
	if m.fence != nil {
		if _, err := m.fence.Publish(tools.DispatchFenceApproval, nil, update); err != nil {
			slog.Error("approval.mode_publish_failed", "error", err)
			return
		}
	} else {
		update()
	}
	slog.Info("approval.mode_switch", "mode", mode.String())
}

// ClearSession empties the in-session decision cache so subsequent calls
// re-classify instead of trusting cached decisions.
func (m *ApprovalMiddleware) ClearSession() {
	update := func() {
		m.sessionMu.Lock()
		defer m.sessionMu.Unlock()
		m.policyEpoch++
		m.session = make(map[string]Classification)
	}
	if m.fence != nil {
		if _, err := m.fence.Publish(tools.DispatchFenceApproval, nil, update); err != nil {
			slog.Error("approval.clear_session_publish_failed", "error", err)
			return
		}
	} else {
		update()
	}
}

// SetPlanModeController binds the Plan state observed by prepared approval
// decisions. The controller must publish transitions through the same fence.
func (m *ApprovalMiddleware) SetPlanModeController(controller core.PlanModeController) error {
	provider, ok := controller.(interface{ DispatchFence() *tools.DispatchFence })
	if !ok || controller == nil || provider.DispatchFence() != m.fence || m.fence == nil {
		return tools.ErrDispatchFenceUnavailable
	}
	_, err := m.fence.Publish(tools.DispatchFenceApproval, nil, func() {
		m.sessionMu.Lock()
		defer m.sessionMu.Unlock()
		m.planMode = controller
		m.policyEpoch++
		m.session = make(map[string]Classification)
	})
	if err != nil {
		return fmt.Errorf("publish Plan Mode controller: %w", err)
	}
	return nil
}

// record attaches the decision attributes to the span and returns the decision.
func (m *ApprovalMiddleware) record(span tracing.TraceSpan, call tools.ToolCall, c Classification, cached bool) Classification {
	span.SetAttributes(
		tracing.Attribute{Key: "classifier", Value: m.effectiveClassifier().Name()},
		tracing.Attribute{Key: "classification", Value: classificationString(c)},
		tracing.Attribute{Key: "tool_name", Value: call.Name},
		tracing.Attribute{Key: "cached", Value: cached},
		tracing.Attribute{Key: "permission_mode", Value: m.currentMode().String()},
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

func (m *ApprovalMiddleware) sessionDelete(key string) {
	m.sessionMu.Lock()
	defer m.sessionMu.Unlock()
	delete(m.session, key)
}
