package cli

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tui"
)

// ProjectPreparedTranscript transforms one coherent prepared-session snapshot
// into the value consumed by the TUI canonical lane. It does not reopen storage
// or infer transcript state from events.
func ProjectPreparedTranscript(ctx context.Context, snapshot session.PreparedSessionSnapshot) (tui.CanonicalTranscriptSnapshot, error) {
	input, err := prepareTranscriptProjection(ctx, snapshot)
	if err != nil {
		return tui.CanonicalTranscriptSnapshot{}, err
	}
	return projectTranscriptRows(input)
}

// ProjectRunObservation copies one owner's status and any matching durable
// terminal fact found in the same snapshot and selected branch. A terminal row
// is an observed fact only; this value makes no saved or acknowledgement claim.
func ProjectRunObservation(ctx context.Context, snapshot session.PreparedSessionSnapshot, handle core.RunStatusHandle, resultRunID string) (tui.RunObservation, error) {
	if handle == nil {
		return tui.RunObservation{}, errors.New("cli: run observation requires an owner status handle")
	}
	input, err := prepareTranscriptProjection(ctx, snapshot)
	if err != nil {
		return tui.RunObservation{}, err
	}
	if _, err := projectTranscriptRows(input); err != nil {
		return tui.RunObservation{}, err
	}
	status := handle.Snapshot()
	if status.RunID == "" {
		return tui.RunObservation{}, errors.New("cli: run status handle has no run ID")
	}
	if resultRunID != "" && resultRunID != status.RunID {
		return tui.RunObservation{}, fmt.Errorf("cli: result run ID %q does not match owner run ID %q", resultRunID, status.RunID)
	}
	observation := tui.RunObservation{
		RunID: status.RunID, Outcome: status.Outcome, ExecutionState: status.ExecutionState,
		EventGapCount: status.EventGapCount,
	}
	for _, entry := range input.path {
		if entry.Type != session.EntryTypeCheckpoint || entry.CheckpointKind != session.CheckpointRunTerminal || entry.RunID != status.RunID {
			continue
		}
		if observation.TerminalEntryID != "" {
			return tui.RunObservation{}, fmt.Errorf("cli: selected branch has multiple terminal checkpoints for run %q", status.RunID)
		}
		if !validProjectedRunOutcome(entry.RunOutcome) {
			return tui.RunObservation{}, fmt.Errorf("cli: terminal checkpoint %q has invalid run outcome %q", entry.ID, entry.RunOutcome)
		}
		observation.TerminalEntryID = entry.ID
		observation.TerminalOutcome = entry.RunOutcome
	}
	return observation, nil
}

type transcriptProjectionInput struct {
	snapshot session.PreparedSessionSnapshot
	entries  map[string]session.SessionEntry
	path     []session.SessionEntry
	pathIDs  map[string]struct{}
}

func prepareTranscriptProjection(ctx context.Context, source session.PreparedSessionSnapshot) (transcriptProjectionInput, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot := source.Clone()
	if snapshot.SessionID == "" || snapshot.ActiveBranchID == "" {
		return transcriptProjectionInput{}, errors.New("cli: prepared snapshot requires a physical session and active branch ID")
	}
	if err := snapshot.PublishedView.Validate(); err != nil {
		return transcriptProjectionInput{}, fmt.Errorf("cli: invalid published session view: %w", err)
	}
	if snapshot.PublishedView.SessionID != snapshot.SessionID || snapshot.PublishedView.BranchID != snapshot.ActiveBranchID {
		return transcriptProjectionInput{}, errors.New("cli: published view identity does not match prepared snapshot")
	}
	entries := make(map[string]session.SessionEntry, len(snapshot.DurableEntries))
	for index, entry := range snapshot.DurableEntries {
		if entry.ID == "" {
			return transcriptProjectionInput{}, fmt.Errorf("cli: durable entry at physical index %d has no ID", index)
		}
		if _, exists := entries[entry.ID]; exists {
			return transcriptProjectionInput{}, fmt.Errorf("cli: durable entry ID %q is repeated", entry.ID)
		}
		entries[entry.ID] = entry
	}
	if snapshot.ActiveLeafEntryID == "" {
		if len(snapshot.DurableEntries) != 0 || len(snapshot.PublishedView.Messages) != 0 {
			return transcriptProjectionInput{}, errors.New("cli: non-empty prepared snapshot has no active leaf")
		}
		return transcriptProjectionInput{snapshot: snapshot, entries: entries, pathIDs: map[string]struct{}{}}, nil
	}

	tree, err := session.NewDefaultSessionTreeBuilder().BuildFromSnapshot(ctx, snapshot)
	if err != nil {
		return transcriptProjectionInput{}, fmt.Errorf("cli: build selected transcript branch from snapshot: %w", err)
	}
	branch, err := tree.GetBranch(ctx, snapshot.ActiveLeafEntryID)
	if err != nil {
		return transcriptProjectionInput{}, fmt.Errorf("cli: read selected transcript branch: %w", err)
	}
	path := make([]session.SessionEntry, 0, len(branch))
	pathIDs := make(map[string]struct{}, len(branch))
	for index, entry := range branch {
		if entry == nil || entry.ID == "" {
			return transcriptProjectionInput{}, fmt.Errorf("cli: selected branch has invalid entry at index %d", index)
		}
		canonical, exists := entries[entry.ID]
		if !exists {
			return transcriptProjectionInput{}, fmt.Errorf("cli: selected branch entry %q is absent from durable snapshot", entry.ID)
		}
		if _, exists := pathIDs[entry.ID]; exists {
			return transcriptProjectionInput{}, fmt.Errorf("cli: selected branch repeats entry %q", entry.ID)
		}
		pathIDs[entry.ID] = struct{}{}
		path = append(path, canonical)
	}
	if len(path) == 0 || path[len(path)-1].ID != snapshot.ActiveLeafEntryID {
		return transcriptProjectionInput{}, errors.New("cli: selected branch does not terminate at the prepared active leaf")
	}
	return transcriptProjectionInput{snapshot: snapshot, entries: entries, path: path, pathIDs: pathIDs}, nil
}

func projectTranscriptRows(input transcriptProjectionInput) (tui.CanonicalTranscriptSnapshot, error) {
	snapshot := input.snapshot
	view := snapshot.PublishedView
	result := tui.CanonicalTranscriptSnapshot{
		PhysicalSessionID: snapshot.SessionID,
		BranchID:          snapshot.ActiveBranchID,
		Revision:          view.Revision,
		MaxSeq:            snapshot.MaxSeq,
		ActiveLeafEntryID: snapshot.ActiveLeafEntryID,
		Rows:              make([]tui.CanonicalTranscriptRow, 0, len(view.Messages)),
	}
	if len(view.Messages) != len(view.SourceEntryIDs) {
		return tui.CanonicalTranscriptSnapshot{}, fmt.Errorf("cli: published view has %d messages and %d source IDs", len(view.Messages), len(view.SourceEntryIDs))
	}
	callTargets := make(map[tui.ToolOccurrenceKey]transcriptCallTarget)
	for index, message := range view.Messages {
		entryID := view.SourceEntryIDs[index]
		entry, exists := input.entries[entryID]
		if !exists {
			return tui.CanonicalTranscriptSnapshot{}, fmt.Errorf("cli: published message %d references missing source entry %q", index, entryID)
		}
		if _, onSelectedBranch := input.pathIDs[entryID]; !onSelectedBranch {
			return tui.CanonicalTranscriptSnapshot{}, fmt.Errorf("cli: published source entry %q is outside the selected active branch", entryID)
		}
		kind, ok := transcriptKindForEntry(entry)
		if !ok {
			return tui.CanonicalTranscriptSnapshot{}, fmt.Errorf("cli: published source entry %q has non-transcript type %q", entryID, entry.Type)
		}
		if !messageMatchesSource(entry, message) {
			return tui.CanonicalTranscriptSnapshot{}, fmt.Errorf("cli: canonical message %d does not match exact source entry %q", index, entryID)
		}
		row := tui.CanonicalTranscriptRow{
			Key:  tui.DurableTranscriptKey{PhysicalSessionID: snapshot.SessionID, BranchID: snapshot.ActiveBranchID, EntryID: entryID},
			Kind: kind, Message: message, SourceSeq: entry.Seq, ActionID: entry.ActionID,
		}
		if kind == tui.TranscriptRowAssistant && entry.RunID != "" && entry.TranscriptIndex != nil {
			row.AssistantOccurrence = &tui.AssistantOccurrenceKey{
				RunID: entry.RunID, CanonicalTranscriptIndex: *entry.TranscriptIndex,
			}
		}
		if kind == tui.TranscriptRowCompaction {
			summary := entry.Summary
			if summary == "" {
				summary = entry.Content
			}
			row.Compaction = &tui.CompactionFact{CompactedThroughEntryID: entry.CompactedThroughEntryID, Summary: summary}
		}
		if kind == tui.TranscriptRowTool && hasCompleteOccurrence(entry) {
			row.Occurrence = &tui.ToolOccurrenceKey{
				RunID: entry.RunID, AssistantTranscriptOrdinal: *entry.AssistantTranscriptOrdinal, ToolCallID: entry.ToolCallID,
			}
		}
		if kind == tui.TranscriptRowAssistant && hasCompleteAssistantOccurrence(entry) {
			row.Calls = make([]tui.ToolOccurrence, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				if call.ID == "" || call.Name == "" {
					continue
				}
				key := tui.ToolOccurrenceKey{RunID: entry.RunID, AssistantTranscriptOrdinal: *entry.AssistantTranscriptOrdinal, ToolCallID: call.ID}
				if _, duplicate := callTargets[key]; duplicate {
					return tui.CanonicalTranscriptSnapshot{}, fmt.Errorf("cli: selected transcript has ambiguous tool occurrence %q", call.ID)
				}
				row.Calls = append(row.Calls, tui.ToolOccurrence{Key: key, ToolName: call.Name})
				callTargets[key] = transcriptCallTarget{rowIndex: len(result.Rows), callIndex: len(row.Calls) - 1, toolName: call.Name}
			}
		}
		result.Rows = append(result.Rows, row)
	}

	// DurableEntries are physical append ordered. Filtering by the validated
	// active path preserves that order without admitting sibling or hidden rows.
	for _, entry := range snapshot.DurableEntries {
		if _, onSelectedBranch := input.pathIDs[entry.ID]; !onSelectedBranch || entry.Type != session.EntryTypeTool {
			continue
		}
		var stage tui.ActionJournalStage
		switch entry.ActionState {
		case session.ActionStateIntent:
			stage = tui.ActionJournalIntent
		case session.ActionStateExecution:
			stage = tui.ActionJournalExecution
		default:
			continue
		}
		if !hasOccurrenceCoordinates(entry) {
			continue
		}
		key := tui.ToolOccurrenceKey{RunID: entry.RunID, AssistantTranscriptOrdinal: *entry.AssistantTranscriptOrdinal, ToolCallID: entry.ToolCallID}
		target, matches := callTargets[key]
		if !matches {
			// The corresponding assistant row may be outside PublishedView
			// while a newer/open group is durable. Never synthesize it here.
			continue
		}
		if target.toolName != entry.ToolName {
			return tui.CanonicalTranscriptSnapshot{}, fmt.Errorf("cli: journal entry %q tool name conflicts with exact assistant occurrence", entry.ID)
		}
		result.Rows[target.rowIndex].Calls[target.callIndex].JournalFacts = append(
			result.Rows[target.rowIndex].Calls[target.callIndex].JournalFacts,
			tui.ActionJournalFact{
				EntryID: entry.ID, ActionBatchID: entry.ActionBatchID, ActionID: entry.ActionID,
				Stage: stage, DispatchState: entry.DispatchState, ResourceState: entry.ResourceState,
				CompletionProof: string(entry.CompletionProof),
			},
		)
	}
	if err := result.Validate(); err != nil {
		return tui.CanonicalTranscriptSnapshot{}, fmt.Errorf("cli: invalid canonical transcript projection: %w", err)
	}
	return result, nil
}

type transcriptCallTarget struct {
	rowIndex  int
	callIndex int
	toolName  string
}

func transcriptKindForEntry(entry session.SessionEntry) (tui.TranscriptRowKind, bool) {
	switch entry.Type {
	case session.EntryTypeUser:
		return tui.TranscriptRowUser, true
	case session.EntryTypeAssistant:
		return tui.TranscriptRowAssistant, true
	case session.EntryTypeTool:
		if entry.ActionState != "" {
			return 0, false
		}
		return tui.TranscriptRowTool, true
	case session.EntryTypeSystem:
		return tui.TranscriptRowSystem, true
	case session.EntryTypeCompaction:
		return tui.TranscriptRowCompaction, true
	default:
		return 0, false
	}
}

func messageMatchesSource(entry session.SessionEntry, message core.AgentMessage) bool {
	expected := core.AgentMessage{
		Content: entry.Content, ContentBlocks: entry.ContentBlocks, Usage: entry.Usage,
		ToolCalls: entry.ToolCalls, ToolCallID: entry.ToolCallID, ToolName: entry.ToolName,
		InputKind: entry.InputKind, ReceiptOutcome: entry.ReceiptOutcome,
	}
	switch entry.Type {
	case session.EntryTypeUser:
		expected.Role = "user"
	case session.EntryTypeAssistant:
		expected.Role = "assistant"
	case session.EntryTypeSystem:
		expected.Role = "system"
	case session.EntryTypeTool:
		expected.Role = "tool"
	case session.EntryTypeCompaction:
		summary := entry.Summary
		if summary == "" {
			summary = entry.Content
		}
		expected = core.AgentMessage{Role: "user", Content: compaction.UntrustedSummaryContext(summary)}
	default:
		return false
	}
	return sameAgentMessage(expected, message)
}

func sameAgentMessage(left, right core.AgentMessage) bool {
	if left.Role != right.Role || left.Content != right.Content || left.ToolCallID != right.ToolCallID ||
		left.ToolName != right.ToolName || left.InputKind != right.InputKind || left.ReceiptOutcome != right.ReceiptOutcome {
		return false
	}
	if !sameOptionalUsage(left.Usage, right.Usage) || !sameSliceValue(left.ContentBlocks, right.ContentBlocks) ||
		!sameSliceValue(left.ToolCalls, right.ToolCalls) {
		return false
	}
	return true
}

func sameOptionalUsage(left, right *llm.Usage) bool {
	return reflect.DeepEqual(left, right)
}

func sameSliceValue[T any](left, right []T) bool {
	if len(left) != len(right) {
		return false
	}
	if len(left) == 0 {
		return true
	}
	return reflect.DeepEqual(left, right)
}

func hasCompleteOccurrence(entry session.SessionEntry) bool {
	return hasOccurrenceCoordinates(entry) && entry.ToolName != ""
}

func hasOccurrenceCoordinates(entry session.SessionEntry) bool {
	return entry.RunID != "" && entry.AssistantTranscriptOrdinal != nil && entry.ToolCallID != ""
}

func hasCompleteAssistantOccurrence(entry session.SessionEntry) bool {
	return entry.RunID != "" && entry.AssistantTranscriptOrdinal != nil
}

func validProjectedRunOutcome(outcome core.RunOutcome) bool {
	switch outcome {
	case core.RunOutcomeSuccess, core.RunOutcomeError, core.RunOutcomeCancelled, core.RunOutcomeUnknown:
		return true
	default:
		return false
	}
}
