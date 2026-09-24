package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
)

type durableActionOccurrence struct {
	ref        PendingActionRef
	assistant  SessionEntry
	callIndex  int
	toolName   string
	intent     *SessionEntry
	receipt    *SessionEntry
	resolution *ValidatedExecutionResolution
}

type durableRecoveryPlan struct {
	resolutions         []ValidatedExecutionResolution
	pending             []PendingActionRef
	occurrences         []*durableActionOccurrence
	nextTranscriptIndex map[string]uint64
}

type durableActionBatchKey struct {
	runID     string
	ordinal   uint64
	workspace string
	branchID  string
}

// analyzeDurableActionJournal validates all session-local action relationships
// before any RecoveryAuthority is allowed to inspect the global index.
func analyzeDurableActionJournal(entries []SessionEntry, sessionID string) (*durableRecoveryPlan, error) {
	plan := &durableRecoveryPlan{nextTranscriptIndex: make(map[string]uint64)}
	occurrencesByKey := make(map[actionOccurrenceKey]*durableActionOccurrence)
	intentsByActionID := make(map[string]*durableActionOccurrence)
	transcriptIndexes := make(map[string]map[uint64]string)
	assistantOrdinals := make(map[string]map[uint64]string)
	batchKeys := make(map[string]durableActionBatchKey)
	executionRows := make(map[string]*SessionEntry)
	reconciliationRows := make(map[string]*SessionEntry)
	entriesByID := make(map[string]*SessionEntry, len(entries))
	var resolutions []*SessionEntry

	for i := range entries {
		entry := &entries[i]
		entriesByID[entry.ID] = entry
		if entry.RecordVersion != 1 {
			continue
		}
		if entry.TranscriptIndex != nil && entry.Type != EntryTypeCompaction && entry.Type != EntryTypeCheckpoint && entry.ActionState == "" {
			byIndex := transcriptIndexes[entry.RunID]
			if byIndex == nil {
				byIndex = make(map[uint64]string)
				transcriptIndexes[entry.RunID] = byIndex
			}
			if previous, exists := byIndex[*entry.TranscriptIndex]; exists {
				return nil, fmt.Errorf("session: run %q repeats transcript index %d at entries %q and %q", entry.RunID, *entry.TranscriptIndex, previous, entry.ID)
			}
			byIndex[*entry.TranscriptIndex] = entry.ID
			if *entry.TranscriptIndex == ^uint64(0) {
				return nil, fmt.Errorf("session: run %q transcript index is exhausted", entry.RunID)
			}
			if next := *entry.TranscriptIndex + 1; next > plan.nextTranscriptIndex[entry.RunID] {
				plan.nextTranscriptIndex[entry.RunID] = next
			}
		}
		if entry.Type == EntryTypeAssistant && len(entry.ToolCalls) > 0 {
			if entry.AssistantTranscriptOrdinal == nil {
				return nil, fmt.Errorf("session: assistant tool-call entry %q has no occurrence ordinal", entry.ID)
			}
			byOrdinal := assistantOrdinals[entry.RunID]
			if byOrdinal == nil {
				byOrdinal = make(map[uint64]string)
				assistantOrdinals[entry.RunID] = byOrdinal
			}
			ordinal := *entry.AssistantTranscriptOrdinal
			if previous, exists := byOrdinal[ordinal]; exists {
				return nil, fmt.Errorf("session: run %q repeats assistant transcript ordinal %d at entries %q and %q", entry.RunID, ordinal, previous, entry.ID)
			}
			byOrdinal[ordinal] = entry.ID
			seenCallIDs := make(map[string]struct{}, len(entry.ToolCalls))
			for callIndex, call := range entry.ToolCalls {
				if call.ID == "" || call.Name == "" {
					return nil, fmt.Errorf("session: assistant entry %q has an incomplete tool call at index %d", entry.ID, callIndex)
				}
				if _, exists := seenCallIDs[call.ID]; exists {
					return nil, fmt.Errorf("session: assistant entry %q repeats tool-call ID %q", entry.ID, call.ID)
				}
				seenCallIDs[call.ID] = struct{}{}
				key := actionOccurrenceKey{sessionID: sessionID, runID: entry.RunID, ordinal: ordinal, toolCall: call.ID}
				if _, exists := occurrencesByKey[key]; exists {
					return nil, fmt.Errorf("session: duplicate tool-call occurrence in assistant entry %q", entry.ID)
				}
				occurrence := &durableActionOccurrence{
					assistant: *entry.clone(),
					callIndex: callIndex,
					toolName:  call.Name,
				}
				occurrencesByKey[key] = occurrence
				plan.occurrences = append(plan.occurrences, occurrence)
			}
		}
		if entry.ActionState == ActionStateExecution {
			if entry.ExecutionIdentity != nil {
				if err := entry.ExecutionIdentity.Validate(); err != nil {
					return nil, fmt.Errorf("session: execution row %q has invalid identity: %w", entry.ID, err)
				}
			}
			executionRows[entry.ID] = entry
		}
		if entry.CheckpointKind == CheckpointReconciliation {
			reconciliationRows[entry.ID] = entry
		}
		if entry.CheckpointKind == CheckpointExecutionResolution {
			resolutions = append(resolutions, entry)
		}
	}

	for i := range entries {
		entry := &entries[i]
		if entry.RecordVersion != 1 {
			continue
		}
		if entry.ActionState == ActionStateIntent {
			key, err := actionKeyForEntry(*entry, sessionID)
			if err != nil {
				return nil, err
			}
			occurrence, exists := occurrencesByKey[key]
			if !exists {
				return nil, fmt.Errorf("session: intent %q has no matching durable assistant occurrence", entry.ID)
			}
			if entry.Seq <= occurrence.assistant.Seq || entry.Type != EntryTypeTool || entry.ToolName != occurrence.toolName {
				return nil, fmt.Errorf("session: intent %q does not follow and match its assistant tool call", entry.ID)
			}
			if !durableEntryIsAncestor(entriesByID, occurrence.assistant.ID, entry.ID) {
				return nil, fmt.Errorf("session: intent %q is outside its assistant entry's parent chain", entry.ID)
			}
			if entry.WorkspaceID == "" || (occurrence.assistant.WorkspaceID != "" && entry.WorkspaceID != occurrence.assistant.WorkspaceID) {
				return nil, fmt.Errorf("session: intent %q has missing or mismatched workspace identity", entry.ID)
			}
			if occurrence.intent != nil {
				return nil, fmt.Errorf("session: tool-call occurrence %q has multiple intent rows", key.toolCall)
			}
			batchKey := durableActionBatchKey{runID: entry.RunID, ordinal: *entry.AssistantTranscriptOrdinal, workspace: entry.WorkspaceID, branchID: entry.BranchID}
			if previous, exists := batchKeys[entry.ActionBatchID]; exists && previous != batchKey {
				return nil, fmt.Errorf("session: action batch %q mixes unrelated occurrences", entry.ActionBatchID)
			}
			batchKeys[entry.ActionBatchID] = batchKey
			if previous, exists := intentsByActionID[entry.ActionID]; exists && previous.intent != nil {
				return nil, fmt.Errorf("session: action ID %q is reused by intents %q and %q", entry.ActionID, previous.intent.ID, entry.ID)
			}
			occurrence.intent = entry
			occurrence.ref = PendingActionRef{
				SessionID:                  sessionID,
				WorkspaceID:                entry.WorkspaceID,
				RunID:                      entry.RunID,
				ActionBatchID:              entry.ActionBatchID,
				AssistantTranscriptOrdinal: *entry.AssistantTranscriptOrdinal,
				ToolCallID:                 entry.ToolCallID,
				ActionID:                   entry.ActionID,
			}
			intentsByActionID[entry.ActionID] = occurrence
		}
	}

	for i := range entries {
		entry := &entries[i]
		if entry.RecordVersion != 1 {
			continue
		}
		if entry.ActionState == ActionStateExecution {
			key, err := actionKeyForEntry(*entry, sessionID)
			if err != nil {
				return nil, err
			}
			occurrence, exists := occurrencesByKey[key]
			if !exists || occurrence.intent == nil || !sameActionReference(occurrence.ref, refFromEntry(*entry, sessionID)) {
				return nil, fmt.Errorf("session: execution row %q has no exact matching intent", entry.ID)
			}
			if entry.Seq <= occurrence.intent.Seq || entry.ToolName != occurrence.toolName {
				return nil, fmt.Errorf("session: execution row %q does not follow and match its intent", entry.ID)
			}
			if !durableEntryIsAncestor(entriesByID, occurrence.intent.ID, entry.ID) {
				return nil, fmt.Errorf("session: execution row %q is outside its intent's parent chain", entry.ID)
			}
		}
		if entry.Type == EntryTypeTool && entry.ActionState == "" {
			key, err := actionKeyForEntry(*entry, sessionID)
			if err != nil {
				return nil, err
			}
			occurrence, exists := occurrencesByKey[key]
			if !exists {
				return nil, fmt.Errorf("session: receipt %q has no matching assistant tool call", entry.ID)
			}
			if entry.Seq <= occurrence.assistant.Seq || entry.ToolName != occurrence.toolName {
				return nil, fmt.Errorf("session: receipt %q does not follow and match its assistant tool call", entry.ID)
			}
			if !durableEntryIsAncestor(entriesByID, occurrence.assistant.ID, entry.ID) {
				return nil, fmt.Errorf("session: receipt %q is outside its assistant entry's parent chain", entry.ID)
			}
			if occurrence.receipt != nil {
				return nil, fmt.Errorf("session: tool-call occurrence %q has multiple receipts", key.toolCall)
			}
			if (entry.ActionBatchID == "") != (entry.ActionID == "") {
				return nil, fmt.Errorf("session: receipt %q has partial reservation identity", entry.ID)
			}
			if entry.ActionID == "" {
				if occurrence.intent != nil || entry.DispatchState != DispatchStateNotDispatched {
					return nil, fmt.Errorf("session: unreserved receipt %q must prove not-dispatched and have no intent", entry.ID)
				}
			} else if occurrence.intent == nil || !sameActionReference(occurrence.ref, refFromEntry(*entry, sessionID)) {
				return nil, fmt.Errorf("session: receipt %q has no exact matching intent", entry.ID)
			} else if !durableEntryIsAncestor(entriesByID, occurrence.intent.ID, entry.ID) {
				return nil, fmt.Errorf("session: receipt %q is outside its intent's parent chain", entry.ID)
			}
			occurrence.receipt = entry
		}
	}

	for i := range entries {
		entry := &entries[i]
		if entry.RecordVersion != 1 {
			continue
		}
		if entry.CheckpointKind == CheckpointReconciliation {
			key, err := actionKeyForEntry(*entry, sessionID)
			if err != nil {
				return nil, err
			}
			occurrence, exists := occurrencesByKey[key]
			if !exists || occurrence.intent == nil || !sameActionReference(occurrence.ref, refFromEntry(*entry, sessionID)) {
				return nil, fmt.Errorf("session: reconciliation checkpoint %q has no exact matching intent", entry.ID)
			}
			if entry.Type != EntryTypeCheckpoint || entry.Seq <= occurrence.intent.Seq || !durableEntryIsAncestor(entriesByID, occurrence.intent.ID, entry.ID) {
				return nil, fmt.Errorf("session: reconciliation checkpoint %q is outside its intent's later parent chain", entry.ID)
			}
		}
		if entry.CheckpointKind != CheckpointExecutionResolution {
			continue
		}
		if entry.ExecutionResolution == nil {
			return nil, fmt.Errorf("session: resolution checkpoint %q has no typed proof", entry.ID)
		}
		key, err := actionKeyForEntry(*entry, sessionID)
		if err != nil {
			return nil, err
		}
		occurrence, exists := occurrencesByKey[key]
		if !exists || occurrence.intent == nil || !sameActionReference(occurrence.ref, refFromEntry(*entry, sessionID)) {
			return nil, fmt.Errorf("session: resolution checkpoint %q has no exact matching intent", entry.ID)
		}
		if occurrence.receipt == nil || entry.Seq <= occurrence.receipt.Seq {
			return nil, fmt.Errorf("session: resolution checkpoint %q lacks an earlier terminal receipt", entry.ID)
		}
		if occurrence.resolution != nil {
			return nil, fmt.Errorf("session: action %q has multiple execution-resolution checkpoints", entry.ActionID)
		}
		if err := validateExecutionResolutionEvidence(*entry.ExecutionResolution, entry.Seq, occurrence, executionRows, reconciliationRows, sessionID); err != nil {
			return nil, fmt.Errorf("session: invalid resolution checkpoint %q: %w", entry.ID, err)
		}
		if !durableEntryIsAncestor(entriesByID, occurrence.receipt.ID, entry.ID) {
			return nil, fmt.Errorf("session: resolution checkpoint %q is outside its receipt's parent chain", entry.ID)
		}
		validated := &ValidatedExecutionResolution{
			Action:            occurrence.ref,
			CheckpointEntryID: entry.ID,
			CheckpointSeq:     entry.Seq,
			Resolution:        *entry.ExecutionResolution,
		}
		occurrence.resolution = validated
		plan.resolutions = append(plan.resolutions, *validated)
	}

	for _, occurrence := range plan.occurrences {
		if occurrence.intent != nil && occurrence.resolution == nil {
			plan.pending = append(plan.pending, occurrence.ref)
			continue
		}
		if occurrence.intent == nil && occurrence.receipt == nil {
			if occurrence.assistant.WorkspaceID == "" {
				return nil, fmt.Errorf("session: unreserved open occurrence %q has no workspace identity", occurrence.assistant.ToolCalls[occurrence.callIndex].ID)
			}
			occurrence.ref = PendingActionRef{
				SessionID:                  sessionID,
				WorkspaceID:                occurrence.assistant.WorkspaceID,
				RunID:                      occurrence.assistant.RunID,
				AssistantTranscriptOrdinal: *occurrence.assistant.AssistantTranscriptOrdinal,
				ToolCallID:                 occurrence.assistant.ToolCalls[occurrence.callIndex].ID,
			}
			plan.pending = append(plan.pending, occurrence.ref)
		}
	}
	return plan, nil
}

func durableEntryIsAncestor(entries map[string]*SessionEntry, ancestorID, descendantID string) bool {
	if ancestorID == "" || descendantID == "" || ancestorID == descendantID {
		return false
	}
	seen := make(map[string]struct{})
	for currentID := descendantID; currentID != ""; {
		if _, exists := seen[currentID]; exists {
			return false
		}
		seen[currentID] = struct{}{}
		entry, exists := entries[currentID]
		if !exists {
			return false
		}
		if entry.ParentID == ancestorID {
			return true
		}
		currentID = entry.ParentID
	}
	return false
}

func buildRecoveryReceipts(
	plan *durableRecoveryPlan,
	decisions []PendingActionDecision,
	sessionID string,
	activeBranch string,
	activeLeaf string,
	entries map[string]*SessionEntry,
) ([]SessionEntry, error) {
	if plan == nil || len(plan.pending) != len(decisions) {
		return nil, errors.New("session: recovery decisions do not match the pending action list")
	}
	occurrences := make(map[actionOccurrenceKey]*durableActionOccurrence, len(plan.occurrences))
	for _, occurrence := range plan.occurrences {
		key := actionOccurrenceKey{
			sessionID: sessionID,
			runID:     occurrence.ref.RunID,
			ordinal:   occurrence.ref.AssistantTranscriptOrdinal,
			toolCall:  occurrence.ref.ToolCallID,
		}
		occurrences[key] = occurrence
	}
	parentID := activeLeaf
	nextTranscriptIndex := make(map[string]uint64, len(plan.nextTranscriptIndex))
	for runID, index := range plan.nextTranscriptIndex {
		nextTranscriptIndex[runID] = index
	}
	recoveryRows := make([]SessionEntry, 0, len(decisions))
	for i, ref := range plan.pending {
		decision := decisions[i]
		if decision.Action != ref {
			return nil, fmt.Errorf("session: recovery action reference changed at index %d", i)
		}
		occurrence := occurrences[actionOccurrenceKey{
			sessionID: sessionID,
			runID:     ref.RunID,
			ordinal:   ref.AssistantTranscriptOrdinal,
			toolCall:  ref.ToolCallID,
		}]
		if occurrence == nil {
			return nil, fmt.Errorf("session: recovery action %q has no local occurrence", ref.ToolCallID)
		}
		if !durableEntryOnPath(entries, occurrence.assistant.ID, activeLeaf) {
			return nil, fmt.Errorf("session: open action occurrence %q is outside the active branch", ref.ToolCallID)
		}
		if occurrence.receipt != nil {
			if err := validateReceiptAgainstMarker(*occurrence.receipt, decision.MarkerState); err != nil {
				return nil, fmt.Errorf("session: existing receipt %q conflicts with recovery marker: %w", occurrence.receipt.ID, err)
			}
			continue
		}

		dispatchState := DispatchStateNotDispatched
		outcome := core.ReceiptOutcomeInterrupted
		content := "Interrupted before tool dispatch; recovery did not replay the action."
		switch decision.MarkerState {
		case DispatchMarkerAbsent:
		case DispatchMarkerPresent, DispatchMarkerResolved:
			if ref.ActionID == "" {
				return nil, errors.New("session: unreserved occurrence cannot be reconciled to a dispatch marker")
			}
			dispatchState = DispatchStateDispatched
			outcome = core.ReceiptOutcomeUnknown
			content = "Tool outcome is unknown after restart; recovery did not replay the action."
		case DispatchMarkerUnverifiable:
			return nil, fmt.Errorf("session: dispatch marker for action %q is unverifiable", ref.ActionID)
		default:
			return nil, fmt.Errorf("session: invalid dispatch marker state %d", decision.MarkerState)
		}
		if occurrence.intent != nil && ref.ActionID == "" {
			return nil, errors.New("session: reserved intent lost its action ID during recovery")
		}
		index := nextTranscriptIndex[ref.RunID]
		if index == ^uint64(0) {
			return nil, fmt.Errorf("session: run %q transcript index is exhausted", ref.RunID)
		}
		nextTranscriptIndex[ref.RunID] = index + 1
		ordinal := ref.AssistantTranscriptOrdinal
		timestamp := occurrence.assistant.Timestamp
		if timestamp.IsZero() {
			timestamp = time.Unix(0, 0).UTC()
		}
		entryID, err := recoveryReceiptID(ref)
		if err != nil {
			return nil, err
		}
		recoveryRows = append(recoveryRows, SessionEntry{
			ID:                         entryID,
			ParentID:                   parentID,
			Type:                       EntryTypeTool,
			Timestamp:                  timestamp,
			Content:                    content,
			ToolCallID:                 ref.ToolCallID,
			ToolName:                   occurrence.toolName,
			BranchID:                   activeBranch,
			RunID:                      ref.RunID,
			WorkspaceID:                ref.WorkspaceID,
			ActionBatchID:              ref.ActionBatchID,
			ActionID:                   ref.ActionID,
			AssistantTranscriptOrdinal: &ordinal,
			TranscriptIndex:            &index,
			DispatchState:              dispatchState,
			ReceiptOutcome:             string(outcome),
		})
		parentID = entryID
	}
	return recoveryRows, nil
}

func durableEntryOnPath(entries map[string]*SessionEntry, entryID, leafID string) bool {
	return entryID == leafID || durableEntryIsAncestor(entries, entryID, leafID)
}

func validateReceiptAgainstMarker(receipt SessionEntry, marker DispatchMarkerState) error {
	switch marker {
	case DispatchMarkerAbsent:
		if receipt.DispatchState != DispatchStateNotDispatched {
			return errors.New("absent marker conflicts with an already-dispatched receipt")
		}
	case DispatchMarkerPresent, DispatchMarkerResolved:
		if receipt.DispatchState != DispatchStateDispatched {
			return errors.New("present marker conflicts with a not-dispatched receipt")
		}
	case DispatchMarkerUnverifiable:
		return errors.New("dispatch marker is unverifiable")
	default:
		return fmt.Errorf("invalid marker state %d", marker)
	}
	return nil
}

func recoveryReceiptID(ref PendingActionRef) (string, error) {
	encoded, err := json.Marshal(ref)
	if err != nil {
		return "", fmt.Errorf("session: encode recovery action reference: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "recovery-" + hex.EncodeToString(digest[:]), nil
}

func actionKeyForEntry(entry SessionEntry, sessionID string) (actionOccurrenceKey, error) {
	if entry.RunID == "" || entry.AssistantTranscriptOrdinal == nil || entry.ToolCallID == "" {
		return actionOccurrenceKey{}, fmt.Errorf("session: action row %q has incomplete occurrence identity", entry.ID)
	}
	return actionOccurrenceKey{
		sessionID: sessionID,
		runID:     entry.RunID,
		ordinal:   *entry.AssistantTranscriptOrdinal,
		toolCall:  entry.ToolCallID,
	}, nil
}

func refFromEntry(entry SessionEntry, sessionID string) PendingActionRef {
	ref := PendingActionRef{
		SessionID:     sessionID,
		WorkspaceID:   entry.WorkspaceID,
		RunID:         entry.RunID,
		ActionBatchID: entry.ActionBatchID,
		ToolCallID:    entry.ToolCallID,
		ActionID:      entry.ActionID,
	}
	if entry.AssistantTranscriptOrdinal != nil {
		ref.AssistantTranscriptOrdinal = *entry.AssistantTranscriptOrdinal
	}
	return ref
}

func sameActionReference(left, right PendingActionRef) bool {
	return left == right
}

func validateExecutionResolutionEvidence(
	resolution core.ExecutionResolution,
	resolutionSeq uint64,
	occurrence *durableActionOccurrence,
	executionRows map[string]*SessionEntry,
	reconciliationRows map[string]*SessionEntry,
	sessionID string,
) error {
	if resolution.RunID != occurrence.ref.RunID || resolution.ActionID != occurrence.ref.ActionID || resolution.EvidenceRef == "" {
		return errors.New("typed resolution identity or evidence reference does not match the intent")
	}
	switch resolution.Evidence {
	case core.ResolutionNeverDispatched:
		if occurrence.receipt == nil || occurrence.receipt.DispatchState != DispatchStateNotDispatched || occurrence.receipt.ActionID != occurrence.ref.ActionID || (occurrence.receipt.ReceiptOutcome != string(core.ReceiptOutcomeInterrupted) && occurrence.receipt.ReceiptOutcome != string(core.ReceiptOutcomeError)) || resolution.EvidenceRef != occurrence.receipt.ID {
			return errors.New("never-dispatched proof lacks its exact not-dispatched receipt")
		}
	case core.ResolutionLocalResourcesStopped:
		proof, exists := executionRows[resolution.EvidenceRef]
		if !exists || proof.ExecutionIdentity == nil || proof.Seq >= resolutionSeq || !sameActionReference(occurrence.ref, refFromEntry(*proof, sessionID)) {
			return errors.New("local quiescence proof does not reference a matching execution row with an identity")
		}
	case core.ResolutionRemoteTerminal:
		if occurrence.receipt == nil || resolution.EvidenceRef != occurrence.receipt.ID || occurrence.receipt.DispatchState != DispatchStateDispatched || occurrence.receipt.ReceiptOutcome == string(core.ReceiptOutcomeUnknown) {
			return errors.New("remote terminal proof does not reference a known dispatched receipt")
		}
	case core.ResolutionOperatorReconciled:
		proof, exists := reconciliationRows[resolution.EvidenceRef]
		if !exists || proof.Seq <= occurrence.receipt.Seq || proof.Seq >= resolutionSeq || !sameActionReference(occurrence.ref, refFromEntry(*proof, sessionID)) {
			return errors.New("operator reconciliation proof does not reference a matching reconciliation checkpoint")
		}
	default:
		return fmt.Errorf("unsupported execution-resolution evidence %q", resolution.Evidence)
	}
	return nil
}
