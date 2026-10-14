package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
)

const defaultProviderOperationAttempts = 3

type providerOperationFactory struct {
	record      func(llm.ProviderAttemptResult) error
	retryPolicy llm.RetryPolicy
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
		id:                  id,
		kind:                kind,
		maxAttempts:         maxAttempts,
		candidates:          byID,
		candidateOrder:      append([]llm.ResolvedCandidateHandle(nil), candidates...),
		pending:             make(map[llm.ProviderAttemptID]llm.CandidateID),
		finished:            make(map[llm.ProviderAttemptID]llm.ProviderAttemptResult),
		attemptsByCandidate: make(map[llm.CandidateID]int, len(candidates)),
		setupFailures:       make(map[llm.CandidateID]llm.PreDispatchCandidateFailure),
		usedSetup:           make(map[llm.CandidateID]bool),
		record:              f.record,
		retryPolicy:         f.retryPolicy,
	}, nil
}

type providerOperationAuthority struct {
	mu                  sync.Mutex
	id                  llm.ProviderOperationID
	kind                llm.ProviderOperationKind
	maxAttempts         int
	actualAttempts      int
	retryPolicy         llm.RetryPolicy
	candidates          map[llm.CandidateID]llm.ResolvedCandidateHandle
	candidateOrder      []llm.ResolvedCandidateHandle
	pending             map[llm.ProviderAttemptID]llm.CandidateID
	finished            map[llm.ProviderAttemptID]llm.ProviderAttemptResult
	attemptsByCandidate map[llm.CandidateID]int
	lastFinished        llm.ProviderAttemptID
	setupFailures       map[llm.CandidateID]llm.PreDispatchCandidateFailure
	usedSetup           map[llm.CandidateID]bool
	initialCandidate    llm.CandidateID
	initialReason       llm.CandidateReason
	planned             *llm.CandidateProposal
	authorized          llm.ResolvedCandidateHandle
	authorizationOn     bool
	overflowUsed        bool
	overflowReady       bool
	overflowCandidate   llm.CandidateID
	record              func(llm.ProviderAttemptResult) error
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
		if proposal.Reason == llm.CandidateCyclerSelected {
			lease := proposal.SelectionLease
			if lease == nil || lease.CandidateID() != candidate.CandidateID() || lease.Commit() != nil {
				return deny(llm.DeniedStaleSelectionLease)
			}
		} else if proposal.SelectionLease != nil {
			return deny(llm.DeniedStaleSelectionLease)
		}
		a.authorized = candidate
		a.authorizationOn = true
		if a.initialCandidate == "" {
			a.initialCandidate = candidate.CandidateID()
			a.initialReason = proposal.Reason
		}
		return llm.DispatchDecision{Authorized: true}, nil
	}
	if proposal.Antecedent.Kind == llm.AntecedentPreDispatchFailure {
		failure := proposal.Antecedent.PreDispatch
		if failure == nil || proposal.Antecedent.AttemptID != "" {
			return deny(llm.DeniedStalePrevious)
		}
		stored, ok := a.setupFailures[failure.FailedCandidateID]
		if !a.matchesPlannedProposal(proposal) || !ok || a.usedSetup[failure.FailedCandidateID] || stored != *failure ||
			failure.Class != llm.PreDispatchCandidateUnavailable ||
			(proposal.Reason != llm.CandidateFailover && proposal.Reason != llm.CandidateCyclerPrimary) {
			return deny(llm.DeniedRepeatedPreDispatchFailure)
		}
		if failure.PreviousAttemptID != a.lastFinished {
			return deny(llm.DeniedStalePrevious)
		}
		a.usedSetup[failure.FailedCandidateID] = true
		a.planned = nil
		a.authorized = candidate
		a.authorizationOn = true
		return llm.DispatchDecision{Authorized: true}, nil
	}
	if proposal.Antecedent.Kind != llm.AntecedentFinishedAttempt && proposal.Antecedent.Kind != llm.AntecedentFinishedOverflow {
		return deny(llm.DeniedStalePrevious)
	}
	if !a.matchesPlannedProposal(proposal) {
		return deny(llm.DeniedSafetyFact)
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
	a.planned = nil
	a.authorized = candidate
	a.authorizationOn = true
	return llm.DispatchDecision{Authorized: true}, nil
}

// ActualDispatchCount reports the number of calls that crossed BeginDispatch.
func (a *providerOperationAuthority) ActualDispatchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.actualAttempts
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
	if a.actualAttempts >= a.maxAttempts || len(a.pending) != 0 || a.authorizationOn {
		return &llm.DispatchDeniedError{Code: llm.DeniedBudget}
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

func (a *providerOperationAuthority) nextCandidateProposal(ctx context.Context, err error) (llm.CandidateProposal, time.Duration, bool) {
	if ctx == nil || ctx.Err() != nil || llm.ContinuationNoReplay(ctx) {
		return llm.CandidateProposal{}, 0, false
	}
	if failure, ok := llm.PreDispatchCandidateFailureFromError(err); ok {
		if failure.Class != llm.PreDispatchCandidateUnavailable {
			return llm.CandidateProposal{}, 0, false
		}
		failure, recordErr := a.RecordPreDispatchFailure(ctx, err)
		if recordErr != nil {
			return llm.CandidateProposal{}, 0, false
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.actualAttempts >= a.maxAttempts || len(a.pending) != 0 || a.authorizationOn {
			return llm.CandidateProposal{}, 0, false
		}
		candidate, reason, found := a.nextUnvisitedCandidateLocked(failure.FailedCandidateID)
		if !found {
			return llm.CandidateProposal{}, 0, false
		}
		proposal := llm.CandidateProposal{
			OperationID: a.id,
			Candidate:   candidate,
			Reason:      reason,
			Antecedent: llm.CandidateAntecedent{
				Kind:        llm.AntecedentPreDispatchFailure,
				PreDispatch: &failure,
			},
		}
		a.setPlannedProposalLocked(proposal)
		return proposal, 0, true
	}

	result, finished := llm.FinishedProviderAttemptFromError(err)
	if !finished || result.OperationID == "" || result.AttemptID == "" {
		return llm.CandidateProposal{}, 0, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx.Err() != nil || a.actualAttempts >= a.maxAttempts || len(a.pending) != 0 || a.authorizationOn || result.OperationID != a.id || result.AttemptID != a.lastFinished || result.Failure != llm.FailureTransientProvider {
		return llm.CandidateProposal{}, 0, false
	}
	finishedResult, exists := a.finished[result.AttemptID]
	if !exists || finishedResult.AttemptID != result.AttemptID || finishedResult.OperationID != result.OperationID ||
		finishedResult.Identity != result.Identity || finishedResult.Outcome != result.Outcome ||
		finishedResult.Failure != result.Failure || result.Identity.Candidate == "" {
		return llm.CandidateProposal{}, 0, false
	}
	candidate, exists := a.candidates[result.Identity.Candidate]
	if !exists || candidate.Identity() != result.Identity {
		return llm.CandidateProposal{}, 0, false
	}
	if a.initialReason == llm.CandidateCyclerSelected && len(a.candidateOrder) > 0 {
		primary := a.candidateOrder[0]
		if primary.CandidateID() != result.Identity.Candidate && a.attemptsByCandidate[primary.CandidateID()] == 0 && !a.usedSetup[primary.CandidateID()] {
			proposal := llm.CandidateProposal{
				OperationID: a.id,
				Candidate:   primary,
				Reason:      llm.CandidateCyclerPrimary,
				Antecedent:  llm.CandidateAntecedent{Kind: llm.AntecedentFinishedAttempt, AttemptID: result.AttemptID},
			}
			a.setPlannedProposalLocked(proposal)
			return proposal, 0, true
		}
	}
	attemptIndex := a.attemptsByCandidate[result.Identity.Candidate] - 1
	policyErr := err
	if ctx.Err() == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		policyErr = errors.New("provider transport timeout")
	}
	if a.retryPolicy != nil && a.retryPolicy.ShouldRetry(ctx, policyErr, attemptIndex) {
		proposal := llm.CandidateProposal{
			OperationID: a.id,
			Candidate:   candidate,
			Reason:      llm.CandidateRetry,
			Antecedent:  llm.CandidateAntecedent{Kind: llm.AntecedentFinishedAttempt, AttemptID: result.AttemptID},
		}
		a.setPlannedProposalLocked(proposal)
		return proposal, a.retryPolicy.NextBackoff(ctx, attemptIndex), true
	}
	next, reason, found := a.nextUnvisitedCandidateLocked(result.Identity.Candidate)
	if !found {
		return llm.CandidateProposal{}, 0, false
	}
	proposal := llm.CandidateProposal{
		OperationID: a.id,
		Candidate:   next,
		Reason:      reason,
		Antecedent:  llm.CandidateAntecedent{Kind: llm.AntecedentFinishedAttempt, AttemptID: result.AttemptID},
	}
	a.setPlannedProposalLocked(proposal)
	return proposal, 0, true
}

func (a *providerOperationAuthority) nextUnvisitedCandidateLocked(failed llm.CandidateID) (llm.ResolvedCandidateHandle, llm.CandidateReason, bool) {
	unvisited := func(candidate llm.ResolvedCandidateHandle) bool {
		if candidate == nil || a.attemptsByCandidate[candidate.CandidateID()] != 0 || a.usedSetup[candidate.CandidateID()] {
			return false
		}
		_, failedSetup := a.setupFailures[candidate.CandidateID()]
		return !failedSetup
	}
	if a.initialReason == llm.CandidateCyclerSelected && len(a.candidateOrder) > 0 && a.candidateOrder[0].CandidateID() != failed && unvisited(a.candidateOrder[0]) {
		return a.candidateOrder[0], llm.CandidateCyclerPrimary, true
	}
	index := -1
	for i, candidate := range a.candidateOrder {
		if candidate != nil && candidate.CandidateID() == failed {
			index = i
			break
		}
	}
	if a.actualAttempts == 0 && index > 0 && unvisited(a.candidateOrder[0]) {
		return a.candidateOrder[0], llm.CandidateCyclerPrimary, true
	}
	for i := index + 1; i < len(a.candidateOrder); i++ {
		if unvisited(a.candidateOrder[i]) {
			return a.candidateOrder[i], llm.CandidateFailover, true
		}
	}
	return nil, 0, false
}

func (a *providerOperationAuthority) planCandidateProposal(proposal llm.CandidateProposal) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if proposal.OperationID != a.id || proposal.Candidate == nil || a.candidates[proposal.Candidate.CandidateID()] != proposal.Candidate {
		return errors.New("core: cannot plan a candidate outside the provider operation")
	}
	if a.actualAttempts >= a.maxAttempts || len(a.pending) != 0 || a.authorizationOn {
		return &llm.DispatchDeniedError{Code: llm.DeniedBudget}
	}
	a.setPlannedProposalLocked(proposal)
	return nil
}

func (a *providerOperationAuthority) setPlannedProposalLocked(proposal llm.CandidateProposal) {
	copy := proposal
	if proposal.Antecedent.PreDispatch != nil {
		failure := *proposal.Antecedent.PreDispatch
		copy.Antecedent.PreDispatch = &failure
	}
	a.planned = &copy
}

func (a *providerOperationAuthority) matchesPlannedProposal(proposal llm.CandidateProposal) bool {
	if a.planned == nil || a.planned.OperationID != proposal.OperationID || a.planned.Candidate != proposal.Candidate || a.planned.Reason != proposal.Reason {
		return false
	}
	plannedAntecedent := a.planned.Antecedent
	if plannedAntecedent.Kind != proposal.Antecedent.Kind || plannedAntecedent.AttemptID != proposal.Antecedent.AttemptID {
		return false
	}
	if plannedAntecedent.PreDispatch == nil || proposal.Antecedent.PreDispatch == nil {
		return plannedAntecedent.PreDispatch == nil && proposal.Antecedent.PreDispatch == nil
	}
	return *plannedAntecedent.PreDispatch == *proposal.Antecedent.PreDispatch
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
	a.attemptsByCandidate[candidateID]++
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
