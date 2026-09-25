package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
)

var (
	// ErrDispatchFenceChanged indicates that prepared state no longer matches
	// the registry, approval, and Plan generations captured for the call.
	ErrDispatchFenceChanged = errors.New("tools: prepared dispatch state changed")
	// ErrDispatchFenceUnavailable indicates that a prepared dispatch has no
	// shared generation fence.
	ErrDispatchFenceUnavailable = errors.New("tools: dispatch fence is unavailable")
	// ErrInvalidDispatchFenceDomain indicates an unsupported generation domain.
	ErrInvalidDispatchFenceDomain = errors.New("tools: invalid dispatch fence domain")
	// ErrPreparedInvocationConsumed indicates that a prepared call has already
	// been dispatched or scheduled.
	ErrPreparedInvocationConsumed = errors.New("tools: prepared invocation already consumed")
	// ErrPreparedInvocationUnavailable indicates that the captured executor is
	// not available for this invocation.
	ErrPreparedInvocationUnavailable = errors.New("tools: prepared executor is unavailable")
	// ErrPreparedValueNotJSON indicates that a call or schema contains a value
	// outside the supported JSON protocol tree.
	ErrPreparedValueNotJSON = errors.New("tools: prepared value is not JSON-compatible")
	// ErrPreparedToolPolicyUnavailable indicates that a controlled registry has
	// no complete prepared policy and execution-stage configuration.
	ErrPreparedToolPolicyUnavailable = errors.New("tools: prepared tool policy is unavailable")
	// ErrPreparedToolPolicyDenied indicates that the prepared policy rejected
	// the normalized call.
	ErrPreparedToolPolicyDenied = errors.New("tools: prepared tool policy denied the call")
	// ErrPreparedCallMutation indicates that an execution decorator attempted
	// to change the normalized call after policy approval.
	ErrPreparedCallMutation = errors.New("tools: execution decorator changed the frozen tool call")
	// ErrPreparedProxyNameMismatch indicates that a protected definition proxy
	// was asked to execute a different tool than the one it represents.
	ErrPreparedProxyNameMismatch = errors.New("tools: protected tool proxy name does not match the call")
	// ErrPreparedProxyRegistrationStale indicates that the registry changed
	// after the protected proxy captured its executor snapshot.
	ErrPreparedProxyRegistrationStale = errors.New("tools: protected tool proxy registration is stale")
	// ErrPreparedSchemaInvalid indicates that a registered parameter schema is
	// malformed or uses syntax the prepared validator cannot safely enforce.
	ErrPreparedSchemaInvalid = errors.New("tools: prepared parameter schema is invalid or unsupported")
	// ErrPreparedMutationQueueUnavailable indicates that a mutation queue does
	// not support captured prepared invocations.
	ErrPreparedMutationQueueUnavailable = errors.New("tools: prepared mutation queue is unavailable")
	// ErrPreparedNormalizerUnsupported indicates that a configured preparer is
	// not a reviewed pure normalizer and cannot run before the policy callback.
	ErrPreparedNormalizerUnsupported = errors.New("tools: prepared argument normalizer is unsupported")
	// ErrPreparedOpaqueWrapperUnsupported indicates that a legacy wrapper has
	// not been assigned to a reviewed prepared stage and cannot be bypassed.
	ErrPreparedOpaqueWrapperUnsupported = errors.New("tools: opaque legacy tool wrapper is unsupported for prepared dispatch")
)

// DispatchFenceDomain identifies a shared mutable state source that can
// invalidate a prepared tool decision.
type DispatchFenceDomain uint8

const (
	// DispatchFenceRegistry tracks tool registration replacements.
	DispatchFenceRegistry DispatchFenceDomain = iota + 1
	// DispatchFenceApproval tracks approval mode and reusable approval changes.
	DispatchFenceApproval
	// DispatchFencePlan tracks Plan Mode transitions.
	DispatchFencePlan
)

// DispatchFenceSnapshot is the generation tuple captured by one prepared
// decision. All domains are compared together before a write or dispatch.
type DispatchFenceSnapshot struct {
	Registry uint64
	Approval uint64
	Plan     uint64
}

// DispatchFence serializes the short state publications and final dispatch
// checks shared by tool registration, approval, and Plan Mode. Never hold its
// lock while waiting for user input, syncing a journal, queueing work, or
// invoking a tool.
type DispatchFence struct {
	mu          sync.RWMutex
	generations DispatchFenceSnapshot
}

// NewDispatchFence returns a shared generation fence for one tool runtime.
func NewDispatchFence() *DispatchFence { return &DispatchFence{} }

// Snapshot returns the current registry, approval, and Plan generations.
func (f *DispatchFence) Snapshot() DispatchFenceSnapshot {
	if f == nil {
		return DispatchFenceSnapshot{}
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.generations
}

// read runs a short, read-only callback while holding the fence's read lock.
// Registry implementations use it to capture definition metadata and the
// matching generation tuple without a check-then-read gap.
func (f *DispatchFence) read(callback func(DispatchFenceSnapshot)) error {
	if f == nil {
		return ErrDispatchFenceUnavailable
	}
	if callback == nil {
		return errors.New("tools: dispatch fence reader is nil")
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	callback(f.generations)
	return nil
}

// Publish applies a short state update under the fence and advances its
// generation domain. When expected is non-nil, the complete three-domain
// snapshot must still match; this prevents an approval-cache update from
// overlooking an intervening registry or Plan change. The callback must only
// publish in-memory state and must not block or perform I/O.
func (f *DispatchFence) Publish(domain DispatchFenceDomain, expected *DispatchFenceSnapshot, update func()) (DispatchFenceSnapshot, error) {
	if f == nil {
		return DispatchFenceSnapshot{}, ErrDispatchFenceUnavailable
	}
	if update == nil {
		return DispatchFenceSnapshot{}, errors.New("tools: dispatch fence update is nil")
	}
	if domain != DispatchFenceRegistry && domain != DispatchFenceApproval && domain != DispatchFencePlan {
		return DispatchFenceSnapshot{}, ErrInvalidDispatchFenceDomain
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if expected != nil && f.generations != *expected {
		return f.generations, ErrDispatchFenceChanged
	}

	update()
	switch domain {
	case DispatchFenceRegistry:
		f.generations.Registry++
	case DispatchFenceApproval:
		f.generations.Approval++
	case DispatchFencePlan:
		f.generations.Plan++
	}
	return f.generations, nil
}

// PublishApprovalGrant publishes a monotonic positive approval-cache grant
// without invalidating other permits prepared from the same policy snapshot.
// The complete generation tuple must still match expected. Revocation,
// replacement, mode changes, and other decisions that can remove authority
// must use Publish(DispatchFenceApproval, ...) so existing permits become
// stale.
func (f *DispatchFence) PublishApprovalGrant(expected DispatchFenceSnapshot, update func()) (DispatchFenceSnapshot, error) {
	if f == nil {
		return DispatchFenceSnapshot{}, ErrDispatchFenceUnavailable
	}
	if update == nil {
		return DispatchFenceSnapshot{}, errors.New("tools: approval grant update is nil")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.generations != expected {
		return f.generations, ErrDispatchFenceChanged
	}
	update()
	return f.generations, nil
}

// commit performs the final full-snapshot comparison and invokes mark while
// holding the fence. mark is only for consuming the private one-use permit and
// recording dispatch linearization; it must not invoke tools, wait, enqueue,
// sync, or perform other I/O.
func (f *DispatchFence) commit(expected DispatchFenceSnapshot, mark func()) error {
	if f == nil {
		return ErrDispatchFenceUnavailable
	}
	if mark == nil {
		return errors.New("tools: dispatch commit marker is nil")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.generations != expected {
		return ErrDispatchFenceChanged
	}
	mark()
	return nil
}

// TrustedToolEffect describes a reviewed effect boundary. A registration
// boundary in this package mints effect sets; tool arguments and extension
// metadata cannot construct or change a TrustedEffectSet.
type TrustedToolEffect string

const (
	// EffectWorkspaceRead describes a bounded read of workspace data.
	EffectWorkspaceRead TrustedToolEffect = "workspace_read"
	// EffectWorkspaceWrite describes a workspace mutation.
	EffectWorkspaceWrite TrustedToolEffect = "workspace_write"
	// EffectGitMutation describes a Git state mutation.
	EffectGitMutation TrustedToolEffect = "git_mutation"
	// EffectNetworkRead describes an external network read.
	EffectNetworkRead TrustedToolEffect = "network_read"
	// EffectNetworkWrite describes an external network mutation.
	EffectNetworkWrite TrustedToolEffect = "network_write"
	// EffectExternal describes an external side effect.
	EffectExternal TrustedToolEffect = "external_effect"
	// EffectControl describes an explicitly registered control-plane action.
	EffectControl TrustedToolEffect = "control"
	// EffectUnknown describes an unreviewed or composite execution path.
	EffectUnknown TrustedToolEffect = "unknown"
)

// TrustedEffectSet is an immutable set minted from registration provenance.
// Its zero value is treated as unknown.
type TrustedEffectSet struct {
	values []TrustedToolEffect
}

// Values returns a defensive copy of the trusted effects.
func (s TrustedEffectSet) Values() []TrustedToolEffect {
	if len(s.values) == 0 {
		return []TrustedToolEffect{EffectUnknown}
	}
	return append([]TrustedToolEffect(nil), s.values...)
}

// Contains reports whether the set contains effect.
func (s TrustedEffectSet) Contains(effect TrustedToolEffect) bool {
	if len(s.values) == 0 {
		return effect == EffectUnknown
	}
	for _, value := range s.values {
		if value == effect {
			return true
		}
	}
	return false
}

func unknownTrustedEffects() TrustedEffectSet {
	return TrustedEffectSet{values: []TrustedToolEffect{EffectUnknown}}
}

func newTrustedEffectSet(effects ...TrustedToolEffect) TrustedEffectSet {
	if len(effects) == 0 {
		return unknownTrustedEffects()
	}
	seen := make(map[TrustedToolEffect]struct{}, len(effects))
	values := make([]TrustedToolEffect, 0, len(effects))
	for _, effect := range effects {
		if effect == "" {
			effect = EffectUnknown
		}
		if _, ok := seen[effect]; ok {
			continue
		}
		seen[effect] = struct{}{}
		values = append(values, effect)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return TrustedEffectSet{values: values}
}

// TrustedToolScope is a non-secret identity for the workspace or endpoint
// scope captured at registration. Its fingerprint is opaque to consumers.
type TrustedToolScope struct {
	fingerprint [32]byte
}

// Fingerprint returns the opaque scope fingerprint.
func (s TrustedToolScope) Fingerprint() [32]byte { return s.fingerprint }

func newTrustedToolScope(nonSecretScope string) TrustedToolScope {
	if nonSecretScope == "" {
		return TrustedToolScope{}
	}
	return TrustedToolScope{fingerprint: sha256.Sum256([]byte(nonSecretScope))}
}

// TrustedToolIdentity binds a tool to the registration provenance, executor
// instance generation, and non-secret scope captured by the registry.
type TrustedToolIdentity struct {
	registrationSource     string
	toolName               string
	registrationGeneration uint64
	executorFingerprint    [32]byte
	scope                  TrustedToolScope
}

// TrustedToolIdentitySnapshot is a JSON-safe description of a trusted
// registration for durable audit facts. A snapshot is not dispatch authority
// and cannot be converted back into TrustedToolIdentity.
type TrustedToolIdentitySnapshot struct {
	RegistrationSource     string `json:"registration_source"`
	ToolName               string `json:"tool_name"`
	RegistrationGeneration uint64 `json:"registration_generation"`
	ExecutorFingerprint    string `json:"executor_fingerprint"`
	ScopeFingerprint       string `json:"scope_fingerprint"`
}

// RegistrationSource returns the source label minted by the registration
// boundary.
func (i TrustedToolIdentity) RegistrationSource() string { return i.registrationSource }

// ToolName returns the registered tool name.
func (i TrustedToolIdentity) ToolName() string { return i.toolName }

// RegistrationGeneration returns the per-name registration generation.
func (i TrustedToolIdentity) RegistrationGeneration() uint64 {
	return i.registrationGeneration
}

// ExecutorFingerprint returns the opaque fingerprint of the captured executor.
func (i TrustedToolIdentity) ExecutorFingerprint() [32]byte { return i.executorFingerprint }

// Scope returns the opaque workspace or endpoint scope identity.
func (i TrustedToolIdentity) Scope() TrustedToolScope { return i.scope }

// Snapshot returns a JSON-safe description of this identity. Fingerprints
// are lowercase hexadecimal facts; the snapshot cannot mint runtime trust.
func (i TrustedToolIdentity) Snapshot() TrustedToolIdentitySnapshot {
	return TrustedToolIdentitySnapshot{
		RegistrationSource:     i.registrationSource,
		ToolName:               i.toolName,
		RegistrationGeneration: i.registrationGeneration,
		ExecutorFingerprint:    hex.EncodeToString(i.executorFingerprint[:]),
		ScopeFingerprint:       hex.EncodeToString(i.scope.fingerprint[:]),
	}
}

func newTrustedToolIdentity(source, name string, generation uint64, executorFingerprint [32]byte, scope TrustedToolScope) TrustedToolIdentity {
	return TrustedToolIdentity{
		registrationSource:     source,
		toolName:               name,
		registrationGeneration: generation,
		executorFingerprint:    executorFingerprint,
		scope:                  scope,
	}
}

// FrozenToolCall is a defensive snapshot of a normalized JSON tool call.
type FrozenToolCall struct {
	id     string
	name   string
	args   map[string]any
	digest [32]byte
}

// ToolCall returns a deep copy of the frozen call.
func (c FrozenToolCall) ToolCall() ToolCall {
	return ToolCall{ID: c.id, Name: c.name, Args: cloneFrozenMap(c.args)}
}

// Digest returns the canonical digest of the normalized arguments.
func (c FrozenToolCall) Digest() [32]byte { return c.digest }

func freezeToolCall(call ToolCall) (FrozenToolCall, error) {
	var args map[string]any
	if call.Args != nil {
		cloned, err := cloneProtocolJSON(call.Args)
		if err != nil {
			return FrozenToolCall{}, fmt.Errorf("tools: freeze tool arguments: %w", err)
		}
		var ok bool
		args, ok = cloned.(map[string]any)
		if !ok {
			return FrozenToolCall{}, ErrPreparedValueNotJSON
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return FrozenToolCall{}, fmt.Errorf("tools: digest tool arguments: %w", err)
	}
	return FrozenToolCall{
		id:     call.ID,
		name:   call.Name,
		args:   args,
		digest: sha256.Sum256(encoded),
	}, nil
}

// ToolSchemaSnapshot is an immutable JSON schema captured with a registration.
type ToolSchemaSnapshot struct {
	present bool
	schema  any
	digest  [32]byte
}

// Present reports whether a schema was captured.
func (s ToolSchemaSnapshot) Present() bool { return s.present }

// Schema returns a defensive copy of the captured schema.
func (s ToolSchemaSnapshot) Schema() any { return cloneFrozenJSON(s.schema) }

// Digest returns the canonical digest of the captured schema.
func (s ToolSchemaSnapshot) Digest() [32]byte { return s.digest }

func freezeToolSchema(schema any) (ToolSchemaSnapshot, error) {
	if schema == nil {
		return ToolSchemaSnapshot{}, nil
	}
	cloned, err := cloneProtocolJSON(schema)
	if err != nil {
		return ToolSchemaSnapshot{}, fmt.Errorf("tools: freeze tool schema: %w", err)
	}
	encoded, err := json.Marshal(cloned)
	if err != nil {
		return ToolSchemaSnapshot{}, fmt.Errorf("tools: digest tool schema: %w", err)
	}
	return ToolSchemaSnapshot{present: true, schema: cloned, digest: sha256.Sum256(encoded)}, nil
}

func cloneProtocolJSON(value any) (any, error) {
	cloned, err := cloneProtocolJSONValue(reflect.ValueOf(value))
	if err != nil {
		return nil, err
	}
	if !cloned.IsValid() {
		return nil, nil
	}
	result := cloned.Interface()
	if _, err := json.Marshal(result); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPreparedValueNotJSON, err)
	}
	return result, nil
}

func cloneFrozenJSON(value any) any {
	cloned, err := cloneProtocolJSON(value)
	if err != nil {
		return nil
	}
	return cloned
}

func cloneFrozenMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	cloned, err := cloneProtocolJSON(value)
	if err != nil {
		return nil
	}
	clonedMap, ok := cloned.(map[string]any)
	if !ok {
		return nil
	}
	return clonedMap
}

var jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
var jsonNumberType = reflect.TypeOf(json.Number(""))

const maxPreparedJSONDepth = 128

func cloneProtocolJSONValue(value reflect.Value) (reflect.Value, error) {
	return cloneProtocolJSONValueAtDepth(value, 0)
}

func cloneProtocolJSONValueAtDepth(value reflect.Value, depth int) (reflect.Value, error) {
	if !value.IsValid() {
		return value, nil
	}
	if depth > maxPreparedJSONDepth {
		return reflect.Value{}, ErrPreparedValueNotJSON
	}
	if value.Type() != jsonNumberType && value.Type().Implements(jsonMarshalerType) {
		return reflect.Value{}, ErrPreparedValueNotJSON
	}

	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		cloned, err := cloneProtocolJSONValueAtDepth(value.Elem(), depth)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result, nil
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		if value.Type().Key().Kind() != reflect.String {
			return reflect.Value{}, ErrPreparedValueNotJSON
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			cloned, err := cloneProtocolJSONValueAtDepth(iterator.Value(), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.SetMapIndex(iterator.Key(), cloned)
		}
		return result, nil
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type()), nil
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			cloned, err := cloneProtocolJSONValueAtDepth(value.Index(index), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(index).Set(cloned)
		}
		return result, nil
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			cloned, err := cloneProtocolJSONValueAtDepth(value.Index(index), depth+1)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Index(index).Set(cloned)
		}
		return result, nil
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return value, nil
	default:
		return reflect.Value{}, ErrPreparedValueNotJSON
	}
}

// PreparedPolicyRequest is the immutable policy input for one normalized tool
// call. The callback can authorize this call but cannot replace its identity,
// effects, schema, or normalized arguments.
type PreparedPolicyRequest struct {
	Call        FrozenToolCall
	Identity    TrustedToolIdentity
	Effects     TrustedEffectSet
	Schema      ToolSchemaSnapshot
	Generations DispatchFenceSnapshot
	Fence       *DispatchFence
}

// PreparedPolicyDecision contains only the authorization result and the
// generation snapshot against which it was made. It cannot grant effects or
// change the captured tool identity.
type PreparedPolicyDecision struct {
	Allowed     bool
	Generations DispatchFenceSnapshot
}

// PreparedToolPolicy evaluates approval and Plan policy for a frozen
// registration and normalized call. Always-allow cache writes must use the
// request fence with its expected snapshot and return the resulting snapshot.
type PreparedToolPolicy func(context.Context, PreparedPolicyRequest) (PreparedPolicyDecision, error)

// ToolCallPreparer prepares a tool call without invoking its executor.
type ToolCallPreparer interface {
	toolCallPreparerCapability()
	PrepareToolCall(context.Context, ToolCall) (PreparedInvocation, error)
}

// PreparedInvocation is the one-use dispatch capability returned after schema
// validation and policy approval.
type PreparedInvocation interface {
	preparedInvocationCapability()
	ToolIdentity() TrustedToolIdentity
	EffectSet() TrustedEffectSet
	SchemaDigest() [32]byte
	NormalizedCall() ToolCall
	ArgumentDigest() [32]byte
	// DispatchCommitted reports whether this invocation crossed its final
	// generation-fence commit. It is runtime state, never restored from a
	// durable snapshot, and distinguishes a known pre-dispatch rejection from
	// executor errors that may reuse public error values.
	DispatchCommitted() bool
	// CompletionSignal returns this action's trusted positive-completion
	// signal, if its captured executor path can provide one. The proof enum is
	// available only after the returned Done channel closes.
	CompletionSignal() PreparedCompletionSignal
	Dispatch(context.Context) (*ToolResult, error)
}

// PreparedStreamingInvocation is a prepared capability for an executor that
// supports streamed output. Dispatch and DispatchStreaming share one-use state.
type PreparedStreamingInvocation interface {
	PreparedInvocation
	DispatchStreaming(context.Context, StreamSink) (*ToolResult, error)
}

type preparedInvocationConfig struct {
	fence           *DispatchFence
	generations     DispatchFenceSnapshot
	identity        TrustedToolIdentity
	effects         TrustedEffectSet
	schema          ToolSchemaSnapshot
	call            FrozenToolCall
	execute         func(context.Context, ToolCall) (*ToolResult, error)
	schedule        func(context.Context, *preparedInvocation) (*ToolResult, error)
	completionProof PreparedCompletionProof
}

type preparedInvocation struct {
	fence           *DispatchFence
	generations     DispatchFenceSnapshot
	identity        TrustedToolIdentity
	effects         TrustedEffectSet
	schema          ToolSchemaSnapshot
	call            FrozenToolCall
	execute         func(context.Context, ToolCall) (*ToolResult, error)
	schedule        func(context.Context, *preparedInvocation) (*ToolResult, error)
	completionProof PreparedCompletionProof
	completion      *preparedCompletionState
	consumed        atomic.Bool
	committed       atomic.Bool
}

var _ PreparedInvocation = (*preparedInvocation)(nil)

func (*preparedInvocation) preparedInvocationCapability() {}

func newPreparedInvocation(config preparedInvocationConfig) *preparedInvocation {
	return &preparedInvocation{
		fence:           config.fence,
		generations:     config.generations,
		identity:        config.identity,
		effects:         newTrustedEffectSet(config.effects.Values()...),
		schema:          config.schema,
		call:            config.call,
		execute:         config.execute,
		schedule:        config.schedule,
		completionProof: config.completionProof,
		completion:      newPreparedCompletionState(),
	}
}

// ToolIdentity returns the captured trusted registration identity.
func (p *preparedInvocation) ToolIdentity() TrustedToolIdentity { return p.identity }

// EffectSet returns the captured trusted effect set.
func (p *preparedInvocation) EffectSet() TrustedEffectSet {
	return newTrustedEffectSet(p.effects.Values()...)
}

// SchemaDigest returns the digest of the captured schema.
func (p *preparedInvocation) SchemaDigest() [32]byte { return p.schema.Digest() }

// NormalizedCall returns a defensive copy of the captured normalized call.
func (p *preparedInvocation) NormalizedCall() ToolCall { return p.call.ToolCall() }

// ArgumentDigest returns the digest of the captured normalized arguments.
func (p *preparedInvocation) ArgumentDigest() [32]byte { return p.call.Digest() }

// DispatchCommitted reports whether the final shared-fence commit marker ran.
func (p *preparedInvocation) DispatchCommitted() bool {
	return p != nil && p.committed.Load()
}

// CompletionSignal returns the completion state owned by this sealed
// invocation. Its channel may be attached during dispatch or queue acceptance.
func (p *preparedInvocation) CompletionSignal() PreparedCompletionSignal {
	if p == nil {
		return PreparedCompletionSignal{}
	}
	return p.completion.signal()
}

func (p *preparedInvocation) invalidateCompletionProof() {
	if p != nil {
		p.completion.invalidateProof()
	}
}

// Dispatch consumes the invocation and runs the captured executor outside the
// shared fence after its generation check and linearization marker complete.
func (p *preparedInvocation) Dispatch(ctx context.Context) (*ToolResult, error) {
	if p == nil {
		return nil, ErrPreparedInvocationUnavailable
	}
	if err := p.admit(); err != nil {
		return nil, err
	}
	if p.execute == nil {
		return nil, ErrPreparedInvocationUnavailable
	}
	if err := validatePreparedContext(ctx); err != nil {
		return nil, err
	}
	if p.schedule != nil {
		return p.schedule(ctx, p)
	}
	return p.commitAndInvoke(ctx)
}

// commitAndInvoke is also the only prepared execution entrypoint used by a
// queued worker after dequeue-time generation validation.
func (p *preparedInvocation) commitAndInvoke(ctx context.Context) (*ToolResult, error) {
	if p == nil || p.execute == nil {
		return nil, ErrPreparedInvocationUnavailable
	}
	if err := validatePreparedContext(ctx); err != nil {
		return nil, err
	}
	if err := p.commit(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	executeCtx := ctx
	if p.completionProof == CompletionOwnedProcessGroupJoined {
		executeCtx = withPreparedCompletionReporter(ctx, func(proof PreparedCompletionProof, done <-chan struct{}) {
			p.completion.attach(proof, done)
		})
		done := p.completion.begin(CompletionOwnedProcessGroupJoined)
		if done == nil {
			return nil, ErrPreparedInvocationUnavailable
		}
		completed := false
		defer func() { p.completion.finish(done, completed) }()
		result, err := p.execute(executeCtx, p.call.ToolCall())
		completed = true
		return result, err
	}
	if p.completionProof == CompletionInlinePipelineJoined {
		done := p.completion.begin(CompletionInlinePipelineJoined)
		if done == nil {
			return nil, ErrPreparedInvocationUnavailable
		}
		completed := false
		defer func() { p.completion.finish(done, completed) }()
		result, err := p.execute(executeCtx, p.call.ToolCall())
		completed = true
		return result, err
	}
	return p.execute(executeCtx, p.call.ToolCall())
}

func validatePreparedContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("tools: prepared dispatch context is nil")
	}
	return ctx.Err()
}

func (p *preparedInvocation) admit() error {
	if !p.consumed.CompareAndSwap(false, true) {
		return ErrPreparedInvocationConsumed
	}
	return nil
}

func (p *preparedInvocation) commit(ctx context.Context) error {
	if p == nil || p.fence == nil {
		return ErrDispatchFenceUnavailable
	}
	var contextErr error
	err := p.fence.commit(p.generations, func() {
		contextErr = ctx.Err()
		if contextErr == nil {
			p.committed.Store(true)
		}
	})
	if err != nil {
		return err
	}
	if contextErr != nil {
		return contextErr
	}
	return nil
}

type preparedStreamingInvocation struct {
	*preparedInvocation
	executeStreaming func(context.Context, ToolCall, StreamSink) (*ToolResult, error)
}

var _ PreparedStreamingInvocation = (*preparedStreamingInvocation)(nil)

func newPreparedStreamingInvocation(config preparedInvocationConfig, executeStreaming func(context.Context, ToolCall, StreamSink) (*ToolResult, error)) *preparedStreamingInvocation {
	return &preparedStreamingInvocation{
		preparedInvocation: newPreparedInvocation(config),
		executeStreaming:   executeStreaming,
	}
}

// DispatchStreaming consumes the same one-use state as Dispatch and invokes
// the captured streaming executor with the caller's sink.
func (p *preparedStreamingInvocation) DispatchStreaming(ctx context.Context, sink StreamSink) (*ToolResult, error) {
	if p == nil || p.preparedInvocation == nil {
		return nil, ErrPreparedInvocationUnavailable
	}
	if err := p.admit(); err != nil {
		return nil, err
	}
	if p.executeStreaming == nil {
		return nil, ErrPreparedInvocationUnavailable
	}
	if err := validatePreparedContext(ctx); err != nil {
		return nil, err
	}
	if err := p.commit(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	executeCtx := ctx
	if p.completionProof == CompletionOwnedProcessGroupJoined {
		executeCtx = withPreparedCompletionReporter(ctx, func(proof PreparedCompletionProof, done <-chan struct{}) {
			p.completion.attach(proof, done)
		})
		done := p.completion.begin(CompletionOwnedProcessGroupJoined)
		if done == nil {
			return nil, ErrPreparedInvocationUnavailable
		}
		completed := false
		defer func() { p.completion.finish(done, completed) }()
		result, err := p.executeStreaming(executeCtx, p.call.ToolCall(), sink)
		completed = true
		return result, err
	}
	if p.completionProof == CompletionInlinePipelineJoined {
		done := p.completion.begin(CompletionInlinePipelineJoined)
		if done == nil {
			return nil, ErrPreparedInvocationUnavailable
		}
		completed := false
		defer func() { p.completion.finish(done, completed) }()
		result, err := p.executeStreaming(executeCtx, p.call.ToolCall(), sink)
		completed = true
		return result, err
	}
	return p.executeStreaming(executeCtx, p.call.ToolCall(), sink)
}
