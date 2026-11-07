package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

var _ core.RunCommitProcessSessionSink = (*SessionRunCommitSink)(nil)

// RecordProcessSessionLease syncs one process lease transition to the session
// journal and then mirrors the same validated fact into the workspace index.
func (s *SessionRunCommitSink) RecordProcessSessionLease(ctx context.Context, snapshot tools.ProcessSessionLeaseSnapshot) error {
	if s == nil {
		return errors.New("session: process-session lease requires a run-commit sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, snapshot.RunID); err != nil {
		return err
	}
	if err := s.rejectAfterTerminalLocked(); err != nil {
		return err
	}
	_, err := s.recordProcessSessionLeaseLocked(ctx, snapshot, false)
	return err
}

// CompleteProcessSessionAction syncs the exact process_session call and its
// receipt after dispatch. The logical lease identity is matched to the actual
// original start intent before this row can be used by compaction recovery.
func (s *SessionRunCommitSink) CompleteProcessSessionAction(
	ctx context.Context,
	lease tools.ProcessSessionLeaseSnapshot,
	action tools.ProcessSessionActionRef,
	operation string,
) error {
	if s == nil {
		return errors.New("session: process-session action requires a run-commit sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, action.RunID); err != nil {
		return err
	}
	if err := s.rejectAfterTerminalLocked(); err != nil {
		return err
	}
	if err := validateProcessSessionSnapshot(lease); err != nil {
		return err
	}
	if err := s.validateProcessSessionOriginLocked(lease); err != nil {
		return err
	}
	if operation != tools.ProcessSessionOperationStart && operation != tools.ProcessSessionOperationRead && operation != tools.ProcessSessionOperationStatus && operation != tools.ProcessSessionOperationStop {
		return fmt.Errorf("session: unsupported process-session operation %q", operation)
	}
	intent, occurrence, err := s.processSessionIntentForActionLocked(action)
	if err != nil {
		return err
	}
	actualOperation, ok := processSessionOccurrenceOperation(occurrence)
	if !ok || actualOperation != operation {
		return errors.New("session: process-session action operation differs from its durable tool call")
	}
	if operation == tools.ProcessSessionOperationStart {
		if action != lease.Action {
			return errors.New("session: process-session start action differs from the lease origin")
		}
	} else {
		if action == lease.Action {
			return errors.New("session: process-session control action reuses the start action identity")
		}
		args, ok := occurrence.assistant.ToolCalls[occurrence.callIndex].Args.(map[string]any)
		sessionID, _ := args["session_id"].(string)
		if !ok || sessionID != lease.SessionID {
			return errors.New("session: process-session control action does not name its exact lease")
		}
	}
	ref := refFromEntry(intent, s.sessionID)
	receipt, err := s.findReceiptForIntentLocked(ref, intent)
	if err != nil {
		return err
	}
	if receipt.DispatchState != core.DispatchStateDispatched || receipt.ReceiptOutcome != core.ReceiptOutcomeSuccess || receipt.Seq == 0 {
		return errors.New("session: process-session action requires a synced successful dispatched receipt")
	}
	record := tools.ProcessSessionActionRecord{
		LeaseSessionID: lease.SessionID,
		LeaseAction:    lease.Action,
		Action:         action,
		Operation:      operation,
	}
	rowID, err := stableRunCommitEntryID("process-session-action-v1", record)
	if err != nil {
		return err
	}
	candidate := SessionEntry{
		ID: rowID, Type: EntryTypeCheckpoint, SessionID: s.sessionID, BranchID: s.branchID,
		RunID: action.RunID, WorkspaceID: action.WorkspaceID,
		ActionBatchID: action.ActionBatchID, ActionID: action.ActionID, OwnerGeneration: action.OwnerGeneration,
		AssistantTranscriptOrdinal: uint64Pointer(action.AssistantTranscriptOrdinal), ToolCallID: action.ToolCallID,
		ToolName: intent.ToolName, CheckpointKind: CheckpointProcessSessionAction, ProcessSessionAction: &record,
	}
	for _, existing := range s.branchRows {
		if existing.ID != rowID {
			continue
		}
		if !sameProcessSessionActionRow(existing, candidate) {
			return errors.New("session: process-session action checkpoint conflicts with its durable row")
		}
		return nil
	}
	canonical, err := s.appendAndRefreshRowsLocked(ctx, candidate)
	if err != nil {
		return err
	}
	if len(canonical) != 1 || canonical[0].ID != rowID || canonical[0].Seq == 0 {
		return errors.New("session: durable writer returned an invalid process-session action checkpoint")
	}
	return nil
}

// ResolveProcessSessionLease records positive group/leader/reader closure and
// resolves only the exact original start marker. A terminal-delivered lease
// may be closed after the run using the invocation host's fresh sink.
func (s *SessionRunCommitSink) ResolveProcessSessionLease(
	ctx context.Context,
	snapshot tools.ProcessSessionLeaseSnapshot,
	groupAbsent, leaderWaited, readersJoined bool,
) error {
	if s == nil {
		return errors.New("session: process-session resolution requires a run-commit sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, snapshot.RunID); err != nil {
		return err
	}
	return s.resolveProcessSessionLeaseLocked(ctx, snapshot, groupAbsent, leaderWaited, readersJoined, s.terminalID != "")
}

// CloseProcessSessionLease is the reconciliation-owner path used after an
// invocation's terminal delivery. The caller must hold the prepared session's
// exclusive reconciliation lease; this method still validates the exact
// terminal binding, start receipt, and manager-provided positive join facts.
func (s *SessionRunCommitSink) CloseProcessSessionLease(ctx context.Context, snapshot tools.ProcessSessionLeaseSnapshot) error {
	if s == nil {
		return errors.New("session: process-session closure requires a run-commit sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session: run-commit lease is closed")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := validateProcessSessionSnapshot(snapshot); err != nil {
		return err
	}
	if snapshot.Phase != tools.ProcessSessionLeaseStopped || !snapshot.GroupAbsent || !snapshot.LeaderWaited || !snapshot.ReadersJoined {
		return errors.New("session: process-session closure lacks positive group, leader, and reader join facts")
	}
	if err := s.validateProcessSessionOriginLocked(snapshot); err != nil {
		return err
	}
	if err := s.validateProcessSessionTerminalBindingLocked(snapshot); err != nil {
		return err
	}
	return s.resolveProcessSessionLeaseLocked(ctx, snapshot, true, true, true, true)
}

// ActiveProcessSessionLeases returns current durable active facts for a run.
// A successful start intent, delivered-live execution, receipt, and start
// action row must all be present before a lease appears here.
func (s *SessionRunCommitSink) ActiveProcessSessionLeases(ctx context.Context, runID string) ([]tools.ProcessSessionLeaseSnapshot, error) {
	if s == nil {
		return nil, errors.New("session: process-session lookup requires a run-commit sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateCallContextLocked(ctx, runID); err != nil {
		return nil, err
	}
	latest := make(map[string]SessionEntry)
	for _, row := range s.branchRows {
		lease := row.ProcessSessionLease
		if row.RecordVersion != 1 || row.Type != EntryTypeCheckpoint || row.CheckpointKind != CheckpointProcessSessionLease || lease == nil || lease.RunID != runID || row.Seq == 0 {
			continue
		}
		previous, exists := latest[lease.OwnerID+"\x00"+lease.SessionID]
		if !exists || previous.Seq < row.Seq {
			latest[lease.OwnerID+"\x00"+lease.SessionID] = row
		}
	}
	leases := make([]tools.ProcessSessionLeaseSnapshot, 0, len(latest))
	for _, row := range latest {
		lease := row.ProcessSessionLease
		if lease.Phase != tools.ProcessSessionLeaseActive && lease.Phase != tools.ProcessSessionLeaseDelivered {
			continue
		}
		if err := s.validateActiveProcessSessionLeaseLocked(row); err != nil {
			return nil, err
		}
		leases = append(leases, tools.CloneProcessSessionLeaseSnapshot(*lease))
	}
	sort.Slice(leases, func(left, right int) bool { return leases[left].SessionID < leases[right].SessionID })
	return leases, nil
}

func (s *SessionRunCommitSink) recordProcessSessionLeaseLocked(ctx context.Context, snapshot tools.ProcessSessionLeaseSnapshot, allowAfterTerminal bool) (SessionEntry, error) {
	if !allowAfterTerminal {
		if err := s.rejectAfterTerminalLocked(); err != nil {
			return SessionEntry{}, err
		}
	} else if err := s.validateProcessSessionTerminalBindingLocked(snapshot); err != nil {
		return SessionEntry{}, err
	}
	if err := validateProcessSessionSnapshot(snapshot); err != nil {
		return SessionEntry{}, err
	}
	if err := s.validateProcessSessionOriginLocked(snapshot); err != nil {
		return SessionEntry{}, err
	}
	previous, previousRow, exists, err := s.latestProcessSessionLeaseLocked(snapshot.OwnerID, snapshot.RunID, snapshot.SessionID)
	if err != nil {
		return SessionEntry{}, err
	}
	if exists {
		if !sameProcessSessionLeaseIdentity(previous, snapshot) || !validProcessSessionLeaseTransition(previous.Phase, snapshot.Phase) {
			if !reflect.DeepEqual(previous, snapshot) {
				return SessionEntry{}, errors.New("session: process-session lease changes identity or regresses lifecycle")
			}
		}
	} else if snapshot.Phase != tools.ProcessSessionLeaseGated {
		return SessionEntry{}, errors.New("session: process-session lease must begin with a gated identity")
	}
	rowID, err := stableRunCommitEntryID("process-session-lease-v1", struct {
		OwnerID, RunID, SessionID string
		Phase                     tools.ProcessSessionLeasePhase
	}{snapshot.OwnerID, snapshot.RunID, snapshot.SessionID, snapshot.Phase})
	if err != nil {
		return SessionEntry{}, err
	}
	leaseCopy := tools.CloneProcessSessionLeaseSnapshot(snapshot)
	row := SessionEntry{
		ID: rowID, Type: EntryTypeCheckpoint, SessionID: s.sessionID, BranchID: s.branchID,
		RunID: snapshot.RunID, WorkspaceID: snapshot.WorkspaceID,
		ActionBatchID: snapshot.Action.ActionBatchID, ActionID: snapshot.Action.ActionID,
		OwnerGeneration:            snapshot.OwnerGeneration,
		AssistantTranscriptOrdinal: uint64Pointer(snapshot.Action.AssistantTranscriptOrdinal),
		ToolCallID:                 snapshot.Action.ToolCallID, ToolName: tools.ProcessSessionToolName,
		CheckpointKind: CheckpointProcessSessionLease, ProcessSessionLease: &leaseCopy,
	}
	var canonical SessionEntry
	found := false
	for _, existing := range s.branchRows {
		if existing.ID != rowID {
			continue
		}
		if !sameProcessSessionLeaseRow(existing, row) {
			return SessionEntry{}, errors.New("session: process-session lease checkpoint conflicts with its durable row")
		}
		canonical, found = existing, true
		break
	}
	if !found {
		rows, appendErr := s.appendAndRefreshRowsLocked(ctx, row)
		if appendErr != nil {
			return SessionEntry{}, appendErr
		}
		if len(rows) != 1 || rows[0].ID != rowID || rows[0].Seq == 0 {
			return SessionEntry{}, errors.New("session: durable writer returned an invalid process-session lease checkpoint")
		}
		canonical = rows[0]
	}
	if err := validateProcessSessionLeaseRow(canonical); err != nil {
		return SessionEntry{}, err
	}
	if err := s.index.ApplyProcessSessionLease(ctx, canonical); err != nil {
		return SessionEntry{}, fmt.Errorf("session: apply synced process-session lease to workspace index: %w", err)
	}
	if exists && previousRow.ID == canonical.ID && !sameProcessSessionLeaseRow(previousRow, canonical) {
		return SessionEntry{}, errors.New("session: current process-session lease row changed during retry")
	}
	return canonical, nil
}

func (s *SessionRunCommitSink) resolveProcessSessionLeaseLocked(
	ctx context.Context,
	snapshot tools.ProcessSessionLeaseSnapshot,
	groupAbsent, leaderWaited, readersJoined bool,
	allowAfterTerminal bool,
) error {
	if !groupAbsent || !leaderWaited || !readersJoined || snapshot.Phase != tools.ProcessSessionLeaseStopped || !snapshot.GroupAbsent || !snapshot.LeaderWaited || !snapshot.ReadersJoined {
		return errors.New("session: process-session resolution requires positive group, leader, and reader closure facts")
	}
	if err := validateProcessSessionSnapshot(snapshot); err != nil {
		return err
	}
	if err := s.validateProcessSessionOriginLocked(snapshot); err != nil {
		return err
	}
	if allowAfterTerminal {
		if err := s.validateProcessSessionTerminalBindingLocked(snapshot); err != nil {
			return err
		}
	}
	var row SessionEntry
	previous, previousRow, exists, err := s.latestProcessSessionLeaseLocked(snapshot.OwnerID, snapshot.RunID, snapshot.SessionID)
	if err != nil {
		return err
	}
	if exists && previous.Phase == tools.ProcessSessionLeaseStopped && sameProcessSessionLeaseIdentity(previous, snapshot) {
		// A Stopped row may be synced before its start receipt arrives. Resolution
		// reuses that exact closure checkpoint instead of rewriting the same
		// phase with a later output cursor.
		row = previousRow
	} else {
		row, err = s.recordProcessSessionLeaseLocked(ctx, snapshot, allowAfterTerminal)
		if err != nil {
			return err
		}
	}
	intent, err := s.findActionIntentLocked(snapshot.RunID, snapshot.Action.AssistantTranscriptOrdinal, snapshot.Action.ToolCallID, snapshot.Action.ActionID, snapshot.OwnerGeneration)
	if err != nil {
		return err
	}
	ref := refFromEntry(intent, s.sessionID)
	receipt, err := s.findReceiptForIntentLocked(ref, intent)
	if err != nil {
		return err
	}
	resolution := core.ExecutionResolution{
		RunID: snapshot.RunID, ActionID: snapshot.Action.ActionID,
		Evidence: core.ResolutionLocalResourcesStopped, EvidenceRef: row.ID,
		ProcessSessionClosureRef: row.ID,
	}
	return s.resolveExecutionLocked(ctx, resolution, ref, intent, receipt, false)
}

func (s *SessionRunCommitSink) validateProcessSessionOriginLocked(snapshot tools.ProcessSessionLeaseSnapshot) error {
	if snapshot.OwnerID == "" || snapshot.WorkspaceID != s.workspaceID || snapshot.Action.SessionID != s.sessionID ||
		snapshot.RunID != snapshot.Action.RunID || snapshot.WorkspaceID != snapshot.Action.WorkspaceID ||
		snapshot.OwnerGeneration != snapshot.Action.OwnerGeneration || snapshot.RunID == "" || snapshot.OwnerGeneration == "" {
		return errors.New("session: process-session lease has a mismatched owner, run, workspace, or session identity")
	}
	intent, occurrence, err := s.processSessionIntentForActionLocked(snapshot.Action)
	if err != nil {
		return err
	}
	operation, ok := processSessionOccurrenceOperation(occurrence)
	if !ok || operation != tools.ProcessSessionOperationStart || intent.ToolName != tools.ProcessSessionToolName {
		return errors.New("session: process-session lease origin is not a builtin start action")
	}
	return nil
}

func (s *SessionRunCommitSink) processSessionIntentForActionLocked(action tools.ProcessSessionActionRef) (SessionEntry, *durableActionOccurrence, error) {
	if action.SessionID != s.sessionID || action.WorkspaceID != s.workspaceID || action.RunID == "" || action.ActionID == "" || action.ActionBatchID == "" || action.ToolCallID == "" || action.OwnerGeneration == "" || action.SchemaDigest == ([32]byte{}) || action.ArgumentDigest == ([32]byte{}) {
		return SessionEntry{}, nil, errors.New("session: process-session action has an incomplete immutable reference")
	}
	intent, err := s.findActionIntentLocked(action.RunID, action.AssistantTranscriptOrdinal, action.ToolCallID, action.ActionID, action.OwnerGeneration)
	if err != nil {
		return SessionEntry{}, nil, err
	}
	if intent.ToolName != tools.ProcessSessionToolName || intent.ToolIdentity == nil || intent.ToolIdentity.RegistrationSource != tools.RegistrationSourceBuiltin ||
		intent.ArgumentDigest != digestHex(action.ArgumentDigest) || intent.SchemaDigest != digestHex(action.SchemaDigest) {
		return SessionEntry{}, nil, errors.New("session: process-session action does not match its trusted builtin intent")
	}
	plan, err := analyzeDurableActionJournal(s.branchRows, s.sessionID)
	if err != nil {
		return SessionEntry{}, nil, err
	}
	for _, occurrence := range plan.occurrences {
		if occurrence.intent != nil && occurrence.intent.ID == intent.ID {
			return intent, occurrence, nil
		}
	}
	return SessionEntry{}, nil, errors.New("session: process-session action occurrence is not in the validated journal")
}

func (s *SessionRunCommitSink) latestProcessSessionLeaseLocked(ownerID, runID, sessionID string) (tools.ProcessSessionLeaseSnapshot, SessionEntry, bool, error) {
	var latest SessionEntry
	found := false
	for _, row := range s.branchRows {
		lease := row.ProcessSessionLease
		if row.RecordVersion != 1 || row.Type != EntryTypeCheckpoint || row.CheckpointKind != CheckpointProcessSessionLease || lease == nil ||
			lease.OwnerID != ownerID || lease.RunID != runID || lease.SessionID != sessionID {
			continue
		}
		if found && row.Seq <= latest.Seq {
			return tools.ProcessSessionLeaseSnapshot{}, SessionEntry{}, false, errors.New("session: process-session lease rows are not strictly ordered")
		}
		latest, found = row, true
	}
	if !found {
		return tools.ProcessSessionLeaseSnapshot{}, SessionEntry{}, false, nil
	}
	if err := validateProcessSessionLeaseRow(latest); err != nil {
		return tools.ProcessSessionLeaseSnapshot{}, SessionEntry{}, false, err
	}
	return tools.CloneProcessSessionLeaseSnapshot(*latest.ProcessSessionLease), latest, true, nil
}

func (s *SessionRunCommitSink) validateActiveProcessSessionLeaseLocked(row SessionEntry) error {
	lease := row.ProcessSessionLease
	if lease == nil || (lease.Phase != tools.ProcessSessionLeaseActive && lease.Phase != tools.ProcessSessionLeaseDelivered) {
		return errors.New("session: active process-session lookup found a non-live lease")
	}
	if err := validateProcessSessionLeaseRow(row); err != nil {
		return err
	}
	if err := s.validateProcessSessionOriginLocked(*lease); err != nil {
		return err
	}
	intent, occurrence, err := s.processSessionIntentForActionLocked(lease.Action)
	if err != nil {
		return err
	}
	if operation, ok := processSessionOccurrenceOperation(occurrence); !ok || operation != tools.ProcessSessionOperationStart {
		return errors.New("session: active process-session lease does not name its start action")
	}
	ref := refFromEntry(intent, s.sessionID)
	receipt, err := s.findReceiptForIntentLocked(ref, intent)
	if err != nil || receipt.DispatchState != core.DispatchStateDispatched || receipt.ReceiptOutcome != core.ReceiptOutcomeSuccess || receipt.Seq == 0 {
		return errors.New("session: active process-session lease lacks a synced successful start receipt")
	}
	executionFound := false
	for _, candidate := range s.branchRows {
		if candidate.RecordVersion == 1 && candidate.Type == EntryTypeTool && candidate.ActionState == ActionStateExecution &&
			candidate.ResourceState == core.RunExecutionDeliveredLive && candidate.DispatchState == core.DispatchStateDispatched &&
			candidate.ProcessSessionLease != nil && sameActionReference(ref, refFromEntry(candidate, s.sessionID)) &&
			candidate.Seq < row.Seq && sameProcessSessionLeaseIdentity(*candidate.ProcessSessionLease, *lease) {
			if executionFound {
				return errors.New("session: active process-session lease has multiple delivered-live execution rows")
			}
			executionFound = true
		}
	}
	if !executionFound {
		return errors.New("session: active process-session lease lacks its exact delivered-live execution row")
	}
	startActionFound := false
	for _, candidate := range s.branchRows {
		record := candidate.ProcessSessionAction
		if candidate.RecordVersion == 1 && candidate.Type == EntryTypeCheckpoint && candidate.CheckpointKind == CheckpointProcessSessionAction && record != nil &&
			candidate.Seq > receipt.Seq && record.LeaseSessionID == lease.SessionID && record.LeaseAction == lease.Action && record.Action == lease.Action && record.Operation == tools.ProcessSessionOperationStart {
			startActionFound = true
		}
	}
	if !startActionFound {
		return errors.New("session: active process-session lease lacks its synced start action")
	}
	return nil
}

func (s *SessionRunCommitSink) validateProcessSessionTerminalBindingLocked(snapshot tools.ProcessSessionLeaseSnapshot) error {
	terminal, exists, err := s.findTerminalCheckpointLocked(snapshot.RunID)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	binding := terminal.ProcessSessionDelivery
	if binding == nil || binding.OwnerID != snapshot.OwnerID || binding.RunID != snapshot.RunID || binding.WorkspaceID != snapshot.WorkspaceID {
		return errors.New("session: post-terminal process-session closure has no matching delivered terminal binding")
	}
	for _, sessionID := range binding.SessionIDs {
		if sessionID == snapshot.SessionID {
			return nil
		}
	}
	return errors.New("session: post-terminal process-session closure is absent from the terminal delivery binding")
}

func (s *SessionRunCommitSink) prepareProcessSessionTerminalDeliveryLocked(ctx context.Context, runID string, binding *tools.ProcessSessionDeliveryBinding) error {
	latest := make(map[string]SessionEntry)
	for _, row := range s.branchRows {
		lease := row.ProcessSessionLease
		if row.RecordVersion != 1 || row.Type != EntryTypeCheckpoint || row.CheckpointKind != CheckpointProcessSessionLease || lease == nil || lease.RunID != runID {
			continue
		}
		key := lease.OwnerID + "\x00" + lease.SessionID
		if previous, exists := latest[key]; !exists || previous.Seq < row.Seq {
			latest[key] = row
		}
	}
	active := make([]SessionEntry, 0, len(latest))
	for _, row := range latest {
		lease := row.ProcessSessionLease
		if lease.Phase == tools.ProcessSessionLeaseActive || lease.Phase == tools.ProcessSessionLeaseDelivered {
			if err := s.validateActiveProcessSessionLeaseLocked(row); err != nil {
				return err
			}
			active = append(active, row)
		}
	}
	sort.Slice(active, func(left, right int) bool {
		return active[left].ProcessSessionLease.SessionID < active[right].ProcessSessionLease.SessionID
	})
	if len(active) == 0 {
		if binding != nil {
			return errors.New("session: terminal process-session delivery has no active durable lease")
		}
		return nil
	}
	if binding == nil {
		return errors.New("session: terminal run has active process-session leases without an invocation delivery binding")
	}
	if binding.RunID != runID || binding.WorkspaceID != s.workspaceID || binding.OwnerID == "" || len(binding.SessionIDs) != len(active) || !sort.StringsAreSorted(binding.SessionIDs) {
		return errors.New("session: terminal process-session binding has incomplete or unsorted identity")
	}
	for index, row := range active {
		lease := row.ProcessSessionLease
		if binding.SessionIDs[index] != lease.SessionID || lease.OwnerID != binding.OwnerID || lease.WorkspaceID != binding.WorkspaceID || lease.RunID != binding.RunID {
			return errors.New("session: terminal process-session binding differs from the exact active lease set")
		}
		if lease.Phase == tools.ProcessSessionLeaseDelivered {
			continue
		}
		delivered := tools.CloneProcessSessionLeaseSnapshot(*lease)
		delivered.Phase = tools.ProcessSessionLeaseDelivered
		if _, err := s.recordProcessSessionLeaseLocked(ctx, delivered, false); err != nil {
			return fmt.Errorf("session: persist delivered process-session lease: %w", err)
		}
	}
	return nil
}

func validateProcessSessionSnapshot(snapshot tools.ProcessSessionLeaseSnapshot) error {
	if snapshot.SessionID == "" || snapshot.OwnerID == "" || snapshot.WorkspaceID == "" || snapshot.RunID == "" || snapshot.OwnerGeneration == "" || snapshot.Identity == nil {
		return errors.New("session: process-session lease snapshot has incomplete owner or process identity")
	}
	identity := snapshot.Identity
	if identity.LeaderPID <= 0 || identity.ProcessGroupID != identity.LeaderPID || identity.HostGeneration == "" || identity.ProcessStartToken == "" {
		return errors.New("session: process-session lease snapshot has an invalid process identity")
	}
	switch snapshot.Status {
	case tools.ProcessSessionStarting, tools.ProcessSessionRunning, tools.ProcessSessionStopping, tools.ProcessSessionExited, tools.ProcessSessionStopped:
	default:
		return fmt.Errorf("session: process-session lease snapshot has invalid status %q", snapshot.Status)
	}
	switch snapshot.Phase {
	case tools.ProcessSessionLeaseGated, tools.ProcessSessionLeaseActive, tools.ProcessSessionLeaseDelivered, tools.ProcessSessionLeaseStopping, tools.ProcessSessionLeaseStopped, tools.ProcessSessionLeaseUnknown:
	default:
		return fmt.Errorf("session: process-session lease snapshot has invalid phase %q", snapshot.Phase)
	}
	if snapshot.Phase == tools.ProcessSessionLeaseStopped && (!snapshot.GroupAbsent || !snapshot.LeaderWaited || !snapshot.ReadersJoined) {
		return errors.New("session: stopped process-session lease lacks positive process-group and reader closure")
	}
	return nil
}

func sameProcessSessionLeaseRow(existing, candidate SessionEntry) bool {
	return existing.ID == candidate.ID && existing.Type == EntryTypeCheckpoint && existing.CheckpointKind == CheckpointProcessSessionLease &&
		existing.RunID == candidate.RunID && existing.WorkspaceID == candidate.WorkspaceID && existing.SessionID == candidate.SessionID &&
		existing.ActionBatchID == candidate.ActionBatchID && existing.ActionID == candidate.ActionID &&
		existing.OwnerGeneration == candidate.OwnerGeneration && existing.ToolCallID == candidate.ToolCallID &&
		reflect.DeepEqual(existing.ProcessSessionLease, candidate.ProcessSessionLease)
}

func sameProcessSessionActionRow(existing, candidate SessionEntry) bool {
	return existing.ID == candidate.ID && existing.Type == EntryTypeCheckpoint && existing.CheckpointKind == CheckpointProcessSessionAction &&
		existing.RunID == candidate.RunID && existing.WorkspaceID == candidate.WorkspaceID && existing.SessionID == candidate.SessionID &&
		existing.ActionBatchID == candidate.ActionBatchID && existing.ActionID == candidate.ActionID &&
		existing.OwnerGeneration == candidate.OwnerGeneration && existing.ToolCallID == candidate.ToolCallID &&
		reflect.DeepEqual(existing.ProcessSessionAction, candidate.ProcessSessionAction)
}

func digestHex(digest [32]byte) string {
	const hex = "0123456789abcdef"
	encoded := make([]byte, len(digest)*2)
	for index, value := range digest {
		encoded[index*2] = hex[value>>4]
		encoded[index*2+1] = hex[value&0x0f]
	}
	return string(encoded)
}

func uint64Pointer(value uint64) *uint64 { return &value }

func cloneProcessSessionDeliveryBinding(binding *tools.ProcessSessionDeliveryBinding) *tools.ProcessSessionDeliveryBinding {
	if binding == nil {
		return nil
	}
	copy := *binding
	copy.SessionIDs = append([]string(nil), binding.SessionIDs...)
	return &copy
}
