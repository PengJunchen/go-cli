package approval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

var (
	ErrPreparedPolicyUnavailable = errors.New("approval: prepared policy is not bound to runtime state")
	ErrPreparedIdentityInvalid   = errors.New("approval: prepared tool identity is invalid")
)

type planFenceProvider interface {
	DispatchFence() *tools.DispatchFence
}

type preparedDecisionKeyInput struct {
	RegistrationSource     string                    `json:"registration_source"`
	ToolName               string                    `json:"tool_name"`
	RegistrationGeneration uint64                    `json:"registration_generation"`
	ExecutorFingerprint    string                    `json:"executor_fingerprint"`
	ScopeFingerprint       string                    `json:"scope_fingerprint"`
	Effects                []tools.TrustedToolEffect `json:"effects"`
	SchemaDigest           string                    `json:"schema_digest"`
	ArgumentDigest         string                    `json:"argument_digest"`
	RegistryGeneration     uint64                    `json:"registry_generation"`
	ApprovalGeneration     uint64                    `json:"approval_generation"`
	PlanGeneration         uint64                    `json:"plan_generation"`
	PermissionMode         PermissionMode            `json:"permission_mode"`
	PolicyEpoch            uint64                    `json:"policy_epoch"`
	CacheEpoch             uint64                    `json:"cache_epoch"`
	StoreEpoch             uint64                    `json:"store_epoch"`
}

// PreparedPolicy applies approval and Plan Mode rules to the exact frozen
// registration and normalized call supplied by the controlled tool registry.
// It never re-resolves a tool by name and never holds the dispatch fence while
// waiting for a callback.
func (m *ApprovalMiddleware) PreparedPolicy(ctx context.Context, req tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
	decision := tools.PreparedPolicyDecision{Generations: req.Generations}
	span, _ := tracing.SpanFromContext(ctx, "approval.prepared_decision", tracing.SpanKindInternal)
	defer span.End()

	fence := m.fence
	if m.initErr != nil {
		return decision, fmt.Errorf("%w: %w", ErrPreparedPolicyUnavailable, m.initErr)
	}
	if fence == nil || req.Fence == nil || req.Fence != fence {
		return decision, ErrPreparedPolicyUnavailable
	}
	if fence.Snapshot() != req.Generations {
		return decision, tools.ErrDispatchFenceChanged
	}
	if !validPreparedIdentity(req.Identity, req.Call.ToolCall().Name) {
		return decision, ErrPreparedIdentityInvalid
	}

	mode, policyEpoch, controller := m.preparedState()
	if !validPermissionMode(mode) {
		return decision, fmt.Errorf("%w: %d", ErrInvalidPermissionMode, mode)
	}
	if controller == nil {
		return decision, ErrPreparedPolicyUnavailable
	}
	provider, ok := controller.(planFenceProvider)
	if !ok || provider.DispatchFence() != fence {
		return decision, ErrPreparedPolicyUnavailable
	}
	planActive := controller.IsActive()
	if fence.Snapshot() != req.Generations {
		return decision, tools.ErrDispatchFenceChanged
	}

	call := req.Call.ToolCall()
	classifier := m.preparedClassifiers[mode]
	auditClassifier, _ := classifier.(*AuditClassifier)
	if planActive {
		allowed := preparedPlanCallAllowed(req, call.Name)
		recordPreparedDecision(span, req, mode, "plan", false, true, allowed)
		if !allowed {
			return decision, ErrToolDenied
		}
		decision.Allowed = true
		decision.TerminalObserver = preparedTerminalAuditObserver(auditClassifier, "", "", "plan_mode")
		return decision, nil
	}

	key, err := preparedDecisionKey(req, mode, policyEpoch, m.opts.cache, m.preparedStore)
	if err != nil {
		return decision, err
	}
	if cached, ok := m.sessionLoad(key); ok {
		if fence.Snapshot() != req.Generations {
			return decision, tools.ErrDispatchFenceChanged
		}
		if cached != Allow {
			recordPreparedDecision(span, req, mode, "cached", true, false, false)
			return decision, ErrToolDenied
		}
		recordPreparedDecision(span, req, mode, "cached", true, false, true)
		decision.Allowed = true
		decision.TerminalObserver = preparedTerminalAuditObserver(auditClassifier, "", "", "session_cache")
		return decision, nil
	}
	if m.opts.cache != nil {
		if allowed, ok := m.opts.cache.Get(key); ok && allowed {
			if fence.Snapshot() != req.Generations {
				return decision, tools.ErrDispatchFenceChanged
			}
			recordPreparedDecision(span, req, mode, "always_allow_cache", true, false, true)
			decision.Allowed = true
			decision.TerminalObserver = preparedTerminalAuditObserver(auditClassifier, "", "", "always_allow_cache")
			return decision, nil
		}
	}
	if m.preparedStore != nil {
		cached, found, getErr := m.preparedStore.Get(ctx, key)
		if getErr != nil {
			slog.Warn("approval.prepared_store_get", "error", getErr)
		} else if found {
			if fence.Snapshot() != req.Generations {
				return decision, tools.ErrDispatchFenceChanged
			}
			if cached != Allow {
				recordPreparedDecision(span, req, mode, "cached_deny", true, false, false)
				return decision, ErrToolDenied
			}
			recordPreparedDecision(span, req, mode, "approval_store", true, false, true)
			decision.Allowed = true
			decision.TerminalObserver = preparedTerminalAuditObserver(auditClassifier, "", "", "approval_store")
			return decision, nil
		}
	}

	if classifier == nil {
		recordPreparedDecision(span, req, mode, "missing_classifier", false, false, false)
		return decision, ErrToolDenied
	}
	var classification Classification
	if audited, ok := classifier.(*AuditClassifier); ok {
		var err error
		classification, err = audited.classifyAndAudit(ctx, call)
		if err != nil {
			return decision, err
		}
	} else {
		classification = classifier.Classify(ctx, call)
	}
	proposal := classification.String()
	if classification == Allow && !preparedClassifierAllowTrusted(req, mode) {
		// Classifier name lists are compatibility policy inputs, not trusted
		// registration facts. An unreviewed tool cannot receive a built-in
		// read exemption by registering under a familiar name.
		classification = Ask
	}
	switch classification {
	case Allow:
		if err := recordPreparedResolution(auditClassifier, call, proposal, "", "classifier"); err != nil {
			return decision, err
		}
		return m.publishPreparedAllow(ctx, span, req, mode, key, false, auditClassifier, proposal, "", "classifier")
	case Deny:
		if err := recordPreparedResolution(auditClassifier, call, proposal, "", "classifier"); err != nil {
			return decision, errors.Join(ErrToolDenied, err)
		}
		recordPreparedDecision(span, req, mode, classification.String(), false, false, false)
		return decision, ErrToolDenied
	case Ask:
		return m.resolvePreparedAsk(ctx, span, req, mode, key, auditClassifier, proposal)
	default:
		if err := recordPreparedResolution(auditClassifier, call, proposal, "", "invalid_classification"); err != nil {
			return decision, errors.Join(ErrToolDenied, err)
		}
		recordPreparedDecision(span, req, mode, "invalid_classification", false, false, false)
		return decision, ErrToolDenied
	}
}

func (m *ApprovalMiddleware) resolvePreparedAsk(
	ctx context.Context,
	span tracing.TraceSpan,
	req tools.PreparedPolicyRequest,
	mode PermissionMode,
	key string,
	auditClassifier *AuditClassifier,
	proposal string,
) (tools.PreparedPolicyDecision, error) {
	decision := tools.PreparedPolicyDecision{Generations: req.Generations}
	call := req.Call.ToolCall()
	if m.opts.callback == nil {
		if m.opts.autoApprove {
			if err := recordPreparedResolution(auditClassifier, call, proposal, "", "auto_approve"); err != nil {
				return decision, err
			}
			if m.fence.Snapshot() != req.Generations {
				return decision, tools.ErrDispatchFenceChanged
			}
			recordPreparedDecision(span, req, mode, "auto_approve", false, false, true)
			decision.Allowed = true
			decision.TerminalObserver = preparedTerminalAuditObserver(auditClassifier, proposal, "", "auto_approve")
			return decision, nil
		}
		if err := recordPreparedResolution(auditClassifier, call, proposal, "", "policy_deny"); err != nil {
			return decision, errors.Join(ErrToolDenied, err)
		}
		recordPreparedDecision(span, req, mode, "ask_denied", false, false, false)
		return decision, ErrToolDenied
	}

	result, choice, resolution, err := m.requestPreparedApproval(ctx, call)
	if err != nil {
		auditErr := recordPreparedResolution(auditClassifier, call, proposal, "", "callback_error")
		return decision, errors.Join(err, auditErr)
	}
	switch result {
	case ApprovalAllow:
		if err := recordPreparedResolution(auditClassifier, call, proposal, choice, resolution); err != nil {
			return decision, err
		}
		if m.fence.Snapshot() != req.Generations {
			return decision, tools.ErrDispatchFenceChanged
		}
		recordPreparedDecision(span, req, mode, "allow_once", false, false, true)
		decision.Allowed = true
		decision.TerminalObserver = preparedTerminalAuditObserver(auditClassifier, proposal, choice, resolution)
		return decision, nil
	case ApprovalAlwaysAllow:
		if err := recordPreparedResolution(auditClassifier, call, proposal, choice, resolution); err != nil {
			return decision, err
		}
		if m.fence.Snapshot() != req.Generations {
			return decision, tools.ErrDispatchFenceChanged
		}
		return m.publishPreparedAllow(ctx, span, req, mode, key, true, auditClassifier, proposal, choice, resolution)
	case ApprovalDeny:
		auditErr := recordPreparedResolution(auditClassifier, call, proposal, choice, resolution)
		if m.fence.Snapshot() != req.Generations {
			return decision, errors.Join(tools.ErrDispatchFenceChanged, auditErr)
		}
		if err := m.publishPreparedRevocation(req.Generations); err != nil {
			return decision, errors.Join(fmt.Errorf("publish explicit approval denial: %w", err), auditErr)
		}
		if auditErr != nil {
			return decision, errors.Join(ErrToolDenied, auditErr)
		}
		recordPreparedDecision(span, req, mode, "callback_deny", false, false, false)
		return decision, ErrToolDenied
	default:
		if err := recordPreparedResolution(auditClassifier, call, proposal, "", "invalid_response"); err != nil {
			return decision, errors.Join(ErrToolDenied, err)
		}
		recordPreparedDecision(span, req, mode, "invalid_approval_result", false, false, false)
		return decision, ErrToolDenied
	}
}

type approvalChoiceProvider interface {
	requestApprovalWithChoice(context.Context, string, map[string]any) (ApprovalResult, string, string, error)
}

func (m *ApprovalMiddleware) requestPreparedApproval(ctx context.Context, call tools.ToolCall) (ApprovalResult, string, string, error) {
	if provider, ok := m.opts.callback.(approvalChoiceProvider); ok {
		return provider.requestApprovalWithChoice(ctx, call.Name, call.Args)
	}
	result, err := m.opts.callback.RequestApproval(ctx, call.Name, call.Args)
	choice := ""
	resolution := "callback"
	switch result {
	case ApprovalAllow:
		choice = "allow_once"
	case ApprovalAlwaysAllow:
		choice = "allow_always"
	case ApprovalDeny:
		choice = "deny"
	default:
		resolution = "invalid_response"
	}
	return result, choice, resolution, err
}

func recordPreparedResolution(auditClassifier *AuditClassifier, call tools.ToolCall, proposal, choice, resolution string) error {
	if auditClassifier == nil {
		return nil
	}
	if err := auditClassifier.recordResolution(call, proposal, choice, resolution); err != nil {
		return fmt.Errorf("approval audit resolution: %w", err)
	}
	return nil
}

func preparedTerminalAuditObserver(
	auditClassifier *AuditClassifier,
	proposal, choice, resolution string,
) tools.PreparedTerminalObserver {
	if auditClassifier == nil || auditClassifier.audit == nil {
		return nil
	}
	return func(outcome tools.PreparedTerminalOutcome) error {
		return auditClassifier.recordToolOutcome(outcome, proposal, choice, resolution)
	}
}

// AuditHealth exposes the current prepared classifier's redacted sticky audit
// health so observer failures remain readable after Dispatch preserves its
// caller-visible result.
func (m *ApprovalMiddleware) AuditHealth() AuditTrailHealth {
	if m == nil {
		return AuditTrailHealth{Healthy: true}
	}
	mode, _, _ := m.preparedState()
	if audited, ok := m.preparedClassifiers[mode].(*AuditClassifier); ok {
		return audited.AuditHealth()
	}
	if audited, ok := m.classifier.(*AuditClassifier); ok {
		return audited.AuditHealth()
	}
	return AuditTrailHealth{Healthy: true}
}

func (m *ApprovalMiddleware) publishPreparedAllow(
	ctx context.Context,
	span tracing.TraceSpan,
	req tools.PreparedPolicyRequest,
	mode PermissionMode,
	key string,
	cacheGrant bool,
	auditClassifier *AuditClassifier,
	proposal, choice, resolution string,
) (tools.PreparedPolicyDecision, error) {
	decision := tools.PreparedPolicyDecision{Generations: req.Generations}
	cacheUpdate := func() { m.sessionStore(key, Allow) }
	var (
		published tools.DispatchFenceSnapshot
		err       error
	)
	if cacheGrant && m.opts.cache != nil {
		published, err = m.opts.cache.publishPreparedGrant(m.fence, req.Generations, key, cacheUpdate)
	} else {
		published, err = publishApprovalGrant(m.fence, req.Generations, cacheUpdate)
	}
	if err != nil {
		return decision, fmt.Errorf("publish prepared approval grant: %w", err)
	}
	if published != req.Generations {
		return decision, tools.ErrDispatchFenceChanged
	}
	if m.preparedStore != nil {
		if err := m.preparedStore.setPreparedGrant(req.Generations, key); err != nil {
			return decision, fmt.Errorf("publish prepared approval-store grant: %w", err)
		}
	}
	if m.fence.Snapshot() != req.Generations {
		return decision, tools.ErrDispatchFenceChanged
	}
	decision.Allowed = true
	decision.TerminalObserver = preparedTerminalAuditObserver(auditClassifier, proposal, choice, resolution)
	recordPreparedDecision(span, req, mode, map[bool]string{true: "always_allow", false: "classifier_allow"}[cacheGrant], false, false, true)
	return decision, nil
}

func (m *ApprovalMiddleware) preparedState() (PermissionMode, uint64, core.PlanModeController) {
	m.sessionMu.RLock()
	defer m.sessionMu.RUnlock()
	mode := m.mode
	// Without a resolver, valid modes retain the legacy direct-classifier
	// behavior under the default mode. Invalid explicitly stored values must
	// remain visible so PreparedPolicy can fail closed instead of masking them.
	if m.resolver == nil && validPermissionMode(mode) {
		mode = PermissionDefault
	}
	return mode, m.policyEpoch, m.planMode
}

func validPreparedIdentity(identity tools.TrustedToolIdentity, callName string) bool {
	snapshot := identity.Snapshot()
	if snapshot.RegistrationSource == "" || snapshot.ToolName == "" || snapshot.ToolName != callName || snapshot.RegistrationGeneration == 0 {
		return false
	}
	return snapshot.ExecutorFingerprint != hex.EncodeToString(make([]byte, 32))
}

func preparedDecisionKey(
	req tools.PreparedPolicyRequest,
	mode PermissionMode,
	policyEpoch uint64,
	cache *ApprovalCache,
	store *InMemoryApprovalStore,
) (string, error) {
	identity := req.Identity.Snapshot()
	effects := req.Effects.Values()
	schemaDigest := req.Schema.Digest()
	argumentDigest := req.Call.Digest()
	keyInput := preparedDecisionKeyInput{
		RegistrationSource:     identity.RegistrationSource,
		ToolName:               identity.ToolName,
		RegistrationGeneration: identity.RegistrationGeneration,
		ExecutorFingerprint:    identity.ExecutorFingerprint,
		ScopeFingerprint:       identity.ScopeFingerprint,
		Effects:                effects,
		SchemaDigest:           hex.EncodeToString(schemaDigest[:]),
		ArgumentDigest:         hex.EncodeToString(argumentDigest[:]),
		RegistryGeneration:     req.Generations.Registry,
		ApprovalGeneration:     req.Generations.Approval,
		PlanGeneration:         req.Generations.Plan,
		PermissionMode:         mode,
		PolicyEpoch:            policyEpoch,
	}
	if cache != nil {
		keyInput.CacheEpoch = cache.policyEpoch()
	}
	if store != nil {
		keyInput.StoreEpoch = store.policyEpochValue()
	}
	encoded, err := json.Marshal(keyInput)
	if err != nil {
		return "", fmt.Errorf("marshal prepared approval key: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return "prepared:" + hex.EncodeToString(sum[:]), nil
}

func preparedPlanCallAllowed(req tools.PreparedPolicyRequest, name string) bool {
	if !validPreparedIdentity(req.Identity, name) {
		return false
	}
	effects := req.Effects.Values()
	if len(effects) != 1 {
		return false
	}
	switch effects[0] {
	case tools.EffectWorkspaceRead:
		identity := req.Identity.Snapshot()
		return identity.RegistrationSource == tools.RegistrationSourceBuiltin &&
			req.Identity.Scope().Fingerprint() != ([32]byte{}) &&
			identity.ToolName == name
	case tools.EffectNetworkRead:
		return preparedTrustedNetworkRead(req, name)
	case tools.EffectControl:
		if req.Identity.RegistrationSource() != tools.RegistrationSourceTrustedControl {
			return false
		}
		_, allowed := planModeTrustedControlNames[name]
		return allowed && req.Identity.ToolName() == name
	default:
		return false
	}
}

func preparedClassifierAllowTrusted(req tools.PreparedPolicyRequest, mode PermissionMode) bool {
	if mode == PermissionAutoFull {
		return true
	}
	identity := req.Identity.Snapshot()
	effects := req.Effects.Values()
	if len(effects) != 1 {
		return false
	}
	switch effects[0] {
	case tools.EffectWorkspaceRead:
		return identity.RegistrationSource == tools.RegistrationSourceBuiltin && req.Identity.Scope().Fingerprint() != ([32]byte{})
	case tools.EffectNetworkRead:
		return preparedTrustedNetworkRead(req, identity.ToolName)
	case tools.EffectControl:
		if identity.RegistrationSource != tools.RegistrationSourceTrustedControl {
			return false
		}
		_, allowed := planModeTrustedControlNames[identity.ToolName]
		return allowed
	default:
		return false
	}
}

func preparedTrustedNetworkRead(req tools.PreparedPolicyRequest, name string) bool {
	identity := req.Identity.Snapshot()
	if identity.RegistrationSource != tools.RegistrationSourceTrustedNetworkRead || identity.ToolName != name {
		return false
	}
	effects := req.Effects.Values()
	if len(effects) != 1 || effects[0] != tools.EffectNetworkRead {
		return false
	}
	switch name {
	case "web_fetch", "web_search":
		return true
	default:
		return false
	}
}

func (m *ApprovalMiddleware) publishPreparedRevocation(expected tools.DispatchFenceSnapshot) error {
	if m.fence == nil {
		return ErrPreparedPolicyUnavailable
	}
	_, err := m.fence.Publish(tools.DispatchFenceApproval, &expected, func() {
		m.sessionMu.Lock()
		defer m.sessionMu.Unlock()
		m.policyEpoch++
		m.session = make(map[string]Classification)
	})
	return err
}

var planModeTrustedControlNames = map[string]struct{}{
	"ask_user": {}, "tool_search": {}, "task_get": {}, "task_list": {},
	"goal_get": {}, "goal_list": {}, "enter_plan_mode": {}, "exit_plan_mode": {},
}

func recordPreparedDecision(span tracing.TraceSpan, req tools.PreparedPolicyRequest, mode PermissionMode, outcome string, cached, plan, allowed bool) {
	span.SetAttributes(
		tracing.Attribute{Key: "registration_source", Value: req.Identity.RegistrationSource()},
		tracing.Attribute{Key: "tool_name", Value: req.Identity.ToolName()},
		tracing.Attribute{Key: "permission_mode", Value: mode.String()},
		tracing.Attribute{Key: "plan_mode_active", Value: plan},
		tracing.Attribute{Key: "cached", Value: cached},
		tracing.Attribute{Key: "allowed", Value: allowed},
		tracing.Attribute{Key: "outcome", Value: outcome},
	)
	if !allowed {
		span.SetStatus(tracing.SpanStatusError, "denied by prepared approval policy")
	}
	if allowed {
		slog.Debug("approval.prepared.allowed", "tool", req.Identity.ToolName(), "mode", mode.String(), "plan_mode_active", plan, "cached", cached)
	} else {
		slog.Info("approval.prepared.denied", "tool", req.Identity.ToolName(), "mode", mode.String(), "plan_mode_active", plan, "outcome", outcome)
	}
}
