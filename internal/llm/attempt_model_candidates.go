package llm

import (
	"context"
	"errors"
)

func selectionSourceFromModel(model BaseChatModel) CandidateSelectionSource {
	source, _ := CandidateSelectionSourceFromModel(model)
	return source
}

func (r *retryModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(r.next)
}

func (r *retryModel) CandidateSelectionSource() CandidateSelectionSource {
	return selectionSourceFromModel(r.next)
}

func (t *timeoutModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(t.next)
}

func (t *timeoutModel) CandidateSelectionSource() CandidateSelectionSource {
	return selectionSourceFromModel(t.next)
}

func (m *sanitizeModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(m.next)
}

func (m *sanitizeModel) CandidateSelectionSource() CandidateSelectionSource {
	return selectionSourceFromModel(m.next)
}

func (m *loopDetectModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(m.next)
}

func (m *loopDetectModel) CandidateSelectionSource() CandidateSelectionSource {
	return selectionSourceFromModel(m.next)
}

func (v *validateModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(v.next)
}

func (v *validateModel) CandidateSelectionSource() CandidateSelectionSource {
	return selectionSourceFromModel(v.next)
}

func (m *overflowRecoveryModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(m.inner)
}

func (m *overflowRecoveryModel) CandidateSelectionSource() CandidateSelectionSource {
	return selectionSourceFromModel(m.inner)
}

func (m *failoverModel) ProviderCandidates() []ResolvedCandidateHandle {
	var candidates []ResolvedCandidateHandle
	candidates = append(candidates, ProviderCandidatesFromModel(m.primary)...)
	for _, fallback := range m.fallbacks {
		candidates = append(candidates, ProviderCandidatesFromModel(fallback)...)
	}
	return dedupeProviderCandidates(candidates)
}

func (m *failoverModel) CandidateSelectionSource() CandidateSelectionSource {
	if source := selectionSourceFromModel(m.primary); source != nil {
		return source
	}
	candidates := ProviderCandidatesFromModel(m.primary)
	if len(candidates) == 0 {
		return nil
	}
	return fixedInitialCandidateSource{candidate: candidates[0]}
}

type fixedInitialCandidateSource struct{ candidate ResolvedCandidateHandle }

func (s fixedInitialCandidateSource) PeekCandidate(ctx context.Context) (CandidateProposal, error) {
	authority, ok := OperationDispatchAuthorityFromContext(ctx)
	if !ok || s.candidate == nil {
		return CandidateProposal{}, errors.New("llm: initial candidate authority is unavailable")
	}
	return CandidateProposal{
		OperationID: authority.ID(),
		Candidate:   s.candidate,
		Reason:      CandidateInitial,
		Antecedent:  CandidateAntecedent{Kind: AntecedentInitial},
	}, nil
}

func dedupeProviderCandidates(candidates []ResolvedCandidateHandle) []ResolvedCandidateHandle {
	seen := make(map[CandidateID]struct{}, len(candidates))
	out := make([]ResolvedCandidateHandle, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == nil || candidate.CandidateID() == "" {
			continue
		}
		if _, exists := seen[candidate.CandidateID()]; exists {
			continue
		}
		seen[candidate.CandidateID()] = struct{}{}
		out = append(out, candidate)
	}
	return out
}
