package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
	// FailureCancelled means the attempt ended through cancellation.
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

// Origin and candidate provenance are safe to pass to accounting and diagnostics.
// They deliberately omit endpoint, credential, headers, and prompt data.
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
	// DeniedCancelled means the operation context is cancelled.
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

// DispatchDecision carries either a one-shot permit or a typed policy denial.
type DispatchDecision struct {
	Permit DispatchPermit
	Denial *DispatchDenial
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

// DispatchPermit is a one-shot authorization for one operation and candidate.
// The operation owner validates its private ownership binding at BeginDispatch.
type DispatchPermit interface {
	OperationID() ProviderOperationID
	CandidateID() CandidateID
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
// alone authorizes provider dispatch proposals.
type OperationDispatchAuthority interface {
	ID() ProviderOperationID
	AuthorizeCandidate(context.Context, CandidateProposal) (DispatchDecision, error)
}

// RunOperationFactory creates independent main, continuation, or summary
// operation authorities backed by the run's accounting sink.
type RunOperationFactory interface {
	NewOperation(context.Context, ProviderOperationKind, int, []ResolvedCandidateHandle) (OperationDispatchAuthority, error)
}

// ProviderDispatchAuthority consumes a permit and allocates an AttemptID at
// the adapter's actual client.Do boundary.
type ProviderDispatchAuthority interface {
	BeginDispatch(DispatchPermit, ResolvedCandidateHandle) (ProviderAttempt, error)
}

// ProviderAttempt accepts exactly one terminal result for a dispatched call.
type ProviderAttempt interface {
	ID() ProviderAttemptID
	Finish(ProviderAttemptTerminal) (ProviderAttemptResult, error)
}

// PreDispatchCandidateError describes a typed candidate setup failure. It does
// not represent a provider attempt and therefore has no AttemptID.
type PreDispatchCandidateError struct {
	CandidateID CandidateID
	Class       PreDispatchFailureClass
	Err         error
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

var (
	// ErrOperationAuthorityRequired is returned when a guarded model has no Loop authority.
	ErrOperationAuthorityRequired = errors.New("llm: guarded provider model requires an operation authority")
	// ErrProviderDispatchAuthorityRequired is returned when no dispatch owner can consume a permit.
	ErrProviderDispatchAuthorityRequired = errors.New("llm: guarded provider model requires a dispatch authority")
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
	dispatchPermitContextKey
	candidateProposalContextKey
)

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
func WithOperationDispatchAuthority(ctx context.Context, operation OperationDispatchAuthority) context.Context {
	return context.WithValue(ctx, operationDispatchAuthorityContextKey, operation)
}

// OperationDispatchAuthorityFromContext returns the current provider operation owner.
func OperationDispatchAuthorityFromContext(ctx context.Context) (OperationDispatchAuthority, bool) {
	operation, ok := ctx.Value(operationDispatchAuthorityContextKey).(OperationDispatchAuthority)
	return operation, ok && operation != nil
}

// WithDispatchPermit attaches the one-shot permit for the downstream candidate.
func WithDispatchPermit(ctx context.Context, permit DispatchPermit) context.Context {
	return context.WithValue(ctx, dispatchPermitContextKey, permit)
}

// DispatchPermitFromContext returns a permit attached by an authorized operation path.
func DispatchPermitFromContext(ctx context.Context) (DispatchPermit, bool) {
	permit, ok := ctx.Value(dispatchPermitContextKey).(DispatchPermit)
	return permit, ok && permit != nil
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
		return nil, nil, ErrGuardedCandidateUnavailable
	}
	return builder.buildGuarded(ctx, c.cfg, c)
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

func (b providerDispatchBinding) begin(ctx context.Context) (ProviderAttempt, error) {
	if b.mode == UnboundLibraryBuild {
		return nil, nil
	}
	if b.mode != GuardedCandidateBuild || b.candidate == nil || b.candidate.id == "" {
		return nil, ErrGuardedCandidateUnavailable
	}
	operation, ok := OperationDispatchAuthorityFromContext(ctx)
	if !ok || operation.ID() == "" {
		return nil, ErrOperationAuthorityRequired
	}
	providerAuthority, ok := operation.(ProviderDispatchAuthority)
	if !ok {
		return nil, ErrProviderDispatchAuthorityRequired
	}

	proposal, hasProposal := CandidateProposalFromContext(ctx)
	permit, hasPermit := DispatchPermitFromContext(ctx)
	if hasProposal {
		if proposal.OperationID != operation.ID() || proposal.Candidate == nil || proposal.Candidate.CandidateID() != b.candidate.id {
			return nil, ErrDispatchPermitRequired
		}
	} else if !hasPermit {
		proposal = CandidateProposal{
			OperationID: operation.ID(),
			Candidate:   b.candidate,
			Reason:      CandidateInitial,
			Antecedent:  CandidateAntecedent{Kind: AntecedentInitial},
		}
	}

	if !hasPermit {
		decision, err := operation.AuthorizeCandidate(ctx, proposal)
		if err != nil {
			return nil, fmt.Errorf("llm: authorize provider candidate: %w", err)
		}
		if decision.Denial != nil {
			if decision.Permit != nil {
				return nil, ErrInvalidDispatchDecision
			}
			return nil, &DispatchDeniedError{Code: decision.Denial.Code}
		}
		if decision.Permit == nil {
			return nil, ErrInvalidDispatchDecision
		}
		permit = decision.Permit
	}
	if permit.OperationID() != operation.ID() || permit.CandidateID() != b.candidate.id {
		return nil, ErrDispatchPermitRequired
	}
	attempt, err := providerAuthority.BeginDispatch(permit, b.candidate)
	if err != nil {
		return nil, fmt.Errorf("llm: begin provider dispatch: %w", err)
	}
	if attempt == nil || attempt.ID() == "" {
		return nil, errors.New("llm: dispatch authority returned an invalid provider attempt")
	}
	return attempt, nil
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
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ProviderAttemptTerminal{Outcome: AttemptCancelled, Failure: FailureCancelled, Usage: evidence}
	}
	var transportErr *providerTransportFailure
	if errors.As(err, &transportErr) {
		return ProviderAttemptTerminal{Outcome: AttemptTransportFailure, Failure: FailureTransientProvider, Usage: evidence}
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

func finishProviderAttempt(ctx context.Context, attempt ProviderAttempt, err error, usage *Usage) error {
	if attempt == nil {
		return err
	}
	result, finishErr := attempt.Finish(providerAttemptTerminal(ctx, err, usage))
	if finishErr != nil {
		return errors.Join(err, fmt.Errorf("llm: finish provider attempt %s: %w", attempt.ID(), finishErr))
	}
	if result.AttemptID != attempt.ID() {
		return errors.Join(err, errors.New("llm: provider attempt returned a mismatched terminal result"))
	}
	return err
}

func finishProviderAttemptStream(ctx context.Context, source <-chan MessageChunk, attempt ProviderAttempt) <-chan MessageChunk {
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
					slog.Warn("llm_finish_cancelled_provider_attempt", "attempt", attempt.ID(), "err", err)
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
