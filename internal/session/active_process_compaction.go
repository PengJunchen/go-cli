package session

import (
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func validateActiveProcessSessionReferences(entries []SessionEntry, marker SessionEntry, markerIndex int, sessionID string) (map[string]struct{}, error) {
	allowedStartActions := make(map[string]struct{}, len(marker.ActiveProcessSessions))
	if markerIndex < 0 || markerIndex > len(entries) {
		return nil, errors.New("session: active process-session projection has an invalid source position")
	}
	if err := core.ValidateActiveProcessSessionRefs(marker.ActiveProcessSessions, marker.RunID, marker.WorkspaceID); err != nil {
		return nil, fmt.Errorf("invalid reference list: %w", err)
	}
	prefix := entries[:markerIndex]
	type leaseKey struct{ ownerID, sessionID string }
	type actionKey struct{ ownerGeneration, sessionID string }
	if len(marker.ActiveProcessSessions) == 0 {
		latest := make(map[leaseKey]SessionEntry)
		for _, row := range prefix {
			if row.Type != EntryTypeCheckpoint || row.CheckpointKind != CheckpointProcessSessionLease || row.ProcessSessionLease == nil || row.ProcessSessionLease.RunID != marker.RunID {
				continue
			}
			if row.RecordVersion != 1 || row.Seq == 0 {
				return nil, errors.New("session: process-session lease row is not a synced version-one checkpoint")
			}
			if err := validateProcessSessionLeaseRow(row); err != nil {
				return nil, fmt.Errorf("invalid process-session lease row %q: %w", row.ID, err)
			}
			lease := row.ProcessSessionLease
			key := leaseKey{lease.OwnerID, lease.SessionID}
			if previous, exists := latest[key]; !exists || row.Seq > previous.Seq {
				latest[key] = row
			}
		}
		for _, row := range latest {
			lease := row.ProcessSessionLease
			if lease.Phase != tools.ProcessSessionLeaseStopped {
				return nil, fmt.Errorf("session: unresolved process-session %q is missing from the compaction roster", lease.SessionID)
			}
		}
		for _, row := range prefix {
			if row.RecordVersion == 1 && row.Type == EntryTypeTool && row.ActionState == ActionStateExecution && row.ToolName == tools.ProcessSessionToolName && row.ResourceState == core.RunExecutionDeliveredLive && row.ProcessSessionLease == nil {
				return nil, errors.New("session: delivered-live process-session execution lacks its lease snapshot")
			}
		}
		if marker.RunID != "" && sessionID != "" {
			plan, err := analyzeDurableActionJournal(prefix, sessionID)
			if err != nil {
				return nil, fmt.Errorf("validate action journal: %w", err)
			}
			for _, occurrence := range plan.occurrences {
				if occurrence.intent == nil || occurrence.toolName != tools.ProcessSessionToolName {
					continue
				}
				for _, execution := range occurrence.executions {
					if execution.ResourceState != core.RunExecutionDeliveredLive {
						continue
					}
					if execution.ProcessSessionLease == nil {
						return nil, errors.New("session: delivered-live process-session execution lacks its lease snapshot")
					}
					lease := execution.ProcessSessionLease
					latestLease, exists := latest[leaseKey{lease.OwnerID, lease.SessionID}]
					if !exists {
						return nil, fmt.Errorf("session: delivered-live process-session %q has no latest lease checkpoint", lease.SessionID)
					}
					if latestLease.ProcessSessionLease.Phase == tools.ProcessSessionLeaseStopped && occurrence.resolution == nil {
						return nil, fmt.Errorf("session: stopped process-session %q has no durable start-action resolution", lease.SessionID)
					}
				}
			}
		}
		return allowedStartActions, nil
	}
	if marker.RunID == "" || marker.WorkspaceID == "" || sessionID == "" {
		return nil, errors.New("session: active process-session projection has incomplete source identity")
	}
	plan, err := analyzeDurableActionJournal(prefix, sessionID)
	if err != nil {
		return nil, fmt.Errorf("validate action journal: %w", err)
	}
	latestLeaseRows := make(map[leaseKey]SessionEntry)
	latestActionRows := make(map[actionKey]SessionEntry)
	for _, row := range prefix {
		if row.RecordVersion != 1 || row.Type != EntryTypeCheckpoint || row.Seq == 0 {
			continue
		}
		if row.CheckpointKind == CheckpointProcessSessionLease && row.ProcessSessionLease != nil && row.ProcessSessionLease.RunID == marker.RunID {
			lease := row.ProcessSessionLease
			key := leaseKey{lease.OwnerID, lease.SessionID}
			previous, exists := latestLeaseRows[key]
			if exists && previous.Seq >= row.Seq {
				return nil, errors.New("session: process-session lease rows are not in strictly increasing durable order")
			}
			if err := validateProcessSessionLeaseRow(row); err != nil {
				return nil, fmt.Errorf("invalid process-session lease row %q: %w", row.ID, err)
			}
			latestLeaseRows[key] = row
		}
		if row.CheckpointKind == CheckpointProcessSessionAction && row.ProcessSessionAction != nil && row.ProcessSessionAction.LeaseAction.RunID == marker.RunID {
			record := row.ProcessSessionAction
			key := actionKey{record.LeaseAction.OwnerGeneration, record.LeaseSessionID}
			previous, exists := latestActionRows[key]
			if exists && previous.Seq >= row.Seq {
				return nil, errors.New("session: process-session action rows are not in strictly increasing durable order")
			}
			latestActionRows[key] = row
		}
	}

	activeByKey := make(map[leaseKey]SessionEntry)
	for key, row := range latestLeaseRows {
		lease := row.ProcessSessionLease
		if lease.Phase != tools.ProcessSessionLeaseStopped {
			if lease.Phase != tools.ProcessSessionLeaseActive && lease.Phase != tools.ProcessSessionLeaseDelivered {
				return nil, fmt.Errorf("session: process-session %q has unresolved lease phase %q", lease.SessionID, lease.Phase)
			}
			activeByKey[key] = row
		}
	}
	if len(activeByKey) != len(marker.ActiveProcessSessions) {
		return nil, fmt.Errorf("session: active process-session roster has %d references for %d live leases", len(marker.ActiveProcessSessions), len(activeByKey))
	}

	for _, ref := range marker.ActiveProcessSessions {
		key := leaseKey{ref.OwnerID, ref.SessionID}
		leaseRow, exists := activeByKey[key]
		if !exists {
			return nil, fmt.Errorf("session: active process-session reference %q has no latest live lease", ref.SessionID)
		}
		lease := leaseRow.ProcessSessionLease
		if !activeProcessSessionRefMatchesLease(ref, *lease) || leaseRow.RunID != ref.RunID || leaseRow.WorkspaceID != ref.WorkspaceID || leaseRow.OwnerGeneration != ref.OwnerGeneration {
			return nil, fmt.Errorf("session: active process-session reference %q differs from its latest lease", ref.SessionID)
		}
		if err := validateProcessSessionLeaseRow(leaseRow); err != nil {
			return nil, fmt.Errorf("session: active process-session reference %q has an invalid latest lease: %w", ref.SessionID, err)
		}

		var start *durableActionOccurrence
		for _, occurrence := range plan.occurrences {
			if occurrence.intent == nil || occurrence.intent.RunID != ref.RunID || occurrence.intent.ActionID != ref.StartActionID {
				continue
			}
			if start != nil {
				return nil, fmt.Errorf("session: active process-session reference %q has multiple start actions", ref.SessionID)
			}
			start = occurrence
		}
		if start == nil || start.toolName != tools.ProcessSessionToolName || start.intent.ToolIdentity == nil ||
			start.intent.ToolIdentity.RegistrationSource != tools.RegistrationSourceBuiltin || start.intent.OwnerGeneration != ref.OwnerGeneration ||
			start.receipt == nil || start.receipt.DispatchState != DispatchStateDispatched || start.receipt.ReceiptOutcome != core.ReceiptOutcomeSuccess || start.resolution != nil {
			return nil, fmt.Errorf("session: active process-session reference %q lacks its unresolved successful start action", ref.SessionID)
		}
		if operation, ok := processSessionOccurrenceOperation(start); !ok || operation != tools.ProcessSessionOperationStart {
			return nil, fmt.Errorf("session: active process-session reference %q does not name a start operation", ref.SessionID)
		}
		expectedAction, err := processSessionActionRefForIntent(*start.intent, sessionID)
		if err != nil {
			return nil, fmt.Errorf("session: active process-session reference %q has an invalid start intent: %w", ref.SessionID, err)
		}
		if expectedAction.ActionID != ref.StartActionID || expectedAction.OwnerGeneration != ref.OwnerGeneration {
			return nil, fmt.Errorf("session: active process-session reference %q does not match its start intent", ref.SessionID)
		}
		var matchingExecution *SessionEntry
		for _, execution := range start.executions {
			if execution.DispatchState != DispatchStateDispatched || execution.ResourceState != core.RunExecutionDeliveredLive || execution.ProcessSessionLease == nil {
				continue
			}
			if matchingExecution != nil {
				return nil, fmt.Errorf("session: active process-session reference %q has multiple delivered-live executions", ref.SessionID)
			}
			matchingExecution = execution
		}
		if matchingExecution == nil || !sameProcessSessionLeaseIdentity(*matchingExecution.ProcessSessionLease, *lease) ||
			!activeProcessSessionRefMatchesLease(ref, *matchingExecution.ProcessSessionLease) ||
			matchingExecution.ProcessSessionLease.Phase != tools.ProcessSessionLeaseActive || matchingExecution.CompletionProof != "" {
			return nil, fmt.Errorf("session: active process-session reference %q lacks its exact delivered-live execution", ref.SessionID)
		}
		startActionRows := 0
		for _, row := range prefix {
			record := row.ProcessSessionAction
			if row.RecordVersion != 1 || row.Type != EntryTypeCheckpoint || row.CheckpointKind != CheckpointProcessSessionAction || record == nil ||
				record.LeaseSessionID != ref.SessionID || record.LeaseAction != lease.Action || record.Action != lease.Action || record.Operation != tools.ProcessSessionOperationStart {
				continue
			}
			if row.Seq <= start.receipt.Seq || row.RunID != ref.RunID || row.WorkspaceID != ref.WorkspaceID || row.SessionID != sessionID ||
				row.OwnerGeneration != ref.OwnerGeneration || row.ActionID != ref.StartActionID || row.ToolName != tools.ProcessSessionToolName {
				return nil, fmt.Errorf("session: active process-session reference %q has an invalid start-action checkpoint", ref.SessionID)
			}
			if _, err := findProcessSessionActionOccurrence(plan, record.Action, sessionID); err != nil {
				return nil, fmt.Errorf("session: active process-session reference %q has an invalid start-action checkpoint: %w", ref.SessionID, err)
			}
			startActionRows++
		}
		if startActionRows != 1 {
			return nil, fmt.Errorf("session: active process-session reference %q has %d synced start-action checkpoints", ref.SessionID, startActionRows)
		}

		latestAction, exists := latestActionRows[actionKey{ref.OwnerGeneration, ref.SessionID}]
		if !exists || latestAction.Seq <= start.receipt.Seq || latestAction.ProcessSessionAction == nil {
			return nil, fmt.Errorf("session: active process-session reference %q lacks a synced control action", ref.SessionID)
		}
		record := latestAction.ProcessSessionAction
		if latestAction.RunID != ref.RunID || latestAction.WorkspaceID != ref.WorkspaceID || latestAction.SessionID != sessionID ||
			latestAction.OwnerGeneration != ref.OwnerGeneration || latestAction.ActionBatchID != record.Action.ActionBatchID ||
			latestAction.ActionID != record.Action.ActionID || latestAction.ToolCallID != record.Action.ToolCallID ||
			latestAction.AssistantTranscriptOrdinal == nil || *latestAction.AssistantTranscriptOrdinal != record.Action.AssistantTranscriptOrdinal ||
			record.LeaseSessionID != ref.SessionID || record.LeaseAction != lease.Action {
			return nil, fmt.Errorf("session: active process-session reference %q has a mismatched control action", ref.SessionID)
		}
		controlOccurrence, err := findProcessSessionActionOccurrence(plan, record.Action, sessionID)
		if err != nil {
			return nil, fmt.Errorf("session: active process-session reference %q has an invalid control action: %w", ref.SessionID, err)
		}
		operation, ok := processSessionOccurrenceOperation(controlOccurrence)
		if !ok || operation != record.Operation {
			return nil, fmt.Errorf("session: active process-session reference %q control operation differs from its tool call", ref.SessionID)
		}
		if controlOccurrence.receipt == nil || latestAction.Seq <= controlOccurrence.receipt.Seq {
			return nil, fmt.Errorf("session: active process-session reference %q control action was not synced after its receipt", ref.SessionID)
		}
		if record.Operation != tools.ProcessSessionOperationStart && record.Operation != tools.ProcessSessionOperationRead && record.Operation != tools.ProcessSessionOperationStatus && record.Operation != tools.ProcessSessionOperationStop {
			return nil, fmt.Errorf("session: active process-session reference %q has an unsupported control operation", ref.SessionID)
		}
		allowedStartActions[ref.StartActionID] = struct{}{}
	}
	return allowedStartActions, nil
}

func activeProcessSessionRefMatchesLease(ref core.ActiveProcessSessionRef, lease tools.ProcessSessionLeaseSnapshot) bool {
	return ref.OwnerID == lease.OwnerID && ref.WorkspaceID == lease.WorkspaceID && ref.RunID == lease.RunID &&
		ref.SessionID == lease.SessionID && ref.OwnerGeneration == lease.OwnerGeneration && ref.StartActionID == lease.Action.ActionID
}

func processSessionActionRefForIntent(intent SessionEntry, sessionID string) (tools.ProcessSessionActionRef, error) {
	if intent.AssistantTranscriptOrdinal == nil {
		return tools.ProcessSessionActionRef{}, errors.New("intent has no assistant occurrence")
	}
	argumentDigest, err := hex.DecodeString(intent.ArgumentDigest)
	if err != nil || len(argumentDigest) != 32 {
		return tools.ProcessSessionActionRef{}, errors.New("intent has an invalid argument digest")
	}
	schemaDigest, err := hex.DecodeString(intent.SchemaDigest)
	if err != nil || len(schemaDigest) != 32 {
		return tools.ProcessSessionActionRef{}, errors.New("intent has an invalid schema digest")
	}
	var argument, schema [32]byte
	copy(argument[:], argumentDigest)
	copy(schema[:], schemaDigest)
	return tools.ProcessSessionActionRef{
		SessionID: sessionID, WorkspaceID: intent.WorkspaceID, RunID: intent.RunID,
		ActionBatchID: intent.ActionBatchID, ActionID: intent.ActionID, OwnerGeneration: intent.OwnerGeneration,
		AssistantTranscriptOrdinal: *intent.AssistantTranscriptOrdinal, ToolCallID: intent.ToolCallID,
		SchemaDigest: schema, ArgumentDigest: argument,
	}, nil
}

func processSessionOccurrenceOperation(occurrence *durableActionOccurrence) (string, bool) {
	if occurrence == nil || occurrence.callIndex < 0 || occurrence.callIndex >= len(occurrence.assistant.ToolCalls) {
		return "", false
	}
	args, ok := occurrence.assistant.ToolCalls[occurrence.callIndex].Args.(map[string]any)
	if !ok {
		return "", false
	}
	operation, ok := args["operation"].(string)
	return operation, ok
}

func findProcessSessionActionOccurrence(plan *durableRecoveryPlan, action tools.ProcessSessionActionRef, sessionID string) (*durableActionOccurrence, error) {
	if plan == nil {
		return nil, errors.New("action journal is unavailable")
	}
	for _, occurrence := range plan.occurrences {
		if occurrence.intent == nil || occurrence.intent.RunID != action.RunID || occurrence.intent.ActionID != action.ActionID {
			continue
		}
		if occurrence.toolName != tools.ProcessSessionToolName || occurrence.intent.ToolIdentity == nil || occurrence.intent.ToolIdentity.RegistrationSource != tools.RegistrationSourceBuiltin {
			return nil, errors.New("recorded control action is not the trusted process_session tool")
		}
		expected, err := processSessionActionRefForIntent(*occurrence.intent, sessionID)
		if err != nil {
			return nil, err
		}
		if expected != action {
			return nil, errors.New("recorded control action differs from its durable intent")
		}
		if occurrence.receipt == nil || occurrence.receipt.DispatchState != DispatchStateDispatched || occurrence.receipt.ReceiptOutcome != core.ReceiptOutcomeSuccess || occurrence.receipt.Seq == 0 {
			return nil, errors.New("recorded control action has no synced successful dispatched receipt")
		}
		return occurrence, nil
	}
	return nil, errors.New("recorded control action has no durable intent")
}
