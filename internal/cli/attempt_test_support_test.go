package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/pengjunchen/go-cli/internal/llm"
)

var cliTestOperationSequence atomic.Uint64

// withCLITestOperation installs a deliberately small test owner for tests
// that exercise an assembled resolver-built model directly. Production paths
// use the Core Loop factory; tests that need that contract run through Loop.
func withCLITestOperation(ctx context.Context) context.Context {
	sequence := cliTestOperationSequence.Add(1)
	owner := &cliTestOperationOwner{
		id:      llm.ProviderOperationID(fmt.Sprintf("cli-test-op-%d", sequence)),
		pending: make(map[llm.ProviderAttemptID]llm.CandidateID),
	}
	return llm.WithOperationDispatchAuthority(ctx, owner)
}

type cliTestOperationOwner struct {
	mu      sync.Mutex
	id      llm.ProviderOperationID
	next    uint64
	pending map[llm.ProviderAttemptID]llm.CandidateID
	results []llm.ProviderAttemptResult
}

func (o *cliTestOperationOwner) ID() llm.ProviderOperationID { return o.id }

func (o *cliTestOperationOwner) AuthorizeCandidate(_ context.Context, proposal llm.CandidateProposal) (llm.DispatchDecision, error) {
	if proposal.OperationID != o.id || proposal.Candidate == nil || proposal.Antecedent.Kind != llm.AntecedentInitial {
		return llm.DispatchDecision{Denial: &llm.DispatchDenial{Code: llm.DeniedSafetyFact}}, nil
	}
	return llm.DispatchDecision{Authorized: true}, nil
}

func (*cliTestOperationOwner) RecordPreDispatchFailure(_ context.Context, err error) (llm.PreDispatchCandidateFailure, error) {
	failure, ok := llm.PreDispatchCandidateFailureFromError(err)
	if !ok {
		return llm.PreDispatchCandidateFailure{}, errors.New("cli test operation: missing resolver pre-dispatch evidence")
	}
	return failure, nil
}

func (o *cliTestOperationOwner) BeginDispatch(permit *llm.DispatchPermit, candidate llm.ResolvedCandidateHandle) (llm.ProviderAttemptID, error) {
	if permit == nil || !permit.ValidFor(o, candidate) {
		return "", llm.ErrDispatchPermitRequired
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.next++
	id := llm.ProviderAttemptID(fmt.Sprintf("cli-test-attempt-%d-%d", cliTestOperationSequence.Load(), o.next))
	o.pending[id] = candidate.CandidateID()
	return id, nil
}

func (o *cliTestOperationOwner) FinishAttempt(result llm.ProviderAttemptResult) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	candidate, exists := o.pending[result.AttemptID]
	if !exists || result.OperationID != o.id || candidate != result.Identity.Candidate {
		return errors.New("cli test operation: mismatched provider attempt result")
	}
	delete(o.pending, result.AttemptID)
	o.results = append(o.results, result)
	return nil
}
