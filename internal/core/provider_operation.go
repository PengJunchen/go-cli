package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/pengjunchen/go-cli/internal/llm"
)

const defaultProviderOperationAttempts = 3

type providerOperationFactory struct {
	record func(llm.ProviderAttemptResult) error
}

func (f *providerOperationFactory) NewOperation(_ context.Context, kind llm.ProviderOperationKind, maxAttempts int, candidates []llm.ResolvedCandidateHandle) (llm.OperationDispatchAuthority, error) {
	if maxAttempts <= 0 {
		maxAttempts = defaultProviderOperationAttempts
	}
	if maxAttempts > defaultProviderOperationAttempts {
		maxAttempts = defaultProviderOperationAttempts
	}
	if kind < llm.OperationMainRequest || kind > llm.OperationSummary {
		return nil, errors.New("core: invalid provider operation kind")
	}
	if len(candidates) == 0 {
		return nil, errors.New("core: provider operation requires resolver candidates")
	}
	id, err := newProviderOperationID()
	if err != nil {
		return nil, err
	}
	byID := make(map[llm.CandidateID]llm.ResolvedCandidateHandle, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.CandidateID() == "" {
			return nil, errors.New("core: provider operation contains an invalid candidate")
		}
		if _, exists := byID[candidate.CandidateID()]; exists {
			return nil, errors.New("core: provider operation candidate IDs must be unique")
		}
		byID[candidate.CandidateID()] = candidate
	}
	return &providerOperationAuthority{
		id:            id,
		kind:          kind,
		maxAttempts:   maxAttempts,
		candidates:    byID,
		pending:       make(map[llm.ProviderAttemptID]llm.CandidateID),
		finished:      make(map[llm.ProviderAttemptID]llm.ProviderAttemptResult),
		setupFailures: make(map[llm.CandidateID]llm.PreDispatchCandidateFailure),
		usedSetup:     make(map[llm.CandidateID]bool),
		record:        f.record,
	}, nil
}

type providerOperationAuthority struct {
	mu                sync.Mutex
	id                llm.ProviderOperationID
	kind              llm.ProviderOperationKind
	maxAttempts       int
	actualAttempts    int
	candidates        map[llm.CandidateID]llm.ResolvedCandidateHandle
	pending           map[llm.ProviderAttemptID]llm.CandidateID
	finished          map[llm.ProviderAttemptID]llm.ProviderAttemptResult
	lastFinished      llm.ProviderAttemptID
	setupFailures     map[llm.CandidateID]llm.PreDispatchCandidateFailure
	usedSetup         map[llm.CandidateID]bool
	authorized        llm.ResolvedCandidateHandle
	authorizationOn   bool
	overflowUsed      bool
	overflowReady     bool
	overflowCandidate llm.CandidateID
	record            func(llm.ProviderAttemptResult) error
}

func (a *providerOperationAuthority) ID() llm.ProviderOperationID { return a.id }

func (a *providerOperationAuthority) AuthorizeCandidate(ctx context.Context, proposal llm.CandidateProposal) (llm.DispatchDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	deny := func(code llm.DispatchDenialCode) (llm.DispatchDecision, error) {
		a.authorized = nil
		a.authorizationOn = false
		return llm.DispatchDecision{Denial: &llm.DispatchDenial{Code: code}}, nil
	}
	if err := ctx.Err(); err != nil {
		return deny(llm.DeniedCancelled)
	}
	if proposal.OperationID != a.id || proposal.Candidate == nil {
		return deny(llm.DeniedCandidateMismatch)
	}
	candidate, exists := a.candidates[proposal.Candidate.CandidateID()]
	if !exists || candidate != proposal.Candidate {
		return deny(llm.DeniedCandidateOutsideSet)
	}
	if a.authorizationOn || len(a.pending) != 0 {
		return deny(llm.DeniedStalePrevious)
	}
	if a.actualAttempts >= a.maxAttempts {
		return deny(llm.DeniedBudget)
	}
	if a.kind == llm.OperationContinuation && proposal.Antecedent.Kind != llm.AntecedentInitial {
		return deny(llm.DeniedReplayForbidden)
	}
	if proposal.Antecedent.Kind == llm.AntecedentInitial {
		if proposal.Antecedent.AttemptID != "" || proposal.Antecedent.PreDispatch != nil ||
			proposal.Reason != llm.CandidateInitial && proposal.Reason != llm.CandidateCyclerSelected ||
			a.actualAttempts != 0 || a.lastFinished != "" || len(a.setupFailures) != 0 {
			return deny(llm.DeniedStalePrevious)
		}
		a.authorized = candidate
		a.authorizationOn = true
		return llm.DispatchDecision{Authorized: true}, nil
	}
	if proposal.Antecedent.Kind == llm.AntecedentPreDispatchFailure {
		failure := proposal.Antecedent.PreDispatch
		if failure == nil || proposal.Antecedent.AttemptID != "" {
			return deny(llm.DeniedStalePrevious)
		}
		stored, ok := a.setupFailures[failure.FailedCandidateID]
		if !ok || a.usedSetup[failure.FailedCandidateID] || stored != *failure ||
			failure.Class != llm.PreDispatchCandidateUnavailable ||
			(proposal.Reason != llm.CandidateFailover && proposal.Reason != llm.CandidateCyclerPrimary) {
			return deny(llm.DeniedRepeatedPreDispatchFailure)
		}
		if failure.PreviousAttemptID != a.lastFinished {
			return deny(llm.DeniedStalePrevious)
		}
		a.usedSetup[failure.FailedCandidateID] = true
		a.authorized = candidate
		a.authorizationOn = true
		return llm.DispatchDecision{Authorized: true}, nil
	}
	if proposal.Antecedent.Kind != llm.AntecedentFinishedAttempt && proposal.Antecedent.Kind != llm.AntecedentFinishedOverflow {
		return deny(llm.DeniedStalePrevious)
	}
	if proposal.Antecedent.AttemptID == "" || proposal.Antecedent.PreDispatch != nil || proposal.Antecedent.AttemptID != a.lastFinished {
		return deny(llm.DeniedStalePrevious)
	}
	previous, ok := a.finished[proposal.Antecedent.AttemptID]
	if !ok || previous.Outcome == llm.AttemptSuccess || previous.Failure == llm.FailureCancelled {
		return deny(llm.DeniedStalePrevious)
	}
	switch proposal.Reason {
	case llm.CandidateRetry:
		if previous.Failure != llm.FailureTransientProvider || proposal.Antecedent.Kind != llm.AntecedentFinishedAttempt || candidate.CandidateID() != previous.Identity.Candidate {
			return deny(llm.DeniedSafetyFact)
		}
	case llm.CandidateFailover, llm.CandidateCyclerPrimary:
		if previous.Failure != llm.FailureTransientProvider || proposal.Antecedent.Kind != llm.AntecedentFinishedAttempt || candidate.CandidateID() == previous.Identity.Candidate {
			return deny(llm.DeniedSafetyFact)
		}
	case llm.CandidateOverflow:
		if previous.Failure != llm.FailureOverflow || proposal.Antecedent.Kind != llm.AntecedentFinishedOverflow || candidate.CandidateID() != previous.Identity.Candidate {
			return deny(llm.DeniedSafetyFact)
		}
		if !a.overflowUsed || !a.overflowReady || a.overflowCandidate != candidate.CandidateID() {
			return deny(llm.DeniedOverflowRecoveryUsed)
		}
		a.overflowReady = false
	default:
		return deny(llm.DeniedSafetyFact)
	}
	a.authorized = candidate
	a.authorizationOn = true
	return llm.DispatchDecision{Authorized: true}, nil
}

func (a *providerOperationAuthority) BeginOverflowRecovery(ctx context.Context, attemptID llm.ProviderAttemptID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kind != llm.OperationMainRequest || attemptID == "" || a.overflowUsed || a.lastFinished != attemptID {
		return errors.New("core: overflow recovery requires the latest main-request attempt and may be used once")
	}
	result, ok := a.finished[attemptID]
	if !ok || result.Outcome == llm.AttemptSuccess || result.Failure != llm.FailureOverflow || result.Identity.Candidate == "" {
		return errors.New("core: overflow recovery requires a finished typed context-limit attempt")
	}
	a.overflowUsed = true
	a.overflowReady = true
	a.overflowCandidate = result.Identity.Candidate
	return nil
}

func (a *providerOperationAuthority) RecordPreDispatchFailure(ctx context.Context, err error) (llm.PreDispatchCandidateFailure, error) {
	if err := ctx.Err(); err != nil {
		return llm.PreDispatchCandidateFailure{}, err
	}
	failure, ok := llm.PreDispatchCandidateFailureFromError(err)
	if !ok {
		return llm.PreDispatchCandidateFailure{}, errors.New("core: pre-dispatch failure lacks resolver evidence")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.candidates[failure.FailedCandidateID]; !exists {
		return llm.PreDispatchCandidateFailure{}, errors.New("core: pre-dispatch candidate is outside the operation set")
	}
	if _, exists := a.setupFailures[failure.FailedCandidateID]; exists {
		return llm.PreDispatchCandidateFailure{}, errors.New("core: pre-dispatch candidate failure already recorded")
	}
	failure.PreviousAttemptID = a.lastFinished
	a.setupFailures[failure.FailedCandidateID] = failure
	return failure, nil
}

func (a *providerOperationAuthority) BeginDispatch(permit *llm.DispatchPermit, candidate llm.ResolvedCandidateHandle) (llm.ProviderAttemptID, error) {
	if permit == nil || !permit.ValidFor(a, candidate) {
		return "", llm.ErrDispatchPermitRequired
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.authorizationOn || a.authorized != candidate {
		return "", llm.ErrDispatchPermitDenied
	}
	a.authorizationOn = false
	a.authorized = nil
	if a.actualAttempts >= a.maxAttempts {
		return "", &llm.DispatchDeniedError{Code: llm.DeniedBudget}
	}
	if len(a.pending) != 0 {
		return "", &llm.DispatchDeniedError{Code: llm.DeniedStalePrevious}
	}
	id, err := newProviderAttemptID()
	if err != nil {
		return "", err
	}
	a.actualAttempts++
	a.pending[id] = candidate.CandidateID()
	return id, nil
}

func (a *providerOperationAuthority) FinishAttempt(result llm.ProviderAttemptResult) error {
	a.mu.Lock()
	candidateID, exists := a.pending[result.AttemptID]
	if !exists || result.OperationID != a.id || result.Identity.Candidate != candidateID {
		a.mu.Unlock()
		return errors.New("core: provider attempt result does not match a pending dispatch")
	}
	candidate := a.candidates[candidateID]
	if candidate == nil || candidate.Identity() != result.Identity {
		a.mu.Unlock()
		return errors.New("core: provider attempt identity differs from its resolved candidate")
	}
	if _, duplicate := a.finished[result.AttemptID]; duplicate {
		a.mu.Unlock()
		return errors.New("core: provider attempt result was already finished")
	}
	delete(a.pending, result.AttemptID)
	result = cloneCoreAttemptResult(result)
	a.finished[result.AttemptID] = result
	a.lastFinished = result.AttemptID
	record := a.record
	a.mu.Unlock()
	if record != nil {
		if err := record(cloneCoreAttemptResult(result)); err != nil {
			return fmt.Errorf("core: record provider attempt: %w", err)
		}
	}
	return nil
}

func cloneCoreAttemptResult(result llm.ProviderAttemptResult) llm.ProviderAttemptResult {
	clone := result
	if result.Usage.Usage != nil {
		usage := *result.Usage.Usage
		clone.Usage.Usage = &usage
	}
	if result.ProviderReportedCostUSD != nil {
		cost := *result.ProviderReportedCostUSD
		clone.ProviderReportedCostUSD = &cost
	}
	return clone
}

func newProviderOperationID() (llm.ProviderOperationID, error) {
	value, err := newProviderAttemptNonce()
	if err != nil {
		return "", fmt.Errorf("core: create provider operation ID: %w", err)
	}
	return llm.ProviderOperationID("op-" + value), nil
}

func newProviderAttemptID() (llm.ProviderAttemptID, error) {
	value, err := newProviderAttemptNonce()
	if err != nil {
		return "", fmt.Errorf("core: create provider attempt ID: %w", err)
	}
	return llm.ProviderAttemptID("attempt-" + value), nil
}

func newProviderAttemptNonce() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
