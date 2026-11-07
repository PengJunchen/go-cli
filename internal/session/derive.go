package session

import (
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
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
	for index, entry := range entries {
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
				if allowed, reason := isOpenPriorProcessSessionResolution(entry, entries[:index], sessionID, currentGroup); allowed {
					continue
				} else {
					return fmt.Errorf("session: execution resolution %q does not belong to the current assistant tool-call group: %s", entry.ID, reason)
				}
			}
			if _, hasReceipt := receipted[key]; !hasReceipt {
				return fmt.Errorf("session: execution resolution %q precedes its current-group receipt", entry.ID)
			}
			continue
		}
		if entry.RecordVersion == 1 && entry.Type == EntryTypeCheckpoint && entry.CheckpointKind == CheckpointProcessSessionAction {
			if allowed, reason := isOpenProcessSessionActionCheckpoint(entry, entries[:index], sessionID, currentGroup, receipted); allowed {
				continue
			} else {
				return fmt.Errorf("session: process-session action checkpoint %q cannot cross an open tool-call receipt boundary: %s", entry.ID, reason)
			}
		}
		if entry.RecordVersion == 1 && entry.Type == EntryTypeCheckpoint && entry.CheckpointKind == CheckpointProcessSessionLease {
			allowed, reason := isOpenProcessSessionStartLease(entry, entries[:index], sessionID, currentGroup, open)
			if allowed {
				// The process start barrier synchronously journals the exact monotone
				// lease phases before its tool receipt exists. No other checkpoint or
				// action may cross this receipt boundary.
				continue
			}
			stoppedAllowed, stoppedReason := isOpenProcessSessionStoppedLease(entry, entries[:index], sessionID, currentGroup, receipted)
			if stoppedAllowed {
				continue
			}
			priorStoppedAllowed, priorStoppedReason := isOpenPriorProcessSessionStoppedLease(entry, entries[:index], sessionID, currentGroup)
			if priorStoppedAllowed {
				continue
			}
			activeAllowed, activeReason := isOpenProcessSessionActiveLease(entry, entries[:index], sessionID, currentGroup, receipted)
			if activeAllowed {
				continue
			}
			if entry.ProcessSessionLease != nil && entry.ProcessSessionLease.Phase == tools.ProcessSessionLeaseActive {
				reason = activeReason
			} else if entry.ProcessSessionLease != nil && entry.ProcessSessionLease.Phase == tools.ProcessSessionLeaseStopped && reason != "" {
				reason = stoppedReason
				if reason == "stopped lease does not name the current process-session start occurrence" {
					reason = priorStoppedReason
				}
			}
			return fmt.Errorf("session: process-session lease checkpoint %q cannot cross an open tool-call receipt boundary: %s", entry.ID, reason)
		}
		processSessionPhase := ""
		if entry.ProcessSessionLease != nil {
			processSessionPhase = string(entry.ProcessSessionLease.Phase)
		}
		return fmt.Errorf("session: entry %q (type %q, checkpoint %q, process-session phase %q) interleaves an open assistant tool-call group", entry.ID, entry.Type, entry.CheckpointKind, processSessionPhase)
	}
	if len(open) > 0 {
		return fmt.Errorf("%w: version 1 journal tail has %d unreceipted tool calls", errUnclosedToolCallGroup, len(open))
	}
	return nil
}

func isOpenProcessSessionStartLease(
	entry SessionEntry,
	priorEntries []SessionEntry,
	sessionID string,
	currentGroup map[actionOccurrenceKey]string,
	open map[actionOccurrenceKey]string,
) (bool, string) {
	lease := entry.ProcessSessionLease
	if lease == nil || (lease.Phase != tools.ProcessSessionLeaseGated &&
		lease.Phase != tools.ProcessSessionLeaseStopped && lease.Phase != tools.ProcessSessionLeaseUnknown) {
		return false, "phase is not an allowed pre-receipt origin phase"
	}
	if err := validateProcessSessionLeaseRow(entry); err != nil {
		return false, "lease row is invalid: " + err.Error()
	}
	key := actionOccurrenceKey{
		sessionID: sessionID, runID: lease.Action.RunID,
		ordinal: lease.Action.AssistantTranscriptOrdinal, toolCall: lease.Action.ToolCallID,
	}
	name, belongsToCurrentGroup := currentGroup[key]
	openName, occurrenceOpen := open[key]
	if !belongsToCurrentGroup || !occurrenceOpen || name != tools.ProcessSessionToolName || openName != name {
		return false, "lease does not match the open builtin process_session call"
	}
	plan, err := analyzeDurableActionJournal(priorEntries, sessionID)
	if err != nil {
		return false, "prior action journal is invalid"
	}
	for _, occurrence := range plan.occurrences {
		if occurrence.intent == nil || occurrence.intent.RunID != lease.Action.RunID || occurrence.intent.ActionID != lease.Action.ActionID {
			continue
		}
		if occurrence.toolName != tools.ProcessSessionToolName || occurrence.intent.ToolIdentity == nil ||
			occurrence.intent.ToolIdentity.RegistrationSource != tools.RegistrationSourceBuiltin {
			return false, "origin intent is not the builtin process_session tool"
		}
		operation, ok := processSessionOccurrenceOperation(occurrence)
		if !ok || operation != tools.ProcessSessionOperationStart {
			return false, "origin intent is not a start call"
		}
		expected, err := processSessionActionRefForIntent(*occurrence.intent, sessionID)
		if err != nil || expected != lease.Action {
			return false, "lease action reference differs from the frozen start intent"
		}
		var previous *tools.ProcessSessionLeaseSnapshot
		for index := range priorEntries {
			candidate := priorEntries[index].ProcessSessionLease
			if priorEntries[index].Type != EntryTypeCheckpoint || priorEntries[index].CheckpointKind != CheckpointProcessSessionLease || candidate == nil ||
				candidate.OwnerID != lease.OwnerID || candidate.RunID != lease.RunID || candidate.SessionID != lease.SessionID {
				continue
			}
			if !sameProcessSessionLeaseIdentity(*candidate, *lease) {
				return false, "lease identity differs from its preceding phase"
			}
			copy := tools.CloneProcessSessionLeaseSnapshot(*candidate)
			previous = &copy
		}
		if previous == nil {
			return lease.Phase == tools.ProcessSessionLeaseGated, "first process-session lease phase is not gated"
		}
		if !validProcessSessionLeaseTransition(previous.Phase, lease.Phase) {
			return false, "lease phase regresses or skips its recorded transition"
		}
		return true, ""
	}
	return false, "lease has no matching durable start intent"
}

func isOpenProcessSessionStoppedLease(
	entry SessionEntry,
	priorEntries []SessionEntry,
	sessionID string,
	currentGroup map[actionOccurrenceKey]string,
	receipted map[actionOccurrenceKey]struct{},
) (bool, string) {
	lease := entry.ProcessSessionLease
	if entry.RecordVersion != 1 || entry.Type != EntryTypeCheckpoint || entry.CheckpointKind != CheckpointProcessSessionLease ||
		lease == nil || lease.Phase != tools.ProcessSessionLeaseStopped {
		return false, "phase is not a stopped process-session lease"
	}
	if err := validateProcessSessionLeaseRow(entry); err != nil {
		return false, "stopped lease row is invalid: " + err.Error()
	}
	key := actionOccurrenceKey{
		sessionID: sessionID, runID: lease.Action.RunID,
		ordinal: lease.Action.AssistantTranscriptOrdinal, toolCall: lease.Action.ToolCallID,
	}
	if toolName, belongs := currentGroup[key]; !belongs || toolName != tools.ProcessSessionToolName {
		return false, "stopped lease does not name the current process-session start occurrence"
	}
	if _, hasReceipt := receipted[key]; !hasReceipt {
		return false, "stopped lease precedes its start receipt"
	}
	plan, err := analyzeDurableActionJournal(priorEntries, sessionID)
	if err != nil {
		return false, "prior action journal is invalid"
	}
	start, err := findProcessSessionActionOccurrence(plan, lease.Action, sessionID)
	if err != nil {
		return false, "stopped lease has no exact successful start receipt"
	}
	operation, ok := processSessionOccurrenceOperation(start)
	if !ok || operation != tools.ProcessSessionOperationStart || start.resolution != nil {
		return false, "stopped lease origin is not an unresolved start action"
	}
	previous, exists, reason := latestProcessSessionLeaseForAction(priorEntries, lease.SessionID, lease.Action)
	if !exists {
		return false, reason
	}
	if !sameProcessSessionLeaseIdentity(*previous.ProcessSessionLease, *lease) ||
		!validProcessSessionLeaseTransition(previous.ProcessSessionLease.Phase, lease.Phase) {
		return false, "stopped lease does not follow its exact prior identity and monotone phase"
	}
	return true, ""
}

// isOpenPriorProcessSessionStoppedLease permits only the asynchronous physical
// closure of a successful start from an earlier assistant group in the same
// run. The checkpoint is metadata; it does not grant runtime admission.
func isOpenPriorProcessSessionStoppedLease(
	entry SessionEntry,
	priorEntries []SessionEntry,
	sessionID string,
	currentGroup map[actionOccurrenceKey]string,
) (bool, string) {
	lease := entry.ProcessSessionLease
	if entry.RecordVersion != 1 || entry.Type != EntryTypeCheckpoint || entry.CheckpointKind != CheckpointProcessSessionLease ||
		lease == nil || lease.Phase != tools.ProcessSessionLeaseStopped {
		return false, "phase is not a stopped process-session lease"
	}
	if err := validateProcessSessionLeaseRow(entry); err != nil {
		return false, "stopped lease row is invalid: " + err.Error()
	}
	currentRunID, currentOrdinal, ok := openToolGroupCoordinates(currentGroup)
	if !ok || lease.RunID != currentRunID || lease.Action.RunID != currentRunID || lease.Action.AssistantTranscriptOrdinal >= currentOrdinal {
		return false, "stopped lease does not name an earlier process_session start in the current run"
	}
	if !validProcessSessionNativeIdentity(lease.Identity) || !lease.GroupAbsent || !lease.LeaderWaited || !lease.ReadersJoined {
		return false, "stopped lease lacks a valid native identity or complete physical closure proof"
	}
	plan, err := analyzeDurableActionJournal(priorEntries, sessionID)
	if err != nil {
		return false, "prior action journal is invalid"
	}
	start, err := findProcessSessionActionOccurrence(plan, lease.Action, sessionID)
	if err != nil {
		return false, "stopped lease has no exact successful start receipt"
	}
	operation, ok := processSessionOccurrenceOperation(start)
	if !ok || operation != tools.ProcessSessionOperationStart || start.resolution != nil {
		return false, "stopped lease origin is not an unresolved start action"
	}
	if start.intent.WorkspaceID != lease.WorkspaceID || start.intent.OwnerGeneration != lease.OwnerGeneration {
		return false, "stopped lease differs from its frozen start workspace or generation"
	}
	if err := validatePriorProcessSessionLeaseHistory(priorEntries, sessionID, lease.Action, *lease, start.receipt.Seq, entry.Seq); err != nil {
		return false, err.Error()
	}
	return true, ""
}

// isOpenPriorProcessSessionResolution permits the matching asynchronous
// LocalResourcesStopped proof after its Stopped lease has already been
// journaled in the prefix. All proof rows must precede this candidate row.
func isOpenPriorProcessSessionResolution(
	entry SessionEntry,
	priorEntries []SessionEntry,
	sessionID string,
	currentGroup map[actionOccurrenceKey]string,
) (bool, string) {
	if entry.RecordVersion != 1 || entry.Type != EntryTypeCheckpoint || entry.CheckpointKind != CheckpointExecutionResolution ||
		entry.ExecutionResolution == nil || entry.AssistantTranscriptOrdinal == nil {
		return false, "resolution metadata is incomplete"
	}
	resolution := entry.ExecutionResolution
	if resolution.Evidence != core.ResolutionLocalResourcesStopped || resolution.ProcessSessionClosureRef == "" ||
		resolution.EvidenceRef != resolution.ProcessSessionClosureRef {
		return false, "prior-group resolution is not an exact process-session closure"
	}
	currentRunID, currentOrdinal, ok := openToolGroupCoordinates(currentGroup)
	if !ok || entry.RunID != currentRunID || resolution.RunID != currentRunID || *entry.AssistantTranscriptOrdinal >= currentOrdinal {
		return false, "resolution does not name an earlier action in the current run"
	}
	plan, err := analyzeDurableActionJournal(priorEntries, sessionID)
	if err != nil {
		return false, "prior action journal is invalid"
	}
	var start *durableActionOccurrence
	for _, occurrence := range plan.occurrences {
		if occurrence.intent == nil || occurrence.intent.RunID != entry.RunID || occurrence.intent.ActionID != entry.ActionID {
			continue
		}
		if start != nil {
			return false, "resolution origin action is ambiguous in the prior journal"
		}
		start = occurrence
	}
	if start == nil || start.intent.ToolName != tools.ProcessSessionToolName || start.intent.ToolIdentity == nil ||
		start.intent.ToolIdentity.RegistrationSource != tools.RegistrationSourceBuiltin || start.toolName != tools.ProcessSessionToolName {
		return false, "resolution origin is not the builtin process_session action"
	}
	if start.intent.WorkspaceID != entry.WorkspaceID || start.intent.ActionBatchID != entry.ActionBatchID ||
		start.intent.OwnerGeneration != entry.OwnerGeneration || start.intent.ToolCallID != entry.ToolCallID ||
		start.intent.AssistantTranscriptOrdinal == nil || *start.intent.AssistantTranscriptOrdinal != *entry.AssistantTranscriptOrdinal {
		return false, "resolution coordinates differ from the frozen start action"
	}
	operation, ok := processSessionOccurrenceOperation(start)
	if !ok || operation != tools.ProcessSessionOperationStart || start.resolution != nil || start.receipt == nil ||
		start.receipt.DispatchState != DispatchStateDispatched || start.receipt.ReceiptOutcome != core.ReceiptOutcomeSuccess {
		return false, "resolution origin is not an unresolved successful dispatched start"
	}
	expectedAction, err := processSessionActionRefForIntent(*start.intent, sessionID)
	if err != nil {
		return false, "resolution origin action reference is invalid"
	}
	closureIndex := -1
	for index := range priorEntries {
		if priorEntries[index].ID == resolution.ProcessSessionClosureRef {
			closureIndex = index
			break
		}
	}
	if closureIndex < 0 {
		return false, "resolution closure row is not in the prior prefix"
	}
	closure := priorEntries[closureIndex]
	if err := validateProcessSessionLeaseRow(closure); err != nil {
		return false, "resolution closure row is invalid: " + err.Error()
	}
	lease := closure.ProcessSessionLease
	if closure.Seq >= entry.Seq || lease.Phase != tools.ProcessSessionLeaseStopped || lease.Action != expectedAction ||
		lease.RunID != currentRunID || lease.WorkspaceID != start.intent.WorkspaceID || lease.OwnerGeneration != start.intent.OwnerGeneration ||
		!lease.GroupAbsent || !lease.LeaderWaited || !lease.ReadersJoined || !validProcessSessionNativeIdentity(lease.Identity) {
		return false, "resolution does not cite the exact prior positive Stopped lease"
	}
	if err := validatePriorProcessSessionLeaseHistory(priorEntries[:closureIndex], sessionID, expectedAction, *lease, start.receipt.Seq, closure.Seq); err != nil {
		return false, err.Error()
	}
	candidateEntries := append(append([]SessionEntry(nil), priorEntries...), entry)
	validatedPlan, err := analyzeDurableActionJournal(candidateEntries, sessionID)
	if err != nil {
		return false, "resolution does not validate against its exact prefix: " + err.Error()
	}
	for _, occurrence := range validatedPlan.occurrences {
		if occurrence.intent == nil || occurrence.ref != start.ref {
			continue
		}
		if occurrence.resolution == nil || occurrence.resolution.CheckpointEntryID != entry.ID {
			return false, "resolution prefix did not bind to its exact start action"
		}
		return true, ""
	}
	return false, "resolution prefix lost its exact start action"
}

func openToolGroupCoordinates(currentGroup map[actionOccurrenceKey]string) (string, uint64, bool) {
	if len(currentGroup) == 0 {
		return "", 0, false
	}
	runID := ""
	var ordinal uint64
	for key := range currentGroup {
		if runID == "" {
			runID, ordinal = key.runID, key.ordinal
			continue
		}
		if key.runID != runID || key.ordinal != ordinal {
			return "", 0, false
		}
	}
	return runID, ordinal, runID != ""
}

func validatePriorProcessSessionLeaseHistory(
	entries []SessionEntry,
	sessionID string,
	action tools.ProcessSessionActionRef,
	stopped tools.ProcessSessionLeaseSnapshot,
	receiptSeq uint64,
	closureSeq uint64,
) error {
	var previous *tools.ProcessSessionLeaseSnapshot
	var previousSeq uint64
	startActionRows := 0
	for _, entry := range entries {
		if entry.Type == EntryTypeCheckpoint && entry.CheckpointKind == CheckpointProcessSessionAction && entry.ProcessSessionAction != nil &&
			entry.ProcessSessionAction.Action == action && entry.ProcessSessionAction.LeaseAction == action &&
			entry.ProcessSessionAction.LeaseSessionID == stopped.SessionID && entry.ProcessSessionAction.Operation == tools.ProcessSessionOperationStart {
			if entry.RecordVersion != 1 || !processSessionActionEntryMatches(entry, action, sessionID) || entry.Seq <= receiptSeq || entry.Seq >= closureSeq {
				return errors.New("session: earlier stopped lease has an invalid start-action checkpoint")
			}
			startActionRows++
		}
		lease := entry.ProcessSessionLease
		if entry.RecordVersion != 1 || entry.Type != EntryTypeCheckpoint || entry.CheckpointKind != CheckpointProcessSessionLease ||
			lease == nil || lease.SessionID != stopped.SessionID || lease.Action != action {
			continue
		}
		if err := validateProcessSessionLeaseRow(entry); err != nil {
			return fmt.Errorf("session: prior process-session lease is invalid: %w", err)
		}
		if lease.Phase == tools.ProcessSessionLeaseUnknown || !validProcessSessionNativeIdentity(lease.Identity) {
			return errors.New("session: prior process-session lease has unknown phase or invalid native identity")
		}
		if previous == nil {
			if lease.Phase != tools.ProcessSessionLeaseGated {
				return errors.New("session: earlier stopped lease history does not begin with its gated identity")
			}
		} else if !sameProcessSessionLeaseIdentity(*previous, *lease) || entry.Seq <= previousSeq ||
			!validProcessSessionLeaseTransition(previous.Phase, lease.Phase) {
			return errors.New("session: earlier stopped lease history changes identity or phase order")
		}
		copy := tools.CloneProcessSessionLeaseSnapshot(*lease)
		previous = &copy
		previousSeq = entry.Seq
	}
	if startActionRows != 1 {
		return errors.New("session: earlier stopped lease requires one exact start-action checkpoint")
	}
	if previous == nil || previous.Phase == tools.ProcessSessionLeaseUnknown ||
		(previous.Phase != tools.ProcessSessionLeaseGated && previous.Phase != tools.ProcessSessionLeaseActive &&
			previous.Phase != tools.ProcessSessionLeaseDelivered && previous.Phase != tools.ProcessSessionLeaseStopping) ||
		!sameProcessSessionLeaseIdentity(*previous, stopped) || closureSeq <= previousSeq ||
		!validProcessSessionLeaseTransition(previous.Phase, stopped.Phase) {
		return errors.New("session: stopped lease does not follow a permitted exact prior phase")
	}
	return nil
}

func validProcessSessionNativeIdentity(identity *tools.ProcessExecutionIdentity) bool {
	if identity == nil {
		return false
	}
	return (core.ExecutionIdentity{
		Kind: core.ExecutionIdentityProcessGroup, Platform: identity.Platform,
		HostGeneration: identity.HostGeneration, LeaderPID: identity.LeaderPID,
		ProcessStartToken: identity.ProcessStartToken, ProcessGroupID: identity.ProcessGroupID,
	}).Validate() == nil
}

func isOpenProcessSessionActionCheckpoint(
	entry SessionEntry,
	priorEntries []SessionEntry,
	sessionID string,
	currentGroup map[actionOccurrenceKey]string,
	receipted map[actionOccurrenceKey]struct{},
) (bool, string) {
	record := entry.ProcessSessionAction
	if entry.Type != EntryTypeCheckpoint || entry.CheckpointKind != CheckpointProcessSessionAction || record == nil {
		return false, "action checkpoint metadata is missing"
	}
	if !processSessionActionEntryMatches(entry, record.Action, sessionID) || record.LeaseSessionID == "" {
		return false, "action checkpoint coordinates do not match its immutable action"
	}
	key := actionOccurrenceKey{
		sessionID: sessionID, runID: record.Action.RunID,
		ordinal: record.Action.AssistantTranscriptOrdinal, toolCall: record.Action.ToolCallID,
	}
	if toolName, belongs := currentGroup[key]; !belongs || toolName != tools.ProcessSessionToolName {
		return false, "action does not belong to the current builtin process_session call group"
	}
	if _, hasReceipt := receipted[key]; !hasReceipt {
		return false, "action checkpoint precedes its own synced receipt"
	}
	plan, err := analyzeDurableActionJournal(priorEntries, sessionID)
	if err != nil {
		return false, "prior action journal is invalid"
	}
	actionOccurrence, err := findProcessSessionActionOccurrence(plan, record.Action, sessionID)
	if err != nil {
		return false, "action does not match a successful trusted builtin receipt"
	}
	operation, ok := processSessionOccurrenceOperation(actionOccurrence)
	if !ok || operation != record.Operation {
		return false, "action operation differs from its frozen tool call"
	}
	leaseOccurrence, err := findProcessSessionActionOccurrence(plan, record.LeaseAction, sessionID)
	if err != nil {
		return false, "lease origin does not match a successful trusted builtin receipt"
	}
	leaseOperation, ok := processSessionOccurrenceOperation(leaseOccurrence)
	if !ok || leaseOperation != tools.ProcessSessionOperationStart || record.LeaseAction.SessionID != record.Action.SessionID ||
		record.LeaseAction.WorkspaceID != record.Action.WorkspaceID || record.LeaseAction.RunID != record.Action.RunID ||
		record.LeaseAction.OwnerGeneration != record.Action.OwnerGeneration {
		return false, "lease origin is not the exact same-run process-session start"
	}
	if record.Operation == tools.ProcessSessionOperationStart {
		if record.Action != record.LeaseAction {
			return false, "start action differs from its lease origin"
		}
	} else {
		if record.Action == record.LeaseAction {
			return false, "control action reuses its start identity"
		}
		args, ok := actionOccurrence.assistant.ToolCalls[actionOccurrence.callIndex].Args.(map[string]any)
		sessionIDArg, _ := args["session_id"].(string)
		if !ok || sessionIDArg != record.LeaseSessionID {
			return false, "control action does not name its exact process-session lease"
		}
	}
	previous, exists, reason := latestProcessSessionLeaseForAction(priorEntries, record.LeaseSessionID, record.LeaseAction)
	if !exists {
		return false, reason
	}
	if record.Operation == tools.ProcessSessionOperationStart {
		if previous.ProcessSessionLease.Phase != tools.ProcessSessionLeaseGated && previous.ProcessSessionLease.Phase != tools.ProcessSessionLeaseStopped {
			return false, "start action checkpoint must follow only its gated or naturally stopped lease"
		}
	} else if previous.ProcessSessionLease.Phase != tools.ProcessSessionLeaseActive &&
		previous.ProcessSessionLease.Phase != tools.ProcessSessionLeaseDelivered &&
		previous.ProcessSessionLease.Phase != tools.ProcessSessionLeaseStopping &&
		previous.ProcessSessionLease.Phase != tools.ProcessSessionLeaseStopped {
		return false, "control action checkpoint has no prior live or closed lease state"
	}
	if previous.WorkspaceID != record.Action.WorkspaceID || previous.RunID != record.Action.RunID || previous.OwnerGeneration != record.Action.OwnerGeneration {
		return false, "lease origin scope differs from the action"
	}
	return true, ""
}

func isOpenProcessSessionActiveLease(
	entry SessionEntry,
	priorEntries []SessionEntry,
	sessionID string,
	currentGroup map[actionOccurrenceKey]string,
	receipted map[actionOccurrenceKey]struct{},
) (bool, string) {
	lease := entry.ProcessSessionLease
	if entry.Type != EntryTypeCheckpoint || entry.CheckpointKind != CheckpointProcessSessionLease || lease == nil || lease.Phase != tools.ProcessSessionLeaseActive {
		return false, "phase is not an active process-session lease"
	}
	if err := validateProcessSessionLeaseRow(entry); err != nil {
		return false, "active lease row is invalid"
	}
	key := actionOccurrenceKey{
		sessionID: sessionID, runID: lease.Action.RunID,
		ordinal: lease.Action.AssistantTranscriptOrdinal, toolCall: lease.Action.ToolCallID,
	}
	if toolName, belongs := currentGroup[key]; !belongs || toolName != tools.ProcessSessionToolName {
		return false, "active lease does not name the current process-session start occurrence"
	}
	if _, hasReceipt := receipted[key]; !hasReceipt {
		return false, "active lease precedes the start receipt"
	}
	plan, err := analyzeDurableActionJournal(priorEntries, sessionID)
	if err != nil {
		return false, "prior action journal is invalid"
	}
	start, err := findProcessSessionActionOccurrence(plan, lease.Action, sessionID)
	if err != nil {
		return false, "active lease has no exact successful start receipt"
	}
	operation, ok := processSessionOccurrenceOperation(start)
	if !ok || operation != tools.ProcessSessionOperationStart || start.resolution != nil {
		return false, "active lease origin is not an unresolved start action"
	}
	var execution *SessionEntry
	for _, candidate := range start.executions {
		if candidate.DispatchState != core.DispatchStateDispatched || candidate.ResourceState != core.RunExecutionDeliveredLive || candidate.ProcessSessionLease == nil {
			continue
		}
		if execution != nil || candidate.ProcessSessionLease.Phase != tools.ProcessSessionLeaseActive ||
			!sameProcessSessionLeaseIdentity(*candidate.ProcessSessionLease, *lease) || candidate.CompletionProof != "" {
			return false, "active lease does not match one exact delivered-live execution"
		}
		execution = candidate
	}
	if execution == nil {
		return false, "active lease has no delivered-live execution"
	}
	actionRows := 0
	for index, candidate := range priorEntries {
		record := candidate.ProcessSessionAction
		if candidate.RecordVersion != 1 || candidate.Type != EntryTypeCheckpoint || candidate.CheckpointKind != CheckpointProcessSessionAction || record == nil ||
			record.LeaseSessionID != lease.SessionID || record.LeaseAction != lease.Action || record.Action != lease.Action || record.Operation != tools.ProcessSessionOperationStart {
			continue
		}
		if candidate.Seq <= start.receipt.Seq || candidate.Seq >= entry.Seq || !processSessionActionEntryMatches(candidate, lease.Action, sessionID) {
			return false, "start action checkpoint coordinates or sequence are invalid"
		}
		if allowed, reason := isOpenProcessSessionActionCheckpoint(candidate, priorEntries[:index], sessionID, currentGroup, receipted); !allowed {
			return false, "start action checkpoint is invalid: " + reason
		}
		actionRows++
	}
	if actionRows != 1 {
		return false, "active lease requires exactly one prior start-action checkpoint"
	}
	previous, exists, reason := latestProcessSessionLeaseForAction(priorEntries, lease.SessionID, lease.Action)
	if !exists {
		return false, reason
	}
	if previous.ProcessSessionLease.Phase != tools.ProcessSessionLeaseGated || !sameProcessSessionLeaseIdentity(*previous.ProcessSessionLease, *lease) ||
		!validProcessSessionLeaseTransition(previous.ProcessSessionLease.Phase, lease.Phase) {
		return false, "active lease does not follow its exact gated identity"
	}
	return true, ""
}

func processSessionActionEntryMatches(entry SessionEntry, action tools.ProcessSessionActionRef, sessionID string) bool {
	return entry.RecordVersion == 1 && entry.Type == EntryTypeCheckpoint && entry.CheckpointKind == CheckpointProcessSessionAction &&
		entry.SessionID == sessionID && entry.RunID == action.RunID && entry.WorkspaceID == action.WorkspaceID &&
		entry.ActionBatchID == action.ActionBatchID && entry.ActionID == action.ActionID && entry.OwnerGeneration == action.OwnerGeneration &&
		entry.ToolCallID == action.ToolCallID && entry.ToolName == tools.ProcessSessionToolName && entry.AssistantTranscriptOrdinal != nil &&
		*entry.AssistantTranscriptOrdinal == action.AssistantTranscriptOrdinal
}

func latestProcessSessionLeaseForAction(
	entries []SessionEntry,
	leaseSessionID string,
	action tools.ProcessSessionActionRef,
) (SessionEntry, bool, string) {
	var latest SessionEntry
	found := false
	ownerID := ""
	for _, entry := range entries {
		lease := entry.ProcessSessionLease
		if entry.RecordVersion != 1 || entry.Type != EntryTypeCheckpoint || entry.CheckpointKind != CheckpointProcessSessionLease || lease == nil ||
			lease.SessionID != leaseSessionID || lease.Action != action {
			continue
		}
		if err := validateProcessSessionLeaseRow(entry); err != nil {
			return SessionEntry{}, false, "prior lease row is invalid"
		}
		if ownerID != "" && ownerID != lease.OwnerID {
			return SessionEntry{}, false, "lease action has conflicting owner identities"
		}
		ownerID = lease.OwnerID
		if found && entry.Seq <= latest.Seq {
			return SessionEntry{}, false, "prior lease phases are not strictly ordered"
		}
		latest, found = entry, true
	}
	if !found {
		return SessionEntry{}, false, "lease origin has no prior identity checkpoint"
	}
	return latest, true, ""
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
	anchoredRunBoundaries := make(map[string]int)
	for i, entry := range entries {
		if entry.Type != EntryTypeCompaction {
			continue
		}
		lastCompaction = i
		if entry.RecordVersion != 1 {
			if entry.RetainedInputEntryID != "" || entry.RetainedInputPayloadDigest != "" || entry.RetainedInputDigest != "" {
				return nil, fmt.Errorf("session: legacy compaction entry %q carries an anchor", entry.ID)
			}
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
		if entry.RetainedInputEntryID != "" {
			if previous, exists := anchoredRunBoundaries[entry.RunID]; exists && boundaryIndex <= previous {
				return nil, fmt.Errorf("session: anchored compaction entry %q does not advance", entry.ID)
			}
			if err := validateAnchoredCompaction(entries, entry, i, entry.SessionID, entry.BranchID); err != nil {
				return nil, fmt.Errorf("session: invalid anchored compaction entry %q: %w", entry.ID, err)
			}
			anchoredRunBoundaries[entry.RunID] = boundaryIndex
		} else if _, err := validateActiveProcessSessionReferences(entries, entry, i, entry.SessionID); err != nil {
			return nil, fmt.Errorf("session: invalid process-session compaction entry %q: %w", entry.ID, err)
		}
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
	if marker.RetainedInputEntryID != "" {
		anchorIndex, exists := byID[marker.RetainedInputEntryID]
		if !exists || anchorIndex >= boundaryIndex {
			return nil, errors.New("session: anchored compaction lost its original input")
		}
		result = append(result, entries[anchorIndex])
	}
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
		if entry.Type == EntryTypeAssistant && len(entry.ToolCalls) > 0 {
			if len(open) > 0 {
				return fmt.Errorf("session: assistant entry %q precedes tool receipts", entry.ID)
			}
			ordinal := uint64(0)
			if entry.AssistantTranscriptOrdinal != nil {
				ordinal = *entry.AssistantTranscriptOrdinal
			} else if entry.RecordVersion == 1 {
				return errors.New("session: version-one assistant is missing its occurrence")
			}
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
			ordinal := uint64(0)
			if entry.AssistantTranscriptOrdinal != nil {
				ordinal = *entry.AssistantTranscriptOrdinal
			} else if entry.RecordVersion == 1 {
				return fmt.Errorf("session: tool receipt %q has no assistant occurrence ordinal", entry.ID)
			}
			key := occurrence{runID: entry.RunID, ordinal: ordinal, toolCallID: entry.ToolCallID}
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
		if entry.ActionState != "" || (entry.RecordVersion == 0 && (entry.ToolCallID == "" || entry.ToolName == "")) {
			return core.AgentMessage{}, false
		}
		message.Role = "tool"
	case EntryTypeCompaction:
		message.Role = "user"
		message.Runtime.CompactionMarker = true
		content := entry.Summary
		if content == "" {
			content = entry.Content
		}
		content, err := core.ActiveProcessSessionSummaryContext(content, entry.ActiveProcessSessions)
		if err != nil {
			return core.AgentMessage{}, false
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
