package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// ApplyProcessSessionLease mirrors only a session-row lease fact that the
// owning sink has already validated and synced. The live manager capability
// is never persisted in the workspace index.
func (i *WorkspaceQuarantineIndex) ApplyProcessSessionLease(ctx context.Context, row SessionEntry) error {
	if err := validateProcessSessionLeaseRow(row); err != nil {
		return err
	}
	lease := tools.CloneProcessSessionLeaseSnapshot(*row.ProcessSessionLease)
	return i.withLockedIndex(ctx, row.WorkspaceID, func(handle *workspaceIndexHandle) error {
		if err := contextErr(ctx); err != nil {
			return err
		}
		if err := handle.index.syncFile(handle.file); err != nil {
			return fmt.Errorf("session: confirm workspace index durability before process-session lease: %w", err)
		}
		candidate := workspaceIndexRecord{
			Version: 1, RecordID: row.ID, Kind: workspaceIndexProcessSession,
			WorkspaceID: row.WorkspaceID, SessionID: row.SessionID, RunID: row.RunID,
			ActionBatchID: row.ActionBatchID, CheckpointID: row.ID, CheckpointSeq: row.Seq,
			ProcessSessionLease: &lease,
		}
		for _, existing := range handle.state.records {
			if existing.RecordID != row.ID {
				continue
			}
			existingForComparison := existing
			candidateForComparison := candidate
			existingForComparison.Seq = 0
			candidateForComparison.Seq = 0
			if !reflect.DeepEqual(existingForComparison, candidateForComparison) {
				return errors.New("session: process-session workspace record ID conflicts with its durable row")
			}
			return handle.index.syncFile(handle.file)
		}
		return handle.appendRecords(candidate)
	})
}

func validateProcessSessionLeaseRow(row SessionEntry) error {
	lease := row.ProcessSessionLease
	if row.Type != EntryTypeCheckpoint || row.CheckpointKind != CheckpointProcessSessionLease || row.ID == "" || row.Seq == 0 || lease == nil {
		return errors.New("session: process-session lease row is incomplete")
	}
	if row.RunID != lease.RunID || row.WorkspaceID != lease.WorkspaceID || row.SessionID != lease.Action.SessionID ||
		row.ActionBatchID != lease.Action.ActionBatchID || row.ActionID != lease.Action.ActionID ||
		row.OwnerGeneration != lease.Action.OwnerGeneration || row.ToolCallID != lease.Action.ToolCallID {
		return errors.New("session: process-session lease row does not match its action identity")
	}
	for _, field := range []struct{ name, value string }{
		{"session ID", lease.SessionID}, {"owner ID", lease.OwnerID}, {"workspace ID", lease.WorkspaceID},
		{"run ID", lease.RunID}, {"owner generation", lease.OwnerGeneration},
		{"start action session ID", lease.Action.SessionID}, {"action run ID", lease.Action.RunID},
		{"action workspace ID", lease.Action.WorkspaceID}, {"action batch ID", lease.Action.ActionBatchID},
		{"action ID", lease.Action.ActionID}, {"action owner generation", lease.Action.OwnerGeneration},
		{"tool call ID", lease.Action.ToolCallID},
	} {
		if field.value == "" {
			return fmt.Errorf("session: process-session lease snapshot is missing %s", field.name)
		}
	}
	if lease.Action.SessionID != row.SessionID || lease.Action.RunID != lease.RunID ||
		lease.Action.WorkspaceID != lease.WorkspaceID || lease.Action.OwnerGeneration != lease.OwnerGeneration {
		return errors.New("session: process-session lease snapshot action scope does not match its owner")
	}
	if lease.Action.SchemaDigest == ([32]byte{}) {
		return errors.New("session: process-session lease snapshot is missing the action schema digest")
	}
	if lease.Action.ArgumentDigest == ([32]byte{}) {
		return errors.New("session: process-session lease snapshot is missing the action argument digest")
	}
	if lease.Identity == nil || lease.Identity.LeaderPID <= 0 || lease.Identity.ProcessGroupID != lease.Identity.LeaderPID ||
		lease.Identity.HostGeneration == "" || lease.Identity.ProcessStartToken == "" {
		return errors.New("session: process-session lease has no captured local process identity")
	}
	if lease.Phase == tools.ProcessSessionLeaseStopped && (!lease.GroupAbsent || !lease.LeaderWaited || !lease.ReadersJoined) {
		return errors.New("session: stopped process-session lease lacks complete group and reader closure proof")
	}
	switch lease.Phase {
	case tools.ProcessSessionLeaseGated, tools.ProcessSessionLeaseActive, tools.ProcessSessionLeaseDelivered, tools.ProcessSessionLeaseStopping, tools.ProcessSessionLeaseStopped, tools.ProcessSessionLeaseUnknown:
		return nil
	default:
		return fmt.Errorf("session: process-session lease has invalid phase %q", lease.Phase)
	}
}

func (s *workspaceIndexState) addProcessSessionLeaseRecord(record *workspaceIndexRecord) error {
	if err := validateWorkspaceRecordIdentityParts(record.WorkspaceID, record.SessionID, record.RunID, record.ActionBatchID, record.RecordID, record.CheckpointID); err != nil {
		return err
	}
	lease := record.ProcessSessionLease
	if lease == nil || record.SessionID != lease.Action.SessionID || record.RunID != lease.RunID || record.WorkspaceID != lease.WorkspaceID ||
		record.ActionBatchID != lease.Action.ActionBatchID || record.CheckpointID != record.RecordID || record.CheckpointSeq == 0 {
		return errors.New("incomplete or invalid process-session lease index envelope")
	}
	if lease.Phase == tools.ProcessSessionLeaseStopped && (!lease.GroupAbsent || !lease.LeaderWaited || !lease.ReadersJoined) {
		return errors.New("process-session closure lacks complete local join observations")
	}
	key := lease.OwnerID + "\x00" + lease.SessionID
	if previous, exists := s.processSessions[key]; exists {
		if !sameProcessSessionLeaseIdentity(previous, *lease) || !validProcessSessionLeaseTransition(previous.Phase, lease.Phase) {
			return errors.New("process-session lease transition changes identity or regresses lifecycle")
		}
	} else if lease.Phase != tools.ProcessSessionLeaseGated && lease.Phase != tools.ProcessSessionLeaseUnknown {
		return errors.New("process-session lease skips its durable gated identity")
	}
	if existing := s.processSessions[key]; existing.Phase == tools.ProcessSessionLeaseStopped {
		return errors.New("stopped process-session lease cannot transition again")
	}
	copy := tools.CloneProcessSessionLeaseSnapshot(*lease)
	s.processSessions[key] = copy
	return nil
}

func sameProcessSessionLeaseIdentity(left, right tools.ProcessSessionLeaseSnapshot) bool {
	return left.SessionID == right.SessionID && left.OwnerID == right.OwnerID && left.WorkspaceID == right.WorkspaceID &&
		left.RunID == right.RunID && left.OwnerGeneration == right.OwnerGeneration && reflect.DeepEqual(left.Action, right.Action) &&
		reflect.DeepEqual(left.Identity, right.Identity)
}

func validProcessSessionLeaseTransition(previous, next tools.ProcessSessionLeasePhase) bool {
	switch previous {
	case tools.ProcessSessionLeaseGated:
		return next == tools.ProcessSessionLeaseActive || next == tools.ProcessSessionLeaseStopping || next == tools.ProcessSessionLeaseStopped || next == tools.ProcessSessionLeaseUnknown
	case tools.ProcessSessionLeaseActive:
		return next == tools.ProcessSessionLeaseDelivered || next == tools.ProcessSessionLeaseStopping || next == tools.ProcessSessionLeaseStopped || next == tools.ProcessSessionLeaseUnknown
	case tools.ProcessSessionLeaseDelivered:
		return next == tools.ProcessSessionLeaseStopping || next == tools.ProcessSessionLeaseStopped || next == tools.ProcessSessionLeaseUnknown
	case tools.ProcessSessionLeaseStopping:
		return next == tools.ProcessSessionLeaseStopped || next == tools.ProcessSessionLeaseUnknown
	case tools.ProcessSessionLeaseUnknown:
		return next == tools.ProcessSessionLeaseStopping || next == tools.ProcessSessionLeaseStopped
	default:
		return false
	}
}

func (s *workspaceIndexState) allowsProcessSessionAdmission(batch WorkspaceDispatchBatch) bool {
	capability := batch.ProcessSessionAdmission
	if capability == nil || len(batch.Actions) == 0 {
		return false
	}
	for _, action := range batch.Actions {
		if action.Action.RunID != batch.RunID || action.Action.WorkspaceID != batch.WorkspaceID || action.OwnerGeneration == "" ||
			!capability.Allows(action.Action.RunID, action.Action.WorkspaceID, action.OwnerGeneration) {
			return false
		}
	}
	allowedLeaseActions := make(map[workspaceActionKey]struct{})
	for _, lease := range s.processSessions {
		if (lease.Phase != tools.ProcessSessionLeaseActive && lease.Phase != tools.ProcessSessionLeaseDelivered) || !capability.AllowsLeaseForAdmission(lease) {
			continue
		}
		ref := PendingActionRef{
			SessionID: lease.Action.SessionID, WorkspaceID: lease.Action.WorkspaceID, RunID: lease.Action.RunID,
			ActionBatchID: lease.Action.ActionBatchID, AssistantTranscriptOrdinal: lease.Action.AssistantTranscriptOrdinal,
			ToolCallID: lease.Action.ToolCallID, ActionID: lease.Action.ActionID,
		}
		key := workspaceActionFor(ref)
		indexed := s.actionsByKey[key]
		if indexed == nil || indexed.ref != ref || indexed.resolution != nil ||
			indexed.action.OwnerGeneration != lease.OwnerGeneration || indexed.action.ToolIdentity == nil ||
			indexed.action.ToolIdentity.RegistrationSource != tools.RegistrationSourceBuiltin ||
			indexed.action.ToolIdentity.ToolName != tools.ProcessSessionToolName {
			continue
		}
		allowedLeaseActions[key] = struct{}{}
	}
	if len(allowedLeaseActions) == 0 {
		return false
	}
	for _, action := range s.actionsByKey {
		if action.resolution != nil || action.action.NonBlockingDelegation {
			continue
		}
		if _, allowed := allowedLeaseActions[workspaceActionFor(action.ref)]; !allowed {
			return false
		}
	}
	return true
}
