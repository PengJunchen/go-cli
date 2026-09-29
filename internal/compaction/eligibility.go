package compaction

import (
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/llm"
)

var (
	ErrInvalidRunRequestHistory        = errors.New("compaction: invalid durable request history")
	ErrRunHistoryNotCompactionEligible = errors.New("compaction: run history is not safe to compact")
)

// EstimateTokens reports the package's normal request-history estimate for a
// caller that must decide whether a durable request needs preflight compaction.
func EstimateTokens(items []TurnItem, estimator TokenEstimator) int {
	return estimateTokens(items, estimator)
}

// ValidateRunRequestIntegrity validates tool-group closure and runtime source
// provenance. A complete unknown receipt is structurally valid and remains
// usable as unchanged history for a fresh request.
func ValidateRunRequestIntegrity(items []TurnItem) error {
	normalized, _, err := groupTurnItems(items)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRunRequestHistory, err)
	}
	hasDurable := false
	for _, item := range normalized {
		if item.Runtime.Source.Kind != llm.SourceNone && item.Runtime.Source.Kind != llm.SourceEphemeral {
			hasDurable = true
			break
		}
	}
	if !hasDurable {
		return validateReceiptMetadata(normalized)
	}

	var cursor llm.SourceThrough
	var revision, branchID, runID string
	var priorCount *uint64
	transcriptStarted := false
	seen := make(map[string]struct{}, len(normalized))
	for index, item := range normalized {
		source := item.Runtime.Source
		if err := source.Validate(); err != nil {
			return fmt.Errorf("%w: item %d: %v", ErrInvalidRunRequestHistory, index, err)
		}
		if err := validateItemReceipt(item, index); err != nil {
			return err
		}
		if source.Kind == llm.SourceEphemeral {
			continue
		}
		if source.Kind == llm.SourceNone || source.Kind == llm.SourcePendingRunInput {
			return fmt.Errorf("%w: item %d has no committed source (kind %d)", ErrInvalidRunRequestHistory, index, source.Kind)
		}
		key := sourceIdentity(source)
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("%w: item %d repeats a source reference", ErrInvalidRunRequestHistory, index)
		}
		seen[key] = struct{}{}

		switch source.Kind {
		case llm.SourcePriorPublished:
			if transcriptStarted {
				return fmt.Errorf("%w: item %d places prior history after run transcript", ErrInvalidRunRequestHistory, index)
			}
			if revision == "" {
				revision, branchID = source.HistoryRevision, source.BranchID
			} else if revision != source.HistoryRevision || branchID != source.BranchID {
				return fmt.Errorf("%w: item %d changes prior history identity", ErrInvalidRunRequestHistory, index)
			}
			if source.PriorOrdinal != cursor.PriorHistoryCount {
				return fmt.Errorf("%w: item %d has a prior source gap or reordering", ErrInvalidRunRequestHistory, index)
			}
			cursor.PriorHistoryCount++
		case llm.SourceRunTranscript:
			transcriptStarted = true
			if runID == "" {
				runID = source.RunID
			} else if runID != source.RunID {
				return fmt.Errorf("%w: item %d changes run identity", ErrInvalidRunRequestHistory, index)
			}
			if priorCount == nil {
				count := source.PriorSourceCount
				priorCount = &count
			} else if *priorCount != source.PriorSourceCount {
				return fmt.Errorf("%w: item %d changes the captured prior source count", ErrInvalidRunRequestHistory, index)
			}
			if cursor.PriorHistoryCount != source.PriorSourceCount || source.TranscriptIndex != cursor.TranscriptCount {
				return fmt.Errorf("%w: item %d has a transcript gap or invalid prior prefix", ErrInvalidRunRequestHistory, index)
			}
			cursor.TranscriptCount++
		case llm.SourceRunSummary:
			if !item.IsCompaction || item.Role != RoleUser {
				return fmt.Errorf("%w: item %d tags a non-summary as a run summary", ErrInvalidRunRequestHistory, index)
			}
			if runID == "" {
				runID = source.RunID
			} else if runID != source.RunID {
				return fmt.Errorf("%w: item %d changes run identity", ErrInvalidRunRequestHistory, index)
			}
			if source.Through.PriorHistoryCount < cursor.PriorHistoryCount || source.Through.TranscriptCount < cursor.TranscriptCount {
				return fmt.Errorf("%w: item %d moves the source cursor backwards", ErrInvalidRunRequestHistory, index)
			}
			if source.Through.TranscriptCount > 0 && priorCount != nil && source.Through.PriorHistoryCount != *priorCount {
				return fmt.Errorf("%w: item %d summary cursor does not cover the full prior prefix", ErrInvalidRunRequestHistory, index)
			}
			cursor = source.Through
		}
	}
	if priorCount != nil && cursor.TranscriptCount > 0 && cursor.PriorHistoryCount != *priorCount {
		return fmt.Errorf("%w: transcript source does not preserve the full prior prefix", ErrInvalidRunRequestHistory)
	}
	return nil
}

// ValidateCompactionEligible adds the stricter no-unknown rule to request
// integrity. It must run before any strategy or summarizer is invoked.
func ValidateCompactionEligible(items []TurnItem) error {
	if err := ValidateRunRequestIntegrity(items); err != nil {
		return err
	}
	for index, item := range items {
		outcome := item.Runtime.ReceiptOutcome
		if outcome == "" {
			outcome = item.ReceiptOutcome
		}
		if item.Role == RoleTool && outcome == "unknown" {
			return fmt.Errorf("%w: item %d has an unknown tool receipt", ErrRunHistoryNotCompactionEligible, index)
		}
	}
	return nil
}

func validateDurableCompactionInput(items []TurnItem) error {
	if hasDurableSource(items) {
		return ValidateCompactionEligible(items)
	}
	return nil
}

func hasDurableSource(items []TurnItem) bool {
	for _, item := range items {
		kind := item.Runtime.Source.Kind
		if kind != llm.SourceNone && kind != llm.SourceEphemeral {
			return true
		}
	}
	return false
}

func validateReceiptMetadata(items []TurnItem) error {
	for index, item := range items {
		if err := validateItemReceipt(item, index); err != nil {
			return err
		}
	}
	return nil
}

func validateItemReceipt(item TurnItem, index int) error {
	canonical, runtime := item.ReceiptOutcome, item.Runtime.ReceiptOutcome
	if canonical != runtime {
		return fmt.Errorf("%w: item %d receipt metadata differs from canonical receipt", ErrInvalidRunRequestHistory, index)
	}
	if item.Role == RoleTool {
		switch canonical {
		case "success", "error", "interrupted", "unknown":
		default:
			return fmt.Errorf("%w: item %d has an invalid tool receipt outcome", ErrInvalidRunRequestHistory, index)
		}
	} else if canonical != "" {
		return fmt.Errorf("%w: item %d has a receipt outcome on a non-tool message", ErrInvalidRunRequestHistory, index)
	}
	return nil
}

func sourceIdentity(source llm.SourceRef) string {
	switch source.Kind {
	case llm.SourcePriorPublished:
		return fmt.Sprintf("prior:%s:%s:%s:%d", source.HistoryRevision, source.BranchID, source.SourceEntryID, source.PriorOrdinal)
	case llm.SourceRunTranscript:
		return fmt.Sprintf("transcript:%s:%d", source.RunID, source.TranscriptIndex)
	case llm.SourceRunSummary:
		return fmt.Sprintf("summary:%s:%d:%d", source.RunID, source.Through.PriorHistoryCount, source.Through.TranscriptCount)
	default:
		return fmt.Sprintf("non-durable:%d", source.Kind)
	}
}

func currentRunID(items []TurnItem) string {
	for _, item := range items {
		source := item.Runtime.Source
		if source.Kind == llm.SourceRunTranscript || source.Kind == llm.SourceRunSummary || source.Kind == llm.SourcePendingRunInput {
			return source.RunID
		}
	}
	return ""
}

// sourceThroughForPrefix computes the exact source cursor consumed by a
// summary cut. It never derives a boundary from item count or message content.
func sourceThroughForPrefix(items []TurnItem) (llm.SourceThrough, bool, error) {
	if !hasDurableSource(items) {
		return llm.SourceThrough{}, false, nil
	}
	if err := ValidateRunRequestIntegrity(items); err != nil {
		return llm.SourceThrough{}, false, err
	}
	var cursor llm.SourceThrough
	for index, item := range items {
		source := item.Runtime.Source
		switch source.Kind {
		case llm.SourceEphemeral:
			continue
		case llm.SourcePriorPublished:
			if source.PriorOrdinal != cursor.PriorHistoryCount {
				return llm.SourceThrough{}, false, fmt.Errorf("%w: cut has a prior source gap at item %d", ErrInvalidRunRequestHistory, index)
			}
			cursor.PriorHistoryCount++
		case llm.SourceRunTranscript:
			if source.PriorSourceCount != cursor.PriorHistoryCount || source.TranscriptIndex != cursor.TranscriptCount {
				return llm.SourceThrough{}, false, fmt.Errorf("%w: cut has a transcript source gap at item %d", ErrInvalidRunRequestHistory, index)
			}
			cursor.TranscriptCount++
		case llm.SourceRunSummary:
			if source.Through.PriorHistoryCount < cursor.PriorHistoryCount || source.Through.TranscriptCount < cursor.TranscriptCount {
				return llm.SourceThrough{}, false, fmt.Errorf("%w: cut moves source cursor backwards at item %d", ErrInvalidRunRequestHistory, index)
			}
			cursor = source.Through
		default:
			return llm.SourceThrough{}, false, fmt.Errorf("%w: cut contains unsupported source at item %d", ErrInvalidRunRequestHistory, index)
		}
	}
	if cursor == (llm.SourceThrough{}) {
		return llm.SourceThrough{}, false, fmt.Errorf("%w: summary cut does not cover a durable source", ErrInvalidRunRequestHistory)
	}
	return cursor, true, nil
}
