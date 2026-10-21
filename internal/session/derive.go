package session

import (
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
)

var errUnclosedToolCallGroup = errors.New("session: tool-call group is not closed")

// DeriveMessages projects a slice of SessionEntry values into the
// model-visible message stream. It applies the same compaction-point logic as
// DefaultContextManager.BuildContext—entries before the last compaction entry
// are replaced by the compaction summary—and additionally filters entries based
// on their SurfaceOp field. Hidden and compacted entries are excluded; visible
// (or unmarked) entries are included.
func DeriveMessages(entries []SessionEntry) []SessionEntry {
	projected, err := projectSessionEntries(entries)
	if err != nil {
		return nil
	}
	cloned := make([]SessionEntry, len(projected))
	for i, entry := range projected {
		cloned[i] = *entry.clone()
	}
	return cloned
}

// SessionViewFromEntries builds a provenance-preserving model-history view for
// one already-selected branch. branchEntries must be ordered root to leaf;
// branch selection belongs to the caller.
func SessionViewFromEntries(sessionID, branchID, revision string, branchEntries []SessionEntry) (SessionView, error) {
	if sessionID == "" || branchID == "" {
		return SessionView{}, errors.New("session: view requires physical session and branch IDs")
	}
	for _, entry := range branchEntries {
		if entry.RecordVersion == 1 && entry.SessionID != sessionID {
			return SessionView{}, fmt.Errorf("session: entry %q belongs to physical session %q, not %q", entry.ID, entry.SessionID, sessionID)
		}
	}
	if err := validateToolCallJournalTail(sessionID, branchEntries); err != nil {
		return SessionView{}, err
	}
	entries, err := projectSessionEntries(branchEntries)
	if err != nil {
		return SessionView{}, err
	}
	if err := validateProjectedToolGroups(entries); err != nil {
		if errors.Is(err, errUnclosedToolCallGroup) {
			return SessionView{}, fmt.Errorf("session: projected history is inconsistent: %v", err)
		}
		return SessionView{}, err
	}

	view := SessionView{SessionID: sessionID, BranchID: branchID, Revision: revision}
	for _, entry := range entries {
		message, ok := sessionEntryToAgentMessage(entry)
		if !ok {
			continue
		}
		view.Messages = append(view.Messages, message)
		view.SourceEntryIDs = append(view.SourceEntryIDs, entry.ID)
	}
	view.Messages = core.CloneAgentMessages(view.Messages)
	if err := view.Validate(); err != nil {
		return SessionView{}, err
	}
	return view, nil
}

// validateToolCallJournalTail ensures an unfinished version-one assistant
// tool-call group remains the actual journal tail. It first validates the
// complete action/evidence relationships, then permits only exact execution
// observations and resolution checkpoints for that group while sibling calls
// remain open. Other rows cannot cross the receipt boundary.
func validateToolCallJournalTail(sessionID string, entries []SessionEntry) error {
	if _, err := analyzeDurableActionJournal(entries, sessionID); err != nil {
		return fmt.Errorf("session: invalid action journal before view projection: %w", err)
	}
	open := make(map[actionOccurrenceKey]string)
	currentGroup := make(map[actionOccurrenceKey]string)
	receipted := make(map[actionOccurrenceKey]struct{})
	for _, entry := range entries {
		if entry.RecordVersion == 1 {
			if err := validateVersion1EntryCoordinates(entry); err != nil {
				return fmt.Errorf("session: invalid version 1 entry %q: %w", entry.ID, err)
			}
		}
		if len(open) == 0 {
			if entry.RecordVersion == 1 && entry.Type == EntryTypeAssistant && len(entry.ToolCalls) > 0 {
				ordinal := *entry.AssistantTranscriptOrdinal
				currentGroup = make(map[actionOccurrenceKey]string, len(entry.ToolCalls))
				receipted = make(map[actionOccurrenceKey]struct{}, len(entry.ToolCalls))
				for _, call := range entry.ToolCalls {
					key := actionOccurrenceKey{sessionID: sessionID, runID: entry.RunID, ordinal: ordinal, toolCall: call.ID}
					open[key] = call.Name
					currentGroup[key] = call.Name
				}
			}
			continue
		}

		key := actionOccurrenceKey{sessionID: sessionID, runID: entry.RunID, toolCall: entry.ToolCallID}
		if entry.AssistantTranscriptOrdinal != nil {
			key.ordinal = *entry.AssistantTranscriptOrdinal
		}
		name, belongsToCurrentGroup := currentGroup[key]
		openName, occurrenceOpen := open[key]
		if entry.RecordVersion == 1 && entry.Type == EntryTypeTool && entry.ActionState == ActionStateIntent {
			if !belongsToCurrentGroup || !occurrenceOpen || name != openName || entry.AssistantTranscriptOrdinal == nil || name != entry.ToolName {
				return fmt.Errorf("session: action row %q does not belong to the open assistant tool-call group", entry.ID)
			}
			continue
		}
		if entry.RecordVersion == 1 && entry.Type == EntryTypeTool && entry.ActionState == ActionStateExecution {
			if !belongsToCurrentGroup || entry.AssistantTranscriptOrdinal == nil || name != entry.ToolName {
				return fmt.Errorf("session: execution row %q does not belong to the current assistant tool-call group", entry.ID)
			}
			continue
		}
		if entry.RecordVersion == 1 && entry.Type == EntryTypeTool && entry.ActionState == "" {
			if !belongsToCurrentGroup || !occurrenceOpen || entry.AssistantTranscriptOrdinal == nil || name != entry.ToolName {
				return fmt.Errorf("session: receipt %q does not belong to the open assistant tool-call group", entry.ID)
			}
			delete(open, key)
			receipted[key] = struct{}{}
			continue
		}
		if entry.RecordVersion == 1 && entry.Type == EntryTypeCheckpoint && entry.CheckpointKind == CheckpointExecutionResolution {
			if !belongsToCurrentGroup || entry.AssistantTranscriptOrdinal == nil {
				return fmt.Errorf("session: execution resolution %q does not belong to the current assistant tool-call group", entry.ID)
			}
			if _, hasReceipt := receipted[key]; !hasReceipt {
				return fmt.Errorf("session: execution resolution %q precedes its current-group receipt", entry.ID)
			}
			continue
		}
		return fmt.Errorf("session: entry %q interleaves an open assistant tool-call group", entry.ID)
	}
	if len(open) > 0 {
		return fmt.Errorf("%w: version 1 journal tail has %d unreceipted tool calls", errUnclosedToolCallGroup, len(open))
	}
	return nil
}

// projectSessionEntries folds compaction markers into their summaries. Version
// one markers use their exact source boundary; legacy markers retain their
// historical physical-position behavior.
func projectSessionEntries(entries []SessionEntry) ([]SessionEntry, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	hasVersionOne := false
	for _, entry := range entries {
		if entry.RecordVersion > 1 {
			return nil, fmt.Errorf("session: unsupported record version %d at entry %q", entry.RecordVersion, entry.ID)
		}
		if entry.RecordVersion == 1 {
			hasVersionOne = true
			if err := validateVersion1EntryCoordinates(entry); err != nil {
				return nil, fmt.Errorf("session: invalid version 1 entry %q: %w", entry.ID, err)
			}
			if err := validateTreeCheckpointCoordinates(entry); err != nil {
				return nil, fmt.Errorf("session: invalid version 1 entry %q: %w", entry.ID, err)
			}
		}
	}

	if !hasVersionOne {
		start := findLastCompactionPoint(entries)
		result := make([]SessionEntry, 0, len(entries)-start)
		for i, entry := range entries[start:] {
			if entry.Type == EntryTypeCompaction {
				result = append(result, compactionSummaryEntry(entry))
				continue
			}
			if i == 0 && start > 0 {
				continue
			}
			if entry.Type == EntryTypeCheckpoint || !entry.SurfaceVisible() {
				continue
			}
			result = append(result, entry)
		}
		return result, nil
	}

	byID := make(map[string]int, len(entries))
	var lastSequence uint64
	for i, entry := range entries {
		if entry.ID == "" {
			return nil, fmt.Errorf("session: version 1 branch has an entry with no ID at index %d", i)
		}
		if _, exists := byID[entry.ID]; exists {
			return nil, fmt.Errorf("session: version 1 branch repeats entry ID %q", entry.ID)
		}
		byID[entry.ID] = i
		if i > 0 && entry.ParentID != entries[i-1].ID {
			return nil, fmt.Errorf("session: branch entry %q does not follow its parent %q", entry.ID, entry.ParentID)
		}
		if entry.Seq == 0 && entry.RecordVersion == 1 {
			return nil, fmt.Errorf("session: version 1 entry %q has no sequence", entry.ID)
		}
		if entry.Seq != 0 && lastSequence != 0 && entry.Seq <= lastSequence {
			return nil, fmt.Errorf("session: branch sequence does not increase at entry %q", entry.ID)
		}
		if entry.Seq != 0 {
			lastSequence = entry.Seq
		}
		if entry.RecordVersion == 1 && entry.Type == EntryTypeCheckpoint && entry.ActiveLeafEntryID != "" && entry.ActiveLeafEntryID != entry.ID {
			activeLeafIndex, exists := byID[entry.ActiveLeafEntryID]
			if !exists || activeLeafIndex >= i {
				return nil, fmt.Errorf("session: checkpoint entry %q has an unmappable active leaf %q", entry.ID, entry.ActiveLeafEntryID)
			}
		}
	}

	lastCompaction := -1
	lastBoundary := -1
	for i, entry := range entries {
		if entry.Type != EntryTypeCompaction {
			continue
		}
		lastCompaction = i
		if entry.RecordVersion != 1 {
			continue
		}
		boundaryIndex, ok := byID[entry.CompactedThroughEntryID]
		if !ok || boundaryIndex >= i {
			return nil, fmt.Errorf("session: compaction entry %q has an unmappable source boundary %q", entry.ID, entry.CompactedThroughEntryID)
		}
		if lastBoundary > boundaryIndex {
			return nil, fmt.Errorf("session: compaction entry %q has a stale source boundary %q", entry.ID, entry.CompactedThroughEntryID)
		}
		lastBoundary = boundaryIndex
	}

	if lastCompaction < 0 {
		return visibleEntries(entries), nil
	}
	marker := entries[lastCompaction]
	if marker.RecordVersion != 1 {
		for i := 0; i < lastCompaction; i++ {
			if entries[i].RecordVersion == 1 && entries[i].Type == EntryTypeCompaction {
				return nil, fmt.Errorf("session: legacy compaction entry %q follows a version 1 source projection", marker.ID)
			}
		}
		result := make([]SessionEntry, 0, len(entries)-lastCompaction)
		result = append(result, compactionSummaryEntry(marker))
		for _, entry := range entries[lastCompaction+1:] {
			if entry.Type == EntryTypeCompaction || entry.Type == EntryTypeCheckpoint || !entry.SurfaceVisible() {
				continue
			}
			result = append(result, entry)
		}
		return result, nil
	}

	boundaryIndex, ok := byID[marker.CompactedThroughEntryID]
	if !ok || boundaryIndex >= lastCompaction {
		return nil, fmt.Errorf("session: compaction entry %q has an unmappable source boundary %q", marker.ID, marker.CompactedThroughEntryID)
	}
	result := make([]SessionEntry, 0, len(entries)-boundaryIndex)
	result = append(result, compactionSummaryEntry(marker))
	for _, entry := range entries[boundaryIndex+1:] {
		if entry.Type == EntryTypeCompaction || entry.Type == EntryTypeCheckpoint || !entry.SurfaceVisible() {
			continue
		}
		result = append(result, entry)
	}
	return result, nil
}

func visibleEntries(entries []SessionEntry) []SessionEntry {
	result := make([]SessionEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Type == EntryTypeCheckpoint || entry.Type == EntryTypeCompaction || !entry.SurfaceVisible() {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func compactionSummaryEntry(marker SessionEntry) SessionEntry {
	content := marker.Summary
	if content == "" {
		content = marker.Content
	}
	marker.Type = EntryTypeCompaction
	marker.Content = content
	marker.Summary = content
	return marker
}

func validateProjectedToolGroups(entries []SessionEntry) error {
	type occurrence struct {
		runID      string
		ordinal    uint64
		toolCallID string
	}
	open := make(map[occurrence]string)
	for i, entry := range entries {
		if entry.RecordVersion != 1 {
			continue
		}
		if entry.Type == EntryTypeAssistant && len(entry.ToolCalls) > 0 {
			if len(open) > 0 {
				return fmt.Errorf("session: assistant entry %q precedes tool receipts", entry.ID)
			}
			ordinal := *entry.AssistantTranscriptOrdinal
			for _, call := range entry.ToolCalls {
				key := occurrence{runID: entry.RunID, ordinal: ordinal, toolCallID: call.ID}
				if _, exists := open[key]; exists {
					return fmt.Errorf("session: assistant entry %q repeats tool call occurrence %q", entry.ID, call.ID)
				}
				open[key] = call.Name
			}
			continue
		}
		if entry.Type == EntryTypeTool && entry.ActionState != "" {
			continue
		}
		if entry.Type == EntryTypeTool {
			if entry.AssistantTranscriptOrdinal == nil {
				return fmt.Errorf("session: tool receipt %q has no assistant occurrence ordinal", entry.ID)
			}
			key := occurrence{runID: entry.RunID, ordinal: *entry.AssistantTranscriptOrdinal, toolCallID: entry.ToolCallID}
			name, exists := open[key]
			if !exists {
				return fmt.Errorf("session: tool receipt %q has no matching assistant tool call", entry.ID)
			}
			if name != entry.ToolName {
				return fmt.Errorf("session: tool receipt %q names %q, expected %q", entry.ID, entry.ToolName, name)
			}
			delete(open, key)
			continue
		}
		if len(open) > 0 && (entry.Type == EntryTypeUser || entry.Type == EntryTypeAssistant) {
			return fmt.Errorf("session: entry %q precedes tool receipts", entry.ID)
		}
		_ = i
	}
	if len(open) > 0 {
		return fmt.Errorf("%w: version 1 projected history has %d unreceipted tool calls", errUnclosedToolCallGroup, len(open))
	}
	return nil
}

func sessionEntryToAgentMessage(entry SessionEntry) (core.AgentMessage, bool) {
	message := core.AgentMessage{
		Content:                    entry.Content,
		ContentBlocks:              entry.ContentBlocks,
		Usage:                      entry.Usage,
		ToolCalls:                  entry.ToolCalls,
		ToolCallID:                 entry.ToolCallID,
		ToolName:                   entry.ToolName,
		MaterializedArtifactDigest: entry.MaterializedArtifactDigest,
		InputKind:                  entry.InputKind,
		ReceiptOutcome:             entry.ReceiptOutcome,
	}
	switch entry.Type {
	case EntryTypeUser:
		message.Role = "user"
	case EntryTypeAssistant:
		message.Role = "assistant"
	case EntryTypeSystem:
		message.Role = "system"
	case EntryTypeTool:
		if entry.RecordVersion != 1 || entry.ActionState != "" {
			return core.AgentMessage{}, false
		}
		message.Role = "tool"
	case EntryTypeCompaction:
		message.Role = "user"
		content := entry.Summary
		if content == "" {
			content = entry.Content
		}
		message.Content = compaction.UntrustedSummaryContext(content)
		message.ContentBlocks = nil
		message.Usage = nil
		message.ToolCalls = nil
		message.ToolCallID = ""
		message.ToolName = ""
		message.InputKind = ""
		message.ReceiptOutcome = ""
	default:
		return core.AgentMessage{}, false
	}
	return message, true
}

// DeriveAgentMessages projects a slice of SessionEntry values into
// core.AgentMessage values suitable for restoring agent history. It first
// applies DeriveMessages to obtain the model-visible entries, then converts
// them via EntriesToAgentMessages.
func DeriveAgentMessages(entries []SessionEntry) []core.AgentMessage {
	return EntriesToAgentMessages(DeriveMessages(entries))
}

// findLastCompactionPoint returns the index of the last EntryTypeCompaction
// entry in entries, or 0 if none exists. Both DeriveMessages and
// DefaultContextManager.BuildContext use this to determine where to start
// the model-visible message stream.
func findLastCompactionPoint(entries []SessionEntry) int {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Type == EntryTypeCompaction {
			return i
		}
	}
	return 0
}
