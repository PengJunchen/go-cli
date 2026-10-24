package core

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

type providerGenerationAdmissionGuard struct{ check func() error }

func (g providerGenerationAdmissionGuard) Check() error { return g.check() }
func (g providerGenerationAdmissionGuard) BeginCall(context.Context, string) (RunCapabilityInvocation, error) {
	return nil, errors.New("unexpected callback admission in provider authorization fixture")
}

func TestProviderAuthorizationChecksRunAuthorityBeforeLockForEveryCandidate(t *testing.T) {
	candidate := requestEnvelopeCandidate(t, llm.ModelInfo{Name: "authority-fixture", ContextWindow: 128000})
	for _, kind := range []llm.ProviderOperationKind{llm.OperationMainRequest, llm.OperationContinuation, llm.OperationSummary} {
		for _, reason := range []llm.CandidateReason{llm.CandidateInitial, llm.CandidateRetry, llm.CandidateFailover, llm.CandidateOverflow, llm.CandidateCyclerSelected, llm.CandidateCyclerPrimary} {
			t.Run(fmt.Sprintf("operation-%d/reason-%d", kind, reason), func(t *testing.T) {
				owner, err := (&providerOperationFactory{}).NewOperation(context.Background(), kind, 3, []llm.ResolvedCandidateHandle{candidate})
				require.NoError(t, err)
				authority := owner.(*providerOperationAuthority)
				revoked := errors.New("persisted project authorization revoked")
				checks := 0
				ctx := WithRunCapabilityGuard(context.Background(), providerGenerationAdmissionGuard{check: func() error {
					checks++
					unlocked := authority.mu.TryLock()
					require.True(t, unlocked, "store authority checks must run outside the operation lock")
					if unlocked {
						authority.mu.Unlock()
					}
					return revoked
				}})
				decision, err := authority.AuthorizeCandidate(ctx, llm.CandidateProposal{OperationID: authority.ID(), Candidate: candidate, Reason: reason, Antecedent: llm.CandidateAntecedent{Kind: llm.AntecedentInitial}})
				require.ErrorIs(t, err, revoked)
				require.ErrorIs(t, err, llm.ErrDispatchPermitDenied)
				require.False(t, decision.Authorized)
				require.Equal(t, llm.DeniedSafetyFact, decision.Denial.Code)
				require.Equal(t, 1, checks)
				require.Zero(t, authority.actualAttempts)
				require.False(t, authority.authorizationOn)
			})
		}
	}
}

func TestProviderAuthorizationRetainsCanceledDecisionWithoutStoreCheck(t *testing.T) {
	candidate := requestEnvelopeCandidate(t, llm.ModelInfo{Name: "canceled-authority-fixture", ContextWindow: 128000})
	owner, err := (&providerOperationFactory{}).NewOperation(context.Background(), llm.OperationMainRequest, 1, []llm.ResolvedCandidateHandle{candidate})
	require.NoError(t, err)
	ctx := WithRunCapabilityGuard(context.Background(), providerGenerationAdmissionGuard{check: func() error {
		t.Error("already canceled dispatch must not start a persisted-store read")
		return nil
	}})
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	decision, err := owner.AuthorizeCandidate(ctx, llm.CandidateProposal{OperationID: owner.ID(), Candidate: candidate, Reason: llm.CandidateInitial, Antecedent: llm.CandidateAntecedent{Kind: llm.AntecedentInitial}})
	require.NoError(t, err)
	require.False(t, decision.Authorized)
	require.Equal(t, llm.DeniedCancelled, decision.Denial.Code)
}
