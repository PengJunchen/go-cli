package compaction

import (
	"fmt"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// DurableTruncationMarker describes an omission, never a factual summary. A
// durable truncating projection may carry only this fixed body.
const DurableTruncationMarker = "[Earlier conversation omitted to fit the context budget; no semantic summary was generated.]"

func truncateDurablePrefix(items []TurnItem, groups []itemGroup, maxTokens int, estimator TokenEstimator) (CompactionResult, error) {
	if estimateTokens(items, estimator) <= maxTokens {
		return CompactionResult{Items: items, Strategy: StrategyTruncating}, nil
	}
	runID := currentRunID(items)
	if runID == "" {
		return CompactionResult{Items: items}, fmt.Errorf("%w: truncation requires current run source identity", ErrInvalidRunRequestHistory)
	}
	currentIndex := -1
	for index, item := range items {
		source := item.Runtime.Source
		if source.Kind == llm.SourceRunTranscript && source.RunID == runID && source.TranscriptIndex == 0 {
			currentIndex = index
			break
		}
	}
	if currentIndex < 0 {
		return CompactionResult{Items: items}, fmt.Errorf("%w: truncation requires the original current input", ErrInvalidRunRequestHistory)
	}
	// Price each closed group once, then find the smallest removable prefix.
	// Revalidating and rebuilding every candidate would be quadratic for a
	// large imported history. Only the winning boundary needs a source cursor.
	suffixTokens := make([]int, len(groups)+1)
	for index := len(groups) - 1; index >= 0; index-- {
		group := groups[index]
		suffixTokens[index] = saturatingTokenAdd(suffixTokens[index+1], estimateTokens(items[group.start:group.end], estimator))
	}
	marker := TurnItem{Role: RoleUser, Content: DurableTruncationMarker, IsCompaction: true,
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: runID}}}
	markerTokens := estimateTokens([]TurnItem{marker}, estimator)
	prefixSystemTokens := 0
	prefixHasSource := false
	prefixAdvances := false
	for groupIndex, group := range groups {
		cut := group.end
		if cut > currentIndex {
			break
		}
		for _, item := range items[group.start:group.end] {
			kind := item.Runtime.Source.Kind
			if kind != llm.SourceNone && kind != llm.SourceEphemeral {
				prefixHasSource = true
				prefixAdvances = kind != llm.SourceRunSummary
			}
		}
		if group.system {
			if items[group.start].Runtime.Source.Kind != llm.SourceEphemeral {
				return CompactionResult{Items: items}, fmt.Errorf("%w: truncation cannot cover source-backed system instructions", ErrRunHistoryNotCompactionEligible)
			}
			prefixSystemTokens = saturatingTokenAdd(prefixSystemTokens, estimateTokens(items[group.start:group.end], estimator))
		}
		if !prefixHasSource || !prefixAdvances || saturatingTokenAdd(saturatingTokenAdd(prefixSystemTokens, markerTokens), suffixTokens[groupIndex+1]) > maxTokens {
			continue
		}
		old := items[:cut]
		through, _, err := sourceThroughForPrefix(old)
		if err != nil {
			return CompactionResult{Items: items}, err
		}
		kept := make([]TurnItem, 0, len(items)-cut+2)
		for _, item := range old {
			if item.Role == RoleSystem && !item.IsCompaction {
				kept = append(kept, item)
			}
		}
		marker.Runtime.Source.Through = through
		kept = append(kept, marker)
		kept = append(kept, items[cut:]...)
		if estimateTokens(kept, estimator) > maxTokens {
			return CompactionResult{Items: items}, fmt.Errorf("%w: truncation result exceeds its estimated budget", ErrRequiresTruncating)
		}
		return CompactionResult{Items: kept, Strategy: StrategyTruncating, NewProjection: &NewProjection{Summary: DurableTruncationMarker, Through: through}}, nil
	}

	return CompactionResult{Items: items}, fmt.Errorf("%w: truncation cannot fit the omission marker, mandatory systems and complete current run within budget", ErrRequiresTruncating)
}
