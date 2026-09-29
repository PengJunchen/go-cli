package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
)

// ProviderOperationID identifies one logical provider operation.
type ProviderOperationID string

// ProviderAttemptID identifies one request that crossed an adapter's dispatch fence.
type ProviderAttemptID string

// CandidateID is an opaque resolver-issued provider candidate identifier.
type CandidateID string

// CandidateReason describes why an operation proposed a resolved candidate.
type CandidateReason uint8

const (
	// CandidateInitial is the first candidate for an operation.
	CandidateInitial CandidateReason = iota + 1
	// CandidateRetry is a candidate proposed after a retryable finished attempt.
	CandidateRetry
	// CandidateFailover is a configured alternative candidate.
	CandidateFailover
	// CandidateOverflow is a candidate proposed after authorized overflow recovery.
	CandidateOverflow
	// CandidateCyclerSelected is the model selected by a model cycler.
	CandidateCyclerSelected
	// CandidateCyclerPrimary is a cycler's configured primary candidate.
	CandidateCyclerPrimary
)

// ProviderBuildMode fixes whether a built model is a direct library model or a
// resolver-bound candidate.
type ProviderBuildMode uint8

const (
	// UnboundLibraryBuild creates a direct one-call model outside Loop accounting.
	UnboundLibraryBuild ProviderBuildMode = iota + 1
	// GuardedCandidateBuild permanently binds a model to a resolver candidate.
	GuardedCandidateBuild
)

// ProviderOperationKind distinguishes independent kinds of provider work.
type ProviderOperationKind uint8

const (
	// OperationMainRequest is a main model request.
	OperationMainRequest ProviderOperationKind = iota + 1
	// OperationContinuation is a model continuation request.
	OperationContinuation
	// OperationSummary is a compaction summary request.
	OperationSummary
)

// OriginKind describes how the resolver selected an endpoint without exposing
// its URL.
type OriginKind uint8

const (
	// OriginResolverDefaultEndpoint identifies the resolver's configured endpoint.
	OriginResolverDefaultEndpoint OriginKind = iota + 1
	// OriginExplicitRequestEndpoint identifies an endpoint supplied by the request.
	OriginExplicitRequestEndpoint
)

// AttemptOutcome describes the terminal outcome of a dispatched provider call.
type AttemptOutcome uint8

const (
	// AttemptSuccess identifies a completed successful provider call.
	AttemptSuccess AttemptOutcome = iota + 1
	// AttemptProviderFailure identifies a provider response or response-processing failure.
	AttemptProviderFailure
	// AttemptTransportFailure identifies a request that failed in the HTTP transport.
	AttemptTransportFailure
	// AttemptCancelled identifies a dispatched call that ended through cancellation.
	AttemptCancelled
)

// UsageState distinguishes authoritative usage from usage that was not reported.
type UsageState uint8

const (
	// UsageKnown means the provider reported token counts.
	UsageKnown UsageState = iota + 1
	// UsageUnknown means no authoritative token counts were available.
	UsageUnknown
)

// UsageSource describes where an attempt's usage evidence came from.
type UsageSource uint8

const (
	// UsageProviderResponse identifies usage returned by the provider.
	UsageProviderResponse UsageSource = iota + 1
	// UsageNoAuthoritativeUsage identifies an attempt without provider usage.
	UsageNoAuthoritativeUsage
)

// FailureClass carries the typed terminal classification used by operation
// policy. It describes a dispatched attempt only.
type FailureClass uint8

const (
	// FailureNone means the attempt succeeded.
	FailureNone FailureClass = iota
	// FailureTransientProvider means a transient provider or transport failure.
	FailureTransientProvider
	// FailureOverflow means a typed context-limit response.
	FailureOverflow
	// FailureAuth means authentication or authorization failed.
	FailureAuth
	// FailureInvalidRequest means the provider rejected the request as invalid.
	FailureInvalidRequest
	// FailureProtocol means the provider response violated the expected protocol.
	FailureProtocol
	// FailureCancelled means the attempt ended after it was canceled.
	FailureCancelled
	// FailureOther means the failure is not eligible for a generic retry policy.
	FailureOther
)

// PreDispatchFailureClass classifies candidate construction failures that happen
// before an HTTP request crosses the dispatch fence.
type PreDispatchFailureClass uint8

const (
	// PreDispatchCandidateUnavailable is the only class eligible for safe build fallback.
	PreDispatchCandidateUnavailable PreDispatchFailureClass = iota + 1
	// PreDispatchAuth identifies a credential or authentication setup failure.
	PreDispatchAuth
	// PreDispatchInvalidRequest identifies request validation or schema failure.
	PreDispatchInvalidRequest
	// PreDispatchProtocol identifies a protocol setup failure.
	PreDispatchProtocol
	// PreDispatchCancelled identifies cancellation before dispatch.
	PreDispatchCancelled
	// PreDispatchSetupUnknown is an unclassified candidate construction failure.
	PreDispatchSetupUnknown
)

// AntecedentKind identifies the typed fact that permits a candidate proposal.
type AntecedentKind uint8

const (
	// AntecedentInitial identifies an operation's first candidate proposal.
	AntecedentInitial AntecedentKind = iota + 1
	// AntecedentFinishedAttempt references an immutable finished provider attempt.
	AntecedentFinishedAttempt
	// AntecedentFinishedOverflow references a finished context-limit attempt.
	AntecedentFinishedOverflow
	// AntecedentPreDispatchFailure identifies a setup failure with no attempt ID.
	AntecedentPreDispatchFailure
)

// AttemptIdentity contains safe origin and candidate provenance for accounting
// and diagnostics. It deliberately omits endpoint, credential, headers, and
// prompt data.
type AttemptIdentity struct {
	ProviderID string
	ModelID    string
	ProtocolID ProtocolID
	OriginKind OriginKind
	Candidate  CandidateID
}

// PreDispatchCandidateFailure describes a failed candidate construction without
// relabeling a prior provider attempt.
type PreDispatchCandidateFailure struct {
	FailedCandidateID CandidateID
	Class             PreDispatchFailureClass
	PreviousAttemptID ProviderAttemptID
}

// CandidateAntecedent is a tagged union. Initial has no attempt or setup
// failure; finished attempt kinds have only AttemptID; pre-dispatch failure has
// only PreDispatch.
type CandidateAntecedent struct {
	Kind        AntecedentKind
	AttemptID   ProviderAttemptID
	PreDispatch *PreDispatchCandidateFailure
}

// DispatchDenialCode identifies a policy denial without embedding provider data.
type DispatchDenialCode uint8

const (
	// DeniedCancelled means the operation context is canceled.
	DeniedCancelled DispatchDenialCode = iota + 1
	// DeniedReplayForbidden means the operation cannot replay its current action.
	DeniedReplayForbidden
	// DeniedSafetyFact means a required Loop-owned safety fact blocks dispatch.
	DeniedSafetyFact
	// DeniedBudget means the operation has used its actual-dispatch allowance.
	DeniedBudget
	// DeniedStalePrevious means the proposal references a stale or unknown attempt.
	DeniedStalePrevious
	// DeniedCandidateMismatch means the proposal candidate does not match the adapter.
	DeniedCandidateMismatch
	// DeniedCandidateOutsideSet means the candidate is outside the finite operation set.
	DeniedCandidateOutsideSet
	// DeniedRepeatedPreDispatchFailure means this setup failure was already proposed.
	DeniedRepeatedPreDispatchFailure
	// DeniedOverflowRecoveryUsed means the operation already used overflow recovery.
	DeniedOverflowRecoveryUsed
)

// DispatchDenial is a typed policy rejection returned before provider dispatch.
type DispatchDenial struct {
	Code DispatchDenialCode
}

// DispatchDecision carries an authorization decision or a typed policy denial.
// The adapter creates its private one-shot permit only after authorization.
type DispatchDecision struct {
	Authorized bool
	Denial     *DispatchDenial
}

// UsageEvidence retains explicit unknown usage rather than interpreting zero
// tokens as missing data.
type UsageEvidence struct {
	State  UsageState
	Usage  *Usage
	Source UsageSource
}

// UnknownUsageEvidence returns an explicit unknown-usage value.
func UnknownUsageEvidence() UsageEvidence {
	return UsageEvidence{State: UsageUnknown, Source: UsageNoAuthoritativeUsage}
}

// ProviderAttemptTerminal is the immutable terminal data submitted by a guarded
// adapter after its actual call reaches a terminal state.
type ProviderAttemptTerminal struct {
	Outcome                 AttemptOutcome
	Failure                 FailureClass
	Usage                   UsageEvidence
	ProviderReportedCostUSD *float64
}

// ProviderAttemptResult is the immutable result stored by the operation owner.
type ProviderAttemptResult struct {
	OperationID             ProviderOperationID
	AttemptID               ProviderAttemptID
	Identity                AttemptIdentity
	Outcome                 AttemptOutcome
	Failure                 FailureClass
	Usage                   UsageEvidence
	ProviderReportedCostUSD *float64
}

// DispatchPermit is an llm-owned one-shot authorization for one operation and
// resolver-issued candidate. Its zero value is invalid and callers cannot
// construct a valid permit.
type DispatchPermit struct {
	operation *operationAuthorityBinding
	candidate *resolvedCandidateHandle
	state     *atomic.Uint32
}

// OperationID returns the operation that authorized this permit.
func (p *DispatchPermit) OperationID() ProviderOperationID {
	if p == nil || p.operation == nil {
		return ""
	}
	return p.operation.id
}

// CandidateID returns the resolver-issued candidate bound to this permit.
func (p *DispatchPermit) CandidateID() CandidateID {
	if p == nil || p.candidate == nil {
		return ""
	}
	return p.candidate.id
}

// ValidFor reports whether this permit is currently being presented at the
// dispatch fence for its bound authority and exact candidate handle.
func (p *DispatchPermit) ValidFor(authority OperationDispatchAuthority, candidate ResolvedCandidateHandle) bool {
	if p == nil || p.operation == nil || p.candidate == nil || p.state == nil || p.state.Load() != 1 || candidate == nil || nilOperationAuthority(authority) {
		return false
	}
	resolved, ok := candidate.(*resolvedCandidateHandle)
	return ok && p.candidate == resolved && p.operation.id == authority.ID() && sameOperationAuthority(p.operation.authority, authority)
}

// ResolvedCandidateHandle exposes safe identity for one immutable candidate.
// Its unexported marker prevents callers outside this package from minting a
// candidate handle that was not created by ModelResolver.
type ResolvedCandidateHandle interface {
	CandidateID() CandidateID
	Identity() AttemptIdentity
	resolvedCandidateHandle()
}

// CandidateProposal proposes one finite resolver-issued candidate to a Loop
// operation authority.
type CandidateProposal struct {
	OperationID ProviderOperationID
	Candidate   ResolvedCandidateHandle
	Reason      CandidateReason
	Antecedent  CandidateAntecedent
}

// OperationDispatchAuthority is implemented by the Loop operation owner, which
// alone authorizes proposals, accounts for the actual dispatch fence, and
// stores one immutable terminal result.
type OperationDispatchAuthority interface {
	ID() ProviderOperationID
	AuthorizeCandidate(context.Context, CandidateProposal) (DispatchDecision, error)
	// RecordPreDispatchFailure validates resolver-created setup evidence and
	// returns the Loop-owned antecedent allowed for a later safe proposal.
	RecordPreDispatchFailure(context.Context, error) (PreDispatchCandidateFailure, error)
	// BeginDispatch must validate permit.ValidFor with its receiver and the exact candidate before allocating an attempt ID.
	BeginDispatch(*DispatchPermit, ResolvedCandidateHandle) (ProviderAttemptID, error)
	FinishAttempt(ProviderAttemptResult) error
}

// OverflowRecoveryAuthority is the optional Loop-owned gate for a single
// typed context-limit recovery. Core consumes it before compaction; candidate
// authorization then requires the same finished overflow attempt.
type OverflowRecoveryAuthority interface {
	BeginOverflowRecovery(context.Context, ProviderAttemptID) error
}

// RunOperationFactory creates independent main, continuation, or summary
// operation authorities backed by the run's accounting sink.
type RunOperationFactory interface {
	NewOperation(context.Context, ProviderOperationKind, int, []ResolvedCandidateHandle) (OperationDispatchAuthority, error)
}

// ProviderAttempt is an llm-owned concrete one-shot handle for one actual
// provider dispatch. Its zero value is invalid and callers cannot implement
// or mint an attempt.
type ProviderAttempt struct {
	operation *operationAuthorityBinding
	id        ProviderAttemptID
	identity  AttemptIdentity
	finished  *atomic.Bool
}

// ID returns the opaque identifier allocated at the actual dispatch fence.
func (a *ProviderAttempt) ID() ProviderAttemptID {
	if a == nil {
		return ""
	}
	return a.id
}

// Finish reports one terminal result to the operation owner. The handle
// rejects duplicate finishes even when callers race.
func (a *ProviderAttempt) Finish(terminal ProviderAttemptTerminal) (ProviderAttemptResult, error) {
	if a == nil || a.operation == nil || a.id == "" || a.finished == nil {
		return ProviderAttemptResult{}, errors.New("llm: invalid provider attempt")
	}
	if !a.finished.CompareAndSwap(false, true) {
		return ProviderAttemptResult{}, errors.New("llm: provider attempt already finished")
	}
	terminal = cloneProviderAttemptTerminal(terminal)
	result := ProviderAttemptResult{
		OperationID:             a.operation.id,
		AttemptID:               a.id,
		Identity:                a.identity,
		Outcome:                 terminal.Outcome,
		Failure:                 terminal.Failure,
		Usage:                   cloneUsageEvidence(terminal.Usage),
		ProviderReportedCostUSD: cloneFloat(terminal.ProviderReportedCostUSD),
	}
	if err := a.operation.authority.FinishAttempt(cloneProviderAttemptResult(result)); err != nil {
		return result, err
	}
	return result, nil
}

// PreDispatchCandidateError describes a typed candidate setup failure. It does
// not represent a provider attempt and therefore has no AttemptID.
type PreDispatchCandidateError struct {
	CandidateID CandidateID
	Class       PreDispatchFailureClass
	Err         error
	candidate   *resolvedCandidateHandle
}

// Error returns a safe summary that does not include provider endpoint data.
func (e *PreDispatchCandidateError) Error() string {
	if e == nil {
		return "llm: pre-dispatch candidate setup failed"
	}
	return "llm: pre-dispatch candidate setup failed"
}

// Unwrap preserves the original setup failure for typed checks.
func (e *PreDispatchCandidateError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func preDispatchFailureClass(err error) PreDispatchFailureClass {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return PreDispatchCancelled
	case errors.Is(err, ErrProviderCredentialRequired), errors.Is(err, ErrCredentialScopeMismatch):
		return PreDispatchAuth
	case errors.Is(err, ErrUnsupportedProviderProtocol):
		return PreDispatchProtocol
	case errors.Is(err, ErrProviderEndpointRequired):
		return PreDispatchInvalidRequest
	default:
		return PreDispatchSetupUnknown
	}
}

func preDispatchCandidateError(candidate *resolvedCandidateHandle, class PreDispatchFailureClass, err error) error {
	if err == nil {
		return nil
	}
	var existing *PreDispatchCandidateError
	if errors.As(err, &existing) {
		return err
	}
	candidateID := CandidateID("")
	if candidate != nil {
		candidateID = candidate.id
	}
	if class < PreDispatchCandidateUnavailable || class > PreDispatchSetupUnknown {
		class = preDispatchFailureClass(err)
	}
	return &PreDispatchCandidateError{CandidateID: candidateID, Class: class, Err: err, candidate: candidate}
}

// PreDispatchCandidateFailureFromError returns resolver-owned setup evidence.
// A caller-created PreDispatchCandidateError is rejected because it lacks the
// private candidate binding attached by this package.
func PreDispatchCandidateFailureFromError(err error) (PreDispatchCandidateFailure, bool) {
	var setupErr *PreDispatchCandidateError
	if !errors.As(err, &setupErr) || setupErr == nil || setupErr.candidate == nil ||
		setupErr.CandidateID == "" || setupErr.CandidateID != setupErr.candidate.id ||
		setupErr.Class < PreDispatchCandidateUnavailable || setupErr.Class > PreDispatchSetupUnknown {
		return PreDispatchCandidateFailure{}, false
	}
	return PreDispatchCandidateFailure{FailedCandidateID: setupErr.candidate.id, Class: setupErr.Class}, true
}

var (
	// ErrOperationAuthorityRequired is returned when a guarded model has no Loop authority.
	ErrOperationAuthorityRequired = errors.New("llm: guarded provider model requires an operation authority")
	// ErrGuardedCandidateUnavailable is returned when a resolver build cannot create a guarded adapter.
	ErrGuardedCandidateUnavailable = errors.New("llm: resolved candidate has no guarded provider adapter")
	// ErrDispatchPermitRequired is returned when guarded dispatch lacks a matching permit.
	ErrDispatchPermitRequired = errors.New("llm: guarded provider dispatch requires a matching permit")
	// ErrDispatchPermitDenied identifies a typed Loop policy denial.
	ErrDispatchPermitDenied = errors.New("llm: provider dispatch was denied")
	// ErrInvalidDispatchDecision identifies a malformed authority response.
	ErrInvalidDispatchDecision = errors.New("llm: operation authority returned an invalid dispatch decision")
)

type attemptContextKey uint8

const (
	runOperationFactoryContextKey attemptContextKey = iota + 1
	operationDispatchAuthorityContextKey
	candidateProposalContextKey
)

type operationAuthorityBinding struct {
	authority OperationDispatchAuthority
	id        ProviderOperationID
}

func nilOperationAuthority(authority OperationDispatchAuthority) bool {
	if authority == nil {
		return true
	}
	value := reflect.ValueOf(authority)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func sameOperationAuthority(left, right OperationDispatchAuthority) bool {
	if nilOperationAuthority(left) || nilOperationAuthority(right) {
		return false
	}
	leftType := reflect.TypeOf(left)
	if leftType != reflect.TypeOf(right) || !leftType.Comparable() {
		return false
	}
	return reflect.ValueOf(left).Interface() == reflect.ValueOf(right).Interface()
}

// ProviderCandidateSource exposes the resolver-created finite candidate set
// available to one Loop operation. Candidate handles remain opaque and are
// usable only by the guarded adapter bound to the same handle.
type ProviderCandidateSource interface {
	ProviderCandidates() []ResolvedCandidateHandle
}

// ProviderCandidatesFromModel returns a defensive copy of resolver-owned
// candidates exposed by a model or wrapper chain.
func ProviderCandidatesFromModel(model BaseChatModel) []ResolvedCandidateHandle {
	if model == nil {
		return nil
	}
	source, ok := model.(ProviderCandidateSource)
	if !ok {
		return nil
	}
	return append([]ResolvedCandidateHandle(nil), source.ProviderCandidates()...)
}

type resolvedCandidateModel struct {
	inner     BaseChatModel
	candidate *resolvedCandidateHandle
}

func (m *resolvedCandidateModel) Generate(ctx context.Context, messages []Message, opts ...Option) (*Message, error) {
	return m.inner.Generate(ctx, messages, opts...)
}

func (m *resolvedCandidateModel) Stream(ctx context.Context, messages []Message, opts ...Option) (<-chan MessageChunk, error) {
	return m.inner.Stream(ctx, messages, opts...)
}

func (m *resolvedCandidateModel) ProviderCandidates() []ResolvedCandidateHandle {
	if m == nil || m.candidate == nil {
		return nil
	}
	return []ResolvedCandidateHandle{m.candidate}
}

type candidateCleanupBinding struct {
	parent  *candidateCleanupBinding
	once    sync.Once
	cleanup func()
}

type candidateCleanupContextKey struct{}

func withCandidateCleanup(ctx context.Context, cleanup func()) context.Context {
	if cleanup == nil {
		return ctx
	}
	parent, _ := ctx.Value(candidateCleanupContextKey{}).(*candidateCleanupBinding)
	return context.WithValue(ctx, candidateCleanupContextKey{}, &candidateCleanupBinding{parent: parent, cleanup: cleanup})
}

func finishCandidateCleanup(ctx context.Context) {
	binding, _ := ctx.Value(candidateCleanupContextKey{}).(*candidateCleanupBinding)
	for current := binding; current != nil; current = current.parent {
		current.once.Do(current.cleanup)
	}
}

// WithRunOperationFactory attaches the run-scoped factory used for independent
// summary operations.
func WithRunOperationFactory(ctx context.Context, factory RunOperationFactory) context.Context {
	return context.WithValue(ctx, runOperationFactoryContextKey, factory)
}

// RunOperationFactoryFromContext returns the run-scoped operation factory.
func RunOperationFactoryFromContext(ctx context.Context) (RunOperationFactory, bool) {
	factory, ok := ctx.Value(runOperationFactoryContextKey).(RunOperationFactory)
	return factory, ok && factory != nil
}

// WithOperationDispatchAuthority attaches the current provider operation owner.
// This is a trusted application-composition seam: production callers must use
// the Loop-owned factory. Model, tool, and configuration input must never
// install or replace an authority. Resolver-built adapters still require this
// authority and create their private one-shot permit only after it authorizes a
// candidate.
func WithOperationDispatchAuthority(ctx context.Context, operation OperationDispatchAuthority) context.Context {
	if nilOperationAuthority(operation) {
		return context.WithValue(ctx, operationDispatchAuthorityContextKey, (*operationAuthorityBinding)(nil))
	}
	return context.WithValue(ctx, operationDispatchAuthorityContextKey, &operationAuthorityBinding{authority: operation, id: operation.ID()})
}

// OperationDispatchAuthorityFromContext returns the current provider operation owner.
func OperationDispatchAuthorityFromContext(ctx context.Context) (OperationDispatchAuthority, bool) {
	binding, ok := ctx.Value(operationDispatchAuthorityContextKey).(*operationAuthorityBinding)
	if !ok || binding == nil || nilOperationAuthority(binding.authority) || binding.id == "" {
		return nil, false
	}
	return binding.authority, true
}

// WithCandidateProposal attaches a typed candidate proposal for the next guarded adapter.
func WithCandidateProposal(ctx context.Context, proposal CandidateProposal) context.Context {
	return context.WithValue(ctx, candidateProposalContextKey, proposal)
}

// CandidateProposalFromContext returns a typed proposal attached by a model-chain wrapper.
func CandidateProposalFromContext(ctx context.Context) (CandidateProposal, bool) {
	proposal, ok := ctx.Value(candidateProposalContextKey).(CandidateProposal)
	return proposal, ok
}

type guardedResolvedModelBuilder interface {
	buildGuarded(context.Context, ModelConfig, *resolvedCandidateHandle) (BaseChatModel, func(), error)
}

type resolvedCandidateHandle struct {
	id       CandidateID
	identity AttemptIdentity
	provider ModelProvider
	cfg      ModelConfig
}

func (c *resolvedCandidateHandle) CandidateID() CandidateID  { return c.id }
func (c *resolvedCandidateHandle) Identity() AttemptIdentity { return c.identity }
func (*resolvedCandidateHandle) resolvedCandidateHandle()    {}

func (c *resolvedCandidateHandle) build(ctx context.Context) (BaseChatModel, func(), error) {
	builder, ok := c.provider.(guardedResolvedModelBuilder)
	if !ok {
		return nil, nil, preDispatchCandidateError(c, PreDispatchCandidateUnavailable, ErrGuardedCandidateUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, preDispatchCandidateError(c, PreDispatchCancelled, err)
	}
	model, cleanup, err := builder.buildGuarded(ctx, c.cfg, c)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		class := preDispatchFailureClass(err)
		if errors.Is(err, ErrGuardedCandidateUnavailable) {
			class = PreDispatchCandidateUnavailable
		}
		return nil, nil, preDispatchCandidateError(c, class, err)
	}
	if model == nil {
		if cleanup != nil {
			cleanup()
		}
		return nil, nil, preDispatchCandidateError(c, PreDispatchCandidateUnavailable, ErrGuardedCandidateUnavailable)
	}
	return model, cleanup, nil
}

func newCandidateID() (CandidateID, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("llm: create opaque candidate identity: %w", err)
	}
	return CandidateID(hex.EncodeToString(bytes)), nil
}

type providerDispatchBinding struct {
	mode      ProviderBuildMode
	candidate *resolvedCandidateHandle
}

func (b providerDispatchBinding) begin(ctx context.Context) (*ProviderAttempt, error) {
	if b.mode == UnboundLibraryBuild {
		return nil, nil
	}
	if b.mode != GuardedCandidateBuild || b.candidate == nil || b.candidate.id == "" {
		return nil, ErrGuardedCandidateUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, preDispatchCandidateError(b.candidate, PreDispatchCancelled, err)
	}
	binding, ok := ctx.Value(operationDispatchAuthorityContextKey).(*operationAuthorityBinding)
	if !ok || binding == nil || nilOperationAuthority(binding.authority) || binding.id == "" {
		return nil, ErrOperationAuthorityRequired
	}
	if binding.authority.ID() != binding.id {
		return nil, ErrOperationAuthorityRequired
	}

	proposal, hasProposal := CandidateProposalFromContext(ctx)
	if hasProposal {
		proposalCandidate, candidateOK := proposal.Candidate.(*resolvedCandidateHandle)
		if proposal.OperationID != binding.id || !candidateOK || proposalCandidate != b.candidate {
			return nil, ErrDispatchPermitRequired
		}
	} else {
		proposal = CandidateProposal{
			OperationID: binding.id,
			Candidate:   b.candidate,
			Reason:      CandidateInitial,
			Antecedent:  CandidateAntecedent{Kind: AntecedentInitial},
		}
	}

	decision, err := binding.authority.AuthorizeCandidate(ctx, proposal)
	if err != nil {
		return nil, fmt.Errorf("llm: authorize provider candidate: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, preDispatchCandidateError(b.candidate, PreDispatchCancelled, err)
	}
	if decision.Denial != nil {
		if decision.Authorized {
			return nil, ErrInvalidDispatchDecision
		}
		return nil, &DispatchDeniedError{Code: decision.Denial.Code}
	}
	if !decision.Authorized {
		return nil, ErrInvalidDispatchDecision
	}
	permit := &DispatchPermit{operation: binding, candidate: b.candidate, state: &atomic.Uint32{}}
	if !permit.begin(binding, b.candidate) {
		return nil, ErrDispatchPermitRequired
	}
	if err := ctx.Err(); err != nil {
		permit.end()
		return nil, preDispatchCandidateError(b.candidate, PreDispatchCancelled, err)
	}
	attemptID, err := binding.authority.BeginDispatch(permit, b.candidate)
	permit.end()
	if err != nil {
		return nil, fmt.Errorf("llm: begin provider dispatch: %w", err)
	}
	if attemptID == "" {
		return nil, errors.New("llm: dispatch authority returned an invalid provider attempt ID")
	}
	return &ProviderAttempt{operation: binding, id: attemptID, identity: b.candidate.identity, finished: &atomic.Bool{}}, nil
}

func (p *DispatchPermit) begin(operation *operationAuthorityBinding, candidate *resolvedCandidateHandle) bool {
	if p == nil || p.operation != operation || p.candidate != candidate || p.state == nil || operation == nil || candidate == nil {
		return false
	}
	return p.state.CompareAndSwap(0, 1)
}

func (p *DispatchPermit) end() {
	if p != nil && p.state != nil {
		p.state.Store(2)
	}
}

// DispatchDeniedError reports an operation policy denial without exposing
// provider or request data.
type DispatchDeniedError struct {
	Code DispatchDenialCode
}

// Error implements error.
func (*DispatchDeniedError) Error() string { return ErrDispatchPermitDenied.Error() }

// Unwrap supports errors.Is checks for a dispatch policy denial.
func (*DispatchDeniedError) Unwrap() error { return ErrDispatchPermitDenied }

type providerTransportFailure struct{ cause error }

func (e *providerTransportFailure) Error() string {
	if e == nil || e.cause == nil {
		return "llm: provider transport request failed"
	}
	return e.cause.Error()
}
func (e *providerTransportFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func providerAttemptTerminal(ctx context.Context, err error, usage *Usage) ProviderAttemptTerminal {
	evidence := usageEvidence(usage)
	if err == nil {
		return ProviderAttemptTerminal{Outcome: AttemptSuccess, Failure: FailureNone, Usage: evidence}
	}
	if ctx != nil && (errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return ProviderAttemptTerminal{Outcome: AttemptCancelled, Failure: FailureCancelled, Usage: evidence}
	}
	var transportErr *providerTransportFailure
	if errors.As(err, &transportErr) {
		return ProviderAttemptTerminal{Outcome: AttemptTransportFailure, Failure: FailureTransientProvider, Usage: evidence}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ProviderAttemptTerminal{Outcome: AttemptCancelled, Failure: FailureCancelled, Usage: evidence}
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		failure := FailureOther
		switch providerErr.ErrorType {
		case ErrTypeAuth:
			failure = FailureAuth
		case ErrTypeOverflow:
			failure = FailureOverflow
		case ErrTypeInvalidRequest:
			failure = FailureInvalidRequest
		case ErrTypeRateLimit, ErrTypeServer, ErrTypeNetwork:
			failure = FailureTransientProvider
		}
		return ProviderAttemptTerminal{Outcome: AttemptProviderFailure, Failure: failure, Usage: evidence}
	}
	return ProviderAttemptTerminal{Outcome: AttemptProviderFailure, Failure: FailureProtocol, Usage: evidence}
}

func usageEvidence(usage *Usage) UsageEvidence {
	if usage == nil {
		return UnknownUsageEvidence()
	}
	copyUsage := *usage
	return UsageEvidence{State: UsageKnown, Usage: &copyUsage, Source: UsageProviderResponse}
}

func cloneUsageEvidence(evidence UsageEvidence) UsageEvidence {
	clone := evidence
	if evidence.Usage != nil {
		usage := *evidence.Usage
		clone.Usage = &usage
	}
	return clone
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func cloneProviderAttemptTerminal(terminal ProviderAttemptTerminal) ProviderAttemptTerminal {
	clone := terminal
	clone.Usage = cloneUsageEvidence(terminal.Usage)
	clone.ProviderReportedCostUSD = cloneFloat(terminal.ProviderReportedCostUSD)
	return clone
}

func cloneProviderAttemptResult(result ProviderAttemptResult) ProviderAttemptResult {
	clone := result
	clone.Usage = cloneUsageEvidence(result.Usage)
	clone.ProviderReportedCostUSD = cloneFloat(result.ProviderReportedCostUSD)
	return clone
}

func finishProviderAttempt(ctx context.Context, attempt *ProviderAttempt, err error, usage *Usage) error {
	if attempt == nil {
		return err
	}
	terminal := providerAttemptTerminal(ctx, err, usage)
	finishCandidateCleanup(ctx)
	result, finishErr := attempt.Finish(terminal)
	if finishErr != nil {
		return errors.Join(err, fmt.Errorf("llm: finish provider attempt %s: %w", attempt.ID(), finishErr))
	}
	if result.AttemptID != attempt.ID() {
		return errors.Join(err, errors.New("llm: provider attempt returned a mismatched terminal result"))
	}
	if err != nil {
		return &FinishedProviderAttemptError{Result: result, cause: err}
	}
	return nil
}

// FinishedProviderAttemptError keeps the provider error chain available while
// carrying the immutable result that the operation owner already finished.
type FinishedProviderAttemptError struct {
	Result ProviderAttemptResult
	cause  error
}

func (e *FinishedProviderAttemptError) Error() string {
	if e == nil || e.cause == nil {
		return "llm: provider attempt failed"
	}
	return e.cause.Error()
}

func (e *FinishedProviderAttemptError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// FinishedProviderAttemptFromError extracts the immutable finished result
// attached by the actual provider adapter. Wrapper code must not infer an
// attempt from an ordinary provider error.
func FinishedProviderAttemptFromError(err error) (ProviderAttemptResult, bool) {
	var finished *FinishedProviderAttemptError
	if !errors.As(err, &finished) || finished == nil || finished.Result.AttemptID == "" || finished.Result.OperationID == "" {
		return ProviderAttemptResult{}, false
	}
	return cloneProviderAttemptResult(finished.Result), true
}

func withFinishedAttemptProposal(ctx context.Context, model BaseChatModel, reason CandidateReason, result ProviderAttemptResult, candidateID CandidateID) (context.Context, error) {
	authority, active := OperationDispatchAuthorityFromContext(ctx)
	if !active {
		return ctx, nil
	}
	candidates := ProviderCandidatesFromModel(model)
	var candidate ResolvedCandidateHandle
	for _, current := range candidates {
		if candidateID != "" && current.CandidateID() == candidateID {
			candidate = current
			break
		}
		if candidateID == "" && len(candidates) == 1 {
			candidate = current
			break
		}
	}
	if candidate == nil {
		return nil, ErrDispatchPermitRequired
	}
	antecedent := CandidateAntecedent{Kind: AntecedentFinishedAttempt, AttemptID: result.AttemptID}
	if reason == CandidateOverflow {
		antecedent.Kind = AntecedentFinishedOverflow
	}
	return WithCandidateProposal(ctx, CandidateProposal{
		OperationID: authority.ID(),
		Candidate:   candidate,
		Reason:      reason,
		Antecedent:  antecedent,
	}), nil
}

func withPreDispatchCandidateProposal(ctx context.Context, nextModel BaseChatModel, failedErr error, reason CandidateReason) (context.Context, error) {
	candidates := ProviderCandidatesFromModel(nextModel)
	if len(candidates) != 1 || candidates[0] == nil {
		return nil, errors.New("llm: pre-dispatch fallback requires one resolved candidate")
	}
	return withPreDispatchCandidateProposalFor(ctx, candidates[0], failedErr, reason)
}

func withPreDispatchCandidateProposalFor(ctx context.Context, nextCandidate ResolvedCandidateHandle, failedErr error, reason CandidateReason) (context.Context, error) {
	authority, active := OperationDispatchAuthorityFromContext(ctx)
	if !active {
		return nil, ErrOperationAuthorityRequired
	}
	if reason != CandidateFailover && reason != CandidateCyclerPrimary {
		return nil, errors.New("llm: invalid pre-dispatch proposal reason")
	}
	failure, ok := PreDispatchCandidateFailureFromError(failedErr)
	if !ok || failure.Class != PreDispatchCandidateUnavailable {
		return nil, errors.New("llm: pre-dispatch fallback requires resolver unavailable evidence")
	}
	failure, err := authority.RecordPreDispatchFailure(ctx, failedErr)
	if err != nil {
		return nil, fmt.Errorf("llm: record pre-dispatch candidate failure: %w", err)
	}
	if nextCandidate == nil {
		return nil, errors.New("llm: pre-dispatch fallback requires a resolved candidate")
	}
	return WithCandidateProposal(ctx, CandidateProposal{
		OperationID: authority.ID(),
		Candidate:   nextCandidate,
		Reason:      reason,
		Antecedent: CandidateAntecedent{
			Kind:        AntecedentPreDispatchFailure,
			PreDispatch: &failure,
		},
	}), nil
}

func finishProviderAttemptStream(ctx context.Context, source <-chan MessageChunk, attempt *ProviderAttempt) <-chan MessageChunk {
	if attempt == nil {
		return source
	}
	out := make(chan MessageChunk, cap(source))
	go func() {
		defer close(out)
		var usage *Usage
		var terminalErr error
		for {
			select {
			case <-ctx.Done():
				terminalErr = context.Cause(ctx)
				if terminalErr == nil {
					terminalErr = ctx.Err()
				}
				for chunk := range source {
					if chunk.Usage != nil {
						copyUsage := *chunk.Usage
						usage = &copyUsage
					}
					if chunk.Error != nil {
						terminalErr = chunk.Error
					}
				}
				if err := finishProviderAttempt(ctx, attempt, terminalErr, usage); err != nil {
					slog.Warn("llm_finish_canceled_provider_attempt", "attempt", attempt.ID(), "err", err)
				}
				return
			case chunk, ok := <-source:
				if !ok {
					if err := finishProviderAttempt(ctx, attempt, terminalErr, usage); err != nil {
						select {
						case out <- MessageChunk{Error: err}:
						case <-ctx.Done():
						}
					}
					return
				}
				if chunk.Usage != nil {
					copyUsage := *chunk.Usage
					usage = &copyUsage
				}
				if chunk.Error != nil {
					terminalErr = chunk.Error
					continue
				}
				if terminalErr != nil {
					continue
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					terminalErr = context.Cause(ctx)
					if terminalErr == nil {
						terminalErr = ctx.Err()
					}
				}
			}
		}
	}()
	return out
}
