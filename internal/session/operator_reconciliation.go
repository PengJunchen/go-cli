package session

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pengjunchen/go-cli/internal/core"
)

// OperatorReconciliationRequest identifies one exact pending action and records
// a bounded rationale for accepting its still-unknown outcome.
type OperatorReconciliationRequest struct {
	Action            PendingActionRef
	ExpectedRevision  string
	Reason            string
	Reference         string
	AcceptUnknownRisk bool
}

// SyncRecordedResolutions reapplies only resolutions already validated from
// durable session checkpoints. It is safe to retry after a partial local/index
// write because the workspace index rejects conflicting identities and treats
// an exact resolution record idempotently.
func (s *SessionRunCommitSink) SyncRecordedResolutions(ctx context.Context) error {
	if s == nil {
		return errors.New("session: resolution sync requires a run-commit sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session: run-commit lease is closed")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	plan, err := analyzeDurableActionJournal(s.allRows, s.sessionID)
	if err != nil {
		return err
	}
	if len(plan.resolutions) == 0 {
		return nil
	}
	if err := s.index.ApplyDurableResolutions(ctx, plan.resolutions); err != nil {
		return fmt.Errorf("session: sync recorded execution resolutions: %w", err)
	}
	return nil
}

// ReconcileUnknownAction durably records explicit acceptance of one unknown
// dispatched action, writes its typed execution-resolution checkpoint, and
// applies that exact resolution to the workspace quarantine index. It never
// changes or replays the original tool receipt.
func (s *SessionRunCommitSink) ReconcileUnknownAction(ctx context.Context, request OperatorReconciliationRequest) error {
	if s == nil {
		return errors.New("session: operator reconciliation requires a run-commit sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("session: run-commit lease is closed")
	}
	if err := contextErr(ctx); err != nil {
		return err
	}
	if err := request.Action.Validate(); err != nil {
		return err
	}
	if request.ExpectedRevision == "" || request.ExpectedRevision != s.baseView.Revision {
		return errors.New("session: operator reconciliation has a stale or missing session revision")
	}
	record := &OperatorReconciliationRecord{
		Reason: strings.TrimSpace(request.Reason), Reference: strings.TrimSpace(request.Reference),
		AcceptedUnknownRisk: request.AcceptUnknownRisk,
	}
	if err := record.validate(); err != nil {
		return err
	}
	if request.Action.SessionID != s.sessionID || request.Action.WorkspaceID != s.workspaceID {
		return errors.New("session: operator reconciliation action belongs to another session or workspace")
	}
	plan, err := analyzeDurableActionJournal(s.branchRows, s.sessionID)
	if err != nil {
		return err
	}
	var occurrence *durableActionOccurrence
	for _, candidate := range plan.occurrences {
		if sameActionReference(candidate.ref, request.Action) {
			occurrence = candidate
			break
		}
	}
	if occurrence == nil || occurrence.intent == nil || occurrence.receipt == nil {
		return errors.New("session: operator reconciliation action is not a reserved, receipted occurrence on the active branch")
	}
	if occurrence.receipt.DispatchState != DispatchStateDispatched || occurrence.receipt.ReceiptOutcome != core.ReceiptOutcomeUnknown {
		return errors.New("session: operator reconciliation requires the exact dispatched unknown receipt")
	}
	if occurrence.resolution != nil {
		if occurrence.resolution.Resolution.Evidence != core.ResolutionOperatorReconciled {
			return errors.New("session: action already has a different durable execution resolution")
		}
		checkpoint, checkpointErr := findOperatorReconciliationCheckpoint(s.branchRows, request.Action, occurrence.intent.OwnerGeneration)
		if checkpointErr != nil {
			return checkpointErr
		}
		if checkpoint == nil || checkpoint.OperatorReconciliation == nil || *checkpoint.OperatorReconciliation != *record || checkpoint.ID != occurrence.resolution.Resolution.EvidenceRef {
			return errors.New("session: retry conflicts with the durable operator reconciliation record")
		}
		if err := s.index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{*occurrence.resolution}); err != nil {
			return fmt.Errorf("session: retry workspace resolution sync: %w", err)
		}
		return nil
	}
	for _, execution := range occurrence.executions {
		if execution.ExecutionIdentity == nil {
			continue
		}
		inspect := s.inspectExecutionIdentity
		if inspect == nil {
			inspect = core.InspectProcessGroupExit
		}
		inspection := inspect(*execution.ExecutionIdentity)
		if inspection.Status == core.ProcessGroupExitPresent {
			return errors.New("session: operator reconciliation is blocked while the recorded owned process group is present")
		}
	}
	decisions, err := s.index.InspectDispatchMarkers(ctx, []PendingActionRef{request.Action})
	if err != nil {
		return fmt.Errorf("session: inspect action quarantine marker: %w", err)
	}
	if len(decisions) != 1 || decisions[0].Action != request.Action || decisions[0].MarkerState != DispatchMarkerPresent {
		return errors.New("session: operator reconciliation requires one present, unresolved workspace marker")
	}
	checkpointID, err := stableRunCommitEntryID("operator-reconciliation", struct {
		ActionRef       PendingActionRef
		OwnerGeneration string
	}{request.Action, occurrence.intent.OwnerGeneration})
	if err != nil {
		return err
	}
	checkpoint, err := findOperatorReconciliationCheckpoint(s.branchRows, request.Action, occurrence.intent.OwnerGeneration)
	if err != nil {
		return err
	}
	if checkpoint != nil {
		if checkpoint.ID != checkpointID || checkpoint.OperatorReconciliation == nil || *checkpoint.OperatorReconciliation != *record {
			return errors.New("session: existing operator reconciliation checkpoint conflicts with this request")
		}
	} else {
		row := SessionEntry{
			ID: checkpointID, Type: EntryTypeCheckpoint, RunID: request.Action.RunID,
			WorkspaceID: request.Action.WorkspaceID, ActionBatchID: request.Action.ActionBatchID,
			ActionID: request.Action.ActionID, OwnerGeneration: occurrence.intent.OwnerGeneration,
			AssistantTranscriptOrdinal: cloneUint64Pointer(occurrence.intent.AssistantTranscriptOrdinal),
			ToolCallID:                 request.Action.ToolCallID, ToolName: occurrence.toolName,
			CheckpointKind: CheckpointReconciliation, OperatorReconciliation: record,
		}
		canonical, appendErr := s.appendAndRefreshRowsLocked(ctx, row)
		if appendErr != nil {
			return appendErr
		}
		if len(canonical) != 1 || canonical[0].ID != checkpointID || canonical[0].Seq == 0 {
			return errors.New("session: durable writer returned an invalid operator reconciliation checkpoint")
		}
		checkpoint = &canonical[0]
	}
	resolution := core.ExecutionResolution{
		RunID: request.Action.RunID, ActionID: request.Action.ActionID,
		Evidence: core.ResolutionOperatorReconciled, EvidenceRef: checkpoint.ID,
	}
	return s.resolveExecutionLocked(ctx, resolution, request.Action, *occurrence.intent, *occurrence.receipt, true)
}

func findOperatorReconciliationCheckpoint(rows []SessionEntry, ref PendingActionRef, ownerGeneration string) (*SessionEntry, error) {
	var found *SessionEntry
	for index := range rows {
		row := &rows[index]
		if row.RecordVersion != 1 || row.Type != EntryTypeCheckpoint || row.CheckpointKind != CheckpointReconciliation ||
			row.OwnerGeneration != ownerGeneration || !sameActionReference(ref, refFromEntry(*row, ref.SessionID)) {
			continue
		}
		if found != nil {
			return nil, errors.New("session: action has ambiguous operator reconciliation checkpoints")
		}
		found = row
	}
	return found, nil
}
