package core

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

var errDurableCompactionProjectionInvalid = errors.New("core: durable compaction projection is invalid")

func validateDurableRequestSource(items []compaction.TurnItem, snapshot *CompactionSourceSnapshot, runID string) error {
	if err := compaction.ValidateRunRequestIntegrity(items); err != nil {
		return fmt.Errorf("%w: %v", ErrDurableCompactionUnsupported, err)
	}
	if snapshot != nil {
		if err := snapshot.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrDurableCompactionUnsupported, err)
		}
	}
	priorCount := uint64(0)
	if snapshot != nil {
		priorCount = uint64(len(snapshot.SourceEntryIDs))
	}
	seenPrior := make(map[uint64]struct{}, priorCount)
	var summaryThrough llm.SourceThrough
	var summarySeen bool
	for index, item := range items {
		source := item.Runtime.Source
		switch source.Kind {
		case llm.SourceEphemeral:
			continue
		case llm.SourceNone, llm.SourcePendingRunInput:
			return fmt.Errorf("%w: item %d lacks a committed durable source", ErrDurableCompactionUnsupported, index)
		case llm.SourcePriorPublished:
			if snapshot == nil || source.HistoryRevision != snapshot.Revision || source.BranchID != snapshot.BranchID || source.PriorOrdinal >= priorCount || snapshot.SourceEntryIDs[source.PriorOrdinal] != source.SourceEntryID {
				return fmt.Errorf("%w: item %d does not match the sink's captured PublishedView", ErrDurableCompactionUnsupported, index)
			}
			if _, duplicate := seenPrior[source.PriorOrdinal]; duplicate {
				return fmt.Errorf("%w: item %d repeats a PublishedView ordinal", ErrDurableCompactionUnsupported, index)
			}
			seenPrior[source.PriorOrdinal] = struct{}{}
		case llm.SourceRunTranscript:
			if source.RunID != runID || source.PriorSourceCount != priorCount {
				return fmt.Errorf("%w: item %d has a transcript source outside this run snapshot", ErrDurableCompactionUnsupported, index)
			}
		case llm.SourceRunSummary:
			if source.RunID != runID || source.Through.PriorHistoryCount > priorCount || (source.Through.TranscriptCount > 0 && source.Through.PriorHistoryCount != priorCount) {
				return fmt.Errorf("%w: item %d has a summary cursor outside this run snapshot", ErrDurableCompactionUnsupported, index)
			}
			if summarySeen {
				return fmt.Errorf("%w: request contains multiple synthetic summaries from this run", ErrDurableCompactionUnsupported)
			}
			summarySeen = true
			summaryThrough = source.Through
		}
	}
	for ordinal := uint64(0); ordinal < priorCount; ordinal++ {
		if ordinal < summaryThrough.PriorHistoryCount && summarySeen {
			if _, duplicate := seenPrior[ordinal]; duplicate {
				return fmt.Errorf("%w: a retained PublishedView item overlaps the run summary cursor", ErrDurableCompactionUnsupported)
			}
			continue
		}
		if _, exists := seenPrior[ordinal]; !exists {
			return fmt.Errorf("%w: PublishedView ordinal %d is neither retained nor covered by a summary", ErrDurableCompactionUnsupported, ordinal)
		}
	}
	return nil
}

func validateDurableCompactionResult(input []compaction.TurnItem, result compaction.CompactionResult, snapshot *CompactionSourceSnapshot, runID string) (CompactionProjection, error) {
	unsupported := func(reason string) (CompactionProjection, error) {
		return CompactionProjection{}, fmt.Errorf("%w: %s: %w", ErrDurableCompactionUnsupported, reason, errDurableCompactionProjectionInvalid)
	}
	if snapshot == nil {
		return unsupported("the run sink did not expose its captured source snapshot")
	}
	if err := snapshot.Validate(); err != nil {
		return unsupported(err.Error())
	}
	if result.Strategy != compaction.StrategySummary || result.NewProjection == nil {
		return unsupported("the winning strategy did not produce a typed summary projection")
	}
	through := result.NewProjection.Through
	if through == (llm.SourceThrough{}) || through.PriorHistoryCount > uint64(len(snapshot.SourceEntryIDs)) || (through.TranscriptCount > 0 && through.PriorHistoryCount != uint64(len(snapshot.SourceEntryIDs))) {
		return unsupported("the typed summary cursor is outside the captured source view")
	}
	if err := compaction.ValidateRunRequestIntegrity(input); err != nil {
		return unsupported(err.Error())
	}
	if _, err := exactSourceBoundary(input, through); err != nil {
		return unsupported(err.Error())
	}

	summaryIndex := -1
	for index, item := range result.Items {
		if item.Runtime.Source.Kind == llm.SourceRunSummary {
			if summaryIndex >= 0 {
				return unsupported("the result contains more than one run-local summary")
			}
			summaryIndex = index
			if !item.IsCompaction || item.Role != compaction.RoleUser || item.Content != result.NewProjection.Summary {
				return unsupported("the run-local summary body or role differs from the typed projection")
			}
			if item.Runtime.Source.RunID != runID || item.Runtime.Source.Through != through {
				return unsupported("the summary item cursor differs from the typed projection")
			}
		}
	}
	if summaryIndex < 0 {
		return unsupported("the result does not contain its new run-local summary item")
	}
	for index, item := range result.Items {
		if index != summaryIndex && item.IsCompaction && item.Runtime.Source.Kind == llm.SourceRunSummary {
			return unsupported("a same-run synthetic summary remains outside the new marker")
		}
	}

	expectedSuffix := make([]compaction.TurnItem, 0, len(input))
	for index, item := range input {
		source := item.Runtime.Source
		if source.Kind == llm.SourceEphemeral {
			continue
		}
		covered, err := sourceCoveredByThrough(source, through)
		if err != nil {
			return unsupported(fmt.Sprintf("input item %d: %v", index, err))
		}
		if !covered {
			expectedSuffix = append(expectedSuffix, item)
		}
	}
	actualSuffix := make([]compaction.TurnItem, 0, len(expectedSuffix))
	for index, item := range result.Items {
		if item.Runtime.Source.Kind == llm.SourceEphemeral {
			continue
		}
		if index < summaryIndex {
			return unsupported("a source-backed item was retained before the replacement summary")
		}
		if index == summaryIndex {
			continue
		}
		if item.Runtime.Source.Kind == llm.SourceRunSummary {
			return unsupported("a same-run synthetic summary remains in the retained suffix")
		}
		actualSuffix = append(actualSuffix, item)
	}
	if len(actualSuffix) != len(expectedSuffix) {
		return unsupported("the source-backed suffix has missing or extra items")
	}
	for index := range expectedSuffix {
		if !sameDurableSuffixItem(expectedSuffix[index], actualSuffix[index]) {
			return unsupported(fmt.Sprintf("source-backed suffix item %d changed or moved", index))
		}
	}
	return CompactionProjection{
		ExpectedHistoryRevision: snapshot.Revision,
		Through:                 CompactionCursor{PriorHistoryCount: through.PriorHistoryCount, TranscriptCount: through.TranscriptCount},
		Summary:                 result.NewProjection.Summary,
	}, nil
}

func exactSourceBoundary(items []compaction.TurnItem, expected llm.SourceThrough) (int, error) {
	var cursor llm.SourceThrough
	lastRunSummaryCursor := llm.SourceThrough{}
	boundary := -1
	for index, item := range items {
		source := item.Runtime.Source
		switch source.Kind {
		case llm.SourceEphemeral:
			continue
		case llm.SourcePriorPublished:
			if source.PriorOrdinal != cursor.PriorHistoryCount {
				return -1, fmt.Errorf("source gap before item %d", index)
			}
			cursor.PriorHistoryCount++
		case llm.SourceRunTranscript:
			if source.PriorSourceCount != cursor.PriorHistoryCount || source.TranscriptIndex != cursor.TranscriptCount {
				return -1, fmt.Errorf("transcript gap before item %d", index)
			}
			cursor.TranscriptCount++
		case llm.SourceRunSummary:
			if source.Through.PriorHistoryCount < cursor.PriorHistoryCount || source.Through.TranscriptCount < cursor.TranscriptCount {
				return -1, fmt.Errorf("summary cursor moves backwards at item %d", index)
			}
			cursor = source.Through
			lastRunSummaryCursor = cursor
		default:
			return -1, fmt.Errorf("unsupported source kind at item %d", index)
		}
		if cursor == expected {
			boundary = index
		}
	}
	if boundary < 0 || expected == lastRunSummaryCursor {
		return -1, errors.New("typed cursor is not an advancing source boundary")
	}
	return boundary, nil
}

func sourceCoveredByThrough(source llm.SourceRef, through llm.SourceThrough) (bool, error) {
	switch source.Kind {
	case llm.SourcePriorPublished:
		return source.PriorOrdinal < through.PriorHistoryCount, nil
	case llm.SourceRunTranscript:
		return source.TranscriptIndex < through.TranscriptCount, nil
	case llm.SourceRunSummary:
		return source.Through.PriorHistoryCount <= through.PriorHistoryCount && source.Through.TranscriptCount <= through.TranscriptCount, nil
	default:
		return false, fmt.Errorf("source kind %d cannot participate in a durable projection", source.Kind)
	}
}

func sameDurableSuffixItem(expected, actual compaction.TurnItem) bool {
	expected.EstimatedTokens = 0
	actual.EstimatedTokens = 0
	return reflect.DeepEqual(expected, actual)
}
