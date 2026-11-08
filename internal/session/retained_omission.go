package session

import (
	"fmt"
	"reflect"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

// retainedExchangeProof derives both the source partition and the retained
// payload bindings from the journal. It never trusts a caller-supplied body.
func retainedExchangeProof(entries []SessionEntry, marker SessionEntry, markerIndex int) ([]RetainedExchange, error) {
	return retainedExchangeProofWithState(entries, marker, markerIndex, make(map[string]bool))
}

func retainedExchangeProofWithState(entries []SessionEntry, marker SessionEntry, markerIndex int, validated map[string]bool) ([]RetainedExchange, error) {
	if marker.RetainedHistoryBranchID == "" || marker.RetainedInputEntryID == "" || len(marker.RetainedExchanges) == 0 {
		return nil, fmt.Errorf("retained omission lacks its captured source identity")
	}
	positions := make(map[string]int, markerIndex)
	for i, row := range entries[:markerIndex] {
		positions[row.ID] = i
	}
	anchorIndex, ok := positions[marker.RetainedInputEntryID]
	if !ok {
		return nil, fmt.Errorf("retained omission has no original input")
	}
	base, err := projectSessionEntriesWithState(entries[:anchorIndex], validated)
	if err != nil {
		return nil, fmt.Errorf("derive retained omission run-start view: %w", err)
	}
	prior := make(map[string]uint64, len(base))
	for _, row := range base {
		if _, ok := sessionEntryToAgentMessage(row); ok {
			prior[row.ID] = uint64(len(prior))
		}
	}
	projected, err := projectSessionEntriesWithState(entries[:markerIndex], validated)
	if err != nil {
		return nil, fmt.Errorf("derive retained omission source: %w", err)
	}
	priorCount := uint64(len(prior))
	if priorCount > 0 && marker.RetainedHistoryRevision == "" {
		return nil, fmt.Errorf("nonempty retained source view lacks its revision")
	}
	items := make([]compaction.TurnItem, 0, len(projected))
	rows := make(map[llm.SourceRef]SessionEntry, len(projected))
	for _, row := range projected {
		message, visible := sessionEntryToAgentMessage(row)
		if !visible {
			continue
		}
		var source llm.SourceRef
		if ordinal, exists := prior[row.ID]; exists {
			source = llm.SourceRef{Kind: llm.SourcePriorPublished, HistoryRevision: marker.RetainedHistoryRevision, BranchID: marker.RetainedHistoryBranchID, SourceEntryID: row.ID, PriorOrdinal: ordinal}
		} else if row.RunID == marker.RunID && row.TranscriptIndex != nil && row.ActionState == "" && row.Type != EntryTypeCompaction {
			source = llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: marker.RunID, PriorSourceCount: priorCount, TranscriptIndex: *row.TranscriptIndex}
		} else if row.RunID == marker.RunID && row.Type == EntryTypeCompaction {
			boundaryPosition, exists := positions[row.CompactedThroughEntryID]
			if !exists {
				return nil, fmt.Errorf("previous compaction boundary is missing")
			}
			boundary := entries[boundaryPosition]
			through := llm.SourceThrough{}
			if ordinal, exists := prior[boundary.ID]; exists {
				through.PriorHistoryCount = ordinal + 1
			} else if boundary.RunID == marker.RunID && boundary.TranscriptIndex != nil {
				through = llm.SourceThrough{PriorHistoryCount: priorCount, TranscriptCount: *boundary.TranscriptIndex + 1}
			} else {
				return nil, fmt.Errorf("previous compaction has a foreign source boundary")
			}
			ranges := make([]llm.RetainedSourceRange, len(row.RetainedExchanges))
			for i := range row.RetainedExchanges {
				ranges[i] = row.RetainedExchanges[i].Range
			}
			encoded, encodeErr := compaction.EncodeRetainedSourceRanges(ranges)
			if encodeErr != nil {
				return nil, encodeErr
			}
			source = llm.SourceRef{Kind: llm.SourceRunSummary, RunID: marker.RunID, Through: through, RetainCurrentInput: row.RetainedInputEntryID != "", RetainedSourceRanges: encoded}
		} else {
			return nil, fmt.Errorf("retained omission source row %q is outside the captured run", row.ID)
		}
		item := compaction.TurnItem{ID: row.ID, Role: message.Role, ContentBlocks: message.ContentBlocks, Usage: message.Usage, ToolCalls: message.ToolCalls, ToolCallID: message.ToolCallID, ToolName: message.ToolName, ReceiptOutcome: message.ReceiptOutcome, Runtime: llm.RuntimeMetadata{Source: source, ReceiptOutcome: message.ReceiptOutcome}}
		if message.Role == compaction.RoleTool {
			item.ToolResult = message.Content
		} else {
			item.Content = message.Content
		}
		if row.Type == EntryTypeCompaction {
			item.IsCompaction = true
			if raw, exists := compaction.UnwrapUntrustedSummaryContext(item.Content); exists {
				item.Content = raw
			}
			item.Runtime.CompactionMarker = source.Kind == llm.SourceRunSummary
		}
		items = append(items, item)
		rows[source] = row
	}
	boundaryPosition, ok := positions[marker.CompactedThroughEntryID]
	if !ok {
		return nil, fmt.Errorf("retained omission boundary is missing")
	}
	boundaryRow := entries[boundaryPosition]
	if boundaryRow.RunID != marker.RunID || boundaryRow.TranscriptIndex == nil {
		return nil, fmt.Errorf("retained omission boundary is outside the run")
	}
	through := llm.SourceThrough{PriorHistoryCount: priorCount, TranscriptCount: *boundaryRow.TranscriptIndex + 1}
	boundary, err := compaction.SourceBoundaryForProjection(items, through)
	if err != nil {
		return nil, err
	}
	ranges := make([]llm.RetainedSourceRange, len(marker.RetainedExchanges))
	var kept []compaction.TurnItem
	proof := make([]RetainedExchange, len(ranges))
	for i, exchange := range marker.RetainedExchanges {
		ranges[i] = exchange.Range
		start := -1
		for j, item := range items {
			if item.Runtime.Source == exchange.Range.Start {
				start = j
				break
			}
		}
		if start < 0 {
			return nil, fmt.Errorf("retained exchange start is missing")
		}
		foundEnd := false
		proof[i].Range = exchange.Range
		for j := start; j <= boundary; j++ {
			item := items[j]
			row, exists := rows[item.Runtime.Source]
			if !exists {
				return nil, fmt.Errorf("retained exchange row is missing")
			}
			_, digest, digestErr := canonicalEntryPayload(row)
			if digestErr != nil || (row.RecordVersion == 1 && row.PayloadDigest != digest) {
				return nil, fmt.Errorf("retained exchange row payload changed")
			}
			proof[i].Entries = append(proof[i].Entries, RetainedSourceEntry{EntryID: row.ID, PayloadDigest: digest})
			kept = append(kept, item)
			if item.Runtime.Source == exchange.Range.End {
				foundEnd = true
				break
			}
		}
		if !foundEnd {
			return nil, fmt.Errorf("retained exchange end is missing")
		}
	}
	encoded, err := compaction.EncodeRetainedSourceRanges(ranges)
	if err != nil {
		return nil, err
	}
	var anchor compaction.TurnItem
	for _, item := range items {
		if item.Runtime.Source.Kind == llm.SourceRunTranscript && item.Runtime.Source.TranscriptIndex == 0 {
			anchor = item
			break
		}
	}
	if anchor.Runtime.Source.RunID != marker.RunID {
		return nil, fmt.Errorf("retained omission current input is missing")
	}
	summary := compaction.TurnItem{Role: compaction.RoleUser, Content: compaction.DurableTruncationMarker, IsCompaction: true, Runtime: llm.RuntimeMetadata{CompactionMarker: true, Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: marker.RunID, Through: through, RetainCurrentInput: true, RetainedSourceRanges: encoded}}}
	after := append([]compaction.TurnItem{anchor, summary}, kept...)
	after = append(after, items[boundary+1:]...)
	projection := &compaction.NewProjection{Summary: compaction.DurableTruncationMarker, Through: through, RetainCurrentInput: true, RetainedSourceRanges: ranges}
	if err := compaction.ValidateRetainedOmission(items, after, projection); err != nil {
		return nil, fmt.Errorf("retained omission source partition: %w", err)
	}
	return proof, nil
}

func validateRetainedExchangeProof(entries []SessionEntry, marker SessionEntry, markerIndex int, validated map[string]bool) error {
	proof, err := retainedExchangeProofWithState(entries, marker, markerIndex, validated)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(proof, marker.RetainedExchanges) {
		return fmt.Errorf("retained exchange row identities or payload digests differ")
	}
	return nil
}
