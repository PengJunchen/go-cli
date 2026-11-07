package compaction

import (
	"errors"
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
		if err := ValidateImageGroupCut(items, cut); err != nil {
			if errors.Is(err, ErrImageCompactionBlocked) {
				return truncateRetainingImageExchanges(items, groups, currentIndex, runID, maxTokens, estimator)
			}
			return CompactionResult{Items: items}, err
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

	return truncateWithRetainedInput(items, groups, currentIndex, runID, maxTokens, estimator, suffixTokens, markerTokens)
}

func truncateWithRetainedInput(items []TurnItem, groups []itemGroup, currentIndex int, runID string, maxTokens int, estimator TokenEstimator, suffixTokens []int, markerTokens int) (CompactionResult, error) {
	anchor := items[currentIndex]
	if anchor.Role != RoleUser || anchor.IsCompaction || anchor.Runtime.Source.Kind != llm.SourceRunTranscript || anchor.Runtime.Source.TranscriptIndex != 0 {
		return CompactionResult{Items: items}, fmt.Errorf("%w: truncation requires the original user input", ErrInvalidRunRequestHistory)
	}
	anchorTokens := estimateTokens(items[currentIndex:currentIndex+1], estimator)
	var systemTokens int
	var previous llm.SourceThrough
	for _, item := range items {
		if item.Runtime.Source.Kind == llm.SourceRunSummary {
			previous = item.Runtime.Source.Through
		}
	}
	for groupIndex, group := range groups {
		if group.system {
			if items[group.start].Runtime.Source.Kind != llm.SourceEphemeral {
				return CompactionResult{Items: items}, fmt.Errorf("%w: truncation cannot cover source-backed system instructions", ErrRunHistoryNotCompactionEligible)
			}
			systemTokens = saturatingTokenAdd(systemTokens, estimateTokens(items[group.start:group.end], estimator))
		}
		cut := group.end
		if cut <= currentIndex+1 || saturatingTokenAdd(saturatingTokenAdd(saturatingTokenAdd(systemTokens, anchorTokens), markerTokens), suffixTokens[groupIndex+1]) > maxTokens {
			continue
		}
		if err := ValidateImageGroupCut(items, cut); err != nil {
			if errors.Is(err, ErrImageCompactionBlocked) {
				return truncateRetainingImageExchanges(items, groups, currentIndex, runID, maxTokens, estimator)
			}
			return CompactionResult{Items: items}, err
		}
		through, _, err := sourceThroughForPrefix(items[:cut])
		if err != nil {
			return CompactionResult{Items: items}, err
		}
		if through.TranscriptCount < 2 || through.PriorHistoryCount < previous.PriorHistoryCount || through.TranscriptCount < previous.TranscriptCount || through == previous {
			continue
		}
		kept := make([]TurnItem, 0, len(items)-cut+3)
		for _, item := range items[:cut] {
			if item.Runtime.Source.Kind == llm.SourceEphemeral {
				if item.Role != RoleSystem || item.IsCompaction {
					return CompactionResult{Items: items}, fmt.Errorf("%w: truncation cannot omit ephemeral non-system input", ErrRunHistoryNotCompactionEligible)
				}
				kept = append(kept, item)
			}
		}
		kept = append(kept, anchor, TurnItem{
			Role: RoleUser, Content: DurableTruncationMarker, IsCompaction: true,
			Runtime: llm.RuntimeMetadata{CompactionMarker: true, Source: llm.SourceRef{
				Kind: llm.SourceRunSummary, RunID: runID, Through: through, RetainCurrentInput: true,
			}},
		})
		kept = append(kept, items[cut:]...)
		if err := ValidateRunRequestIntegrity(kept); err != nil {
			return CompactionResult{Items: items}, err
		}
		if estimateTokens(kept, estimator) > maxTokens {
			return CompactionResult{Items: items}, fmt.Errorf("%w: anchored truncation exceeds its estimated budget", ErrRequiresTruncating)
		}
		return CompactionResult{Items: kept, Strategy: StrategyTruncating, NewProjection: &NewProjection{
			Summary: DurableTruncationMarker, Through: through, RetainCurrentInput: true,
		}}, nil
	}
	for _, group := range groups {
		for _, item := range items[group.start:group.end] {
			if itemHasImage(item) && group.start != currentIndex {
				return truncateRetainingImageExchanges(items, groups, currentIndex, runID, maxTokens, estimator)
			}
		}
	}
	return CompactionResult{Items: items}, fmt.Errorf("%w: truncation cannot fit the omission marker, mandatory systems and protected current input within budget", ErrRequiresTruncating)
}
