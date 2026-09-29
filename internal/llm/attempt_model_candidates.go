package llm

func (r *retryModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(r.next)
}

func (t *timeoutModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(t.next)
}

func (m *sanitizeModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(m.next)
}

func (m *loopDetectModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(m.next)
}

func (v *validateModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(v.next)
}

func (m *overflowRecoveryModel) ProviderCandidates() []ResolvedCandidateHandle {
	return ProviderCandidatesFromModel(m.inner)
}

func (m *failoverModel) ProviderCandidates() []ResolvedCandidateHandle {
	var candidates []ResolvedCandidateHandle
	candidates = append(candidates, ProviderCandidatesFromModel(m.primary)...)
	for _, fallback := range m.fallbacks {
		candidates = append(candidates, ProviderCandidatesFromModel(fallback)...)
	}
	return dedupeProviderCandidates(candidates)
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
