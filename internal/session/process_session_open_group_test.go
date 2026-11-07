package session

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestOpenToolGroupAcceptsOnlyExactProcessSessionActionAndActivePrefixes(t *testing.T) {
	valid := openProcessSessionGroupRows()
	err := validateToolCallJournalTail("physical-session", valid)
	require.ErrorIs(t, err, errUnclosedToolCallGroup)

	testCases := []struct {
		name   string
		mutate func([]SessionEntry) []SessionEntry
	}{
		{
			name: "action checkpoint before its own receipt",
			mutate: func(rows []SessionEntry) []SessionEntry {
				return moveEntry(rows, "process-action", 5)
			},
		},
		{
			name: "wrong current group occurrence",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[6].ProcessSessionAction.Action.ToolCallID = "foreign-call"
				return rows
			},
		},
		{
			name: "wrong lease owner generation",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[6].ProcessSessionAction.LeaseAction.OwnerGeneration = "foreign-generation"
				return rows
			},
		},
		{
			name: "wrong lease argument digest",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[6].ProcessSessionAction.LeaseAction.ArgumentDigest = [32]byte{9}
				return rows
			},
		},
		{
			name: "wrong lease session selector",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[6].ProcessSessionAction.LeaseSessionID = "foreign-session"
				return rows
			},
		},
		{
			name: "active lease before start action checkpoint",
			mutate: func(rows []SessionEntry) []SessionEntry {
				return moveEntry(rows, "active-lease", 6)
			},
		},
		{
			name: "missing delivered-live execution proof",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[4].ResourceState = core.RunExecutionQuiescent
				return rows
			},
		},
		{
			name: "generic checkpoint interleaving",
			mutate: func(rows []SessionEntry) []SessionEntry {
				checkpoint := SessionEntry{
					ID: "generic-checkpoint", ParentID: "start-receipt", RecordVersion: 1, Seq: 7,
					Type: EntryTypeCheckpoint, SessionID: "physical-session", RunID: "run-a",
					WorkspaceID: "workspace-a", CheckpointKind: CheckpointRunTerminal,
				}
				rows = append(rows[:6], append([]SessionEntry{checkpoint}, rows[6:]...)...)
				return rows
			},
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			rows := test.mutate(openProcessSessionGroupRows())
			if err := validateToolCallJournalTail("physical-session", rows); err == nil || err == errUnclosedToolCallGroup {
				t.Fatalf("expected the invalid process-session prefix to be rejected, got %v", err)
			}
		})
	}
}

func TestOpenToolGroupAllowsOnlyExactPositiveStoppedLeasePrefixes(t *testing.T) {
	stoppedAfterReceipt := stoppedProcessSessionGroupRows()
	err := validateToolCallJournalTail("physical-session", stoppedAfterReceipt)
	require.ErrorIs(t, err, errUnclosedToolCallGroup)

	stoppedBeforeReceipt := openProcessSessionGroupRows()[:4]
	stoppedBeforeReceipt = append(stoppedBeforeReceipt, stoppedProcessLeaseRow("stopped-lease", "gated-lease", 5))
	err = validateToolCallJournalTail("physical-session", stoppedBeforeReceipt)
	require.ErrorIs(t, err, errUnclosedToolCallGroup)

	testCases := []struct {
		name   string
		mutate func([]SessionEntry) []SessionEntry
	}{
		{
			name: "stopped lease lacks joined readers",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[len(rows)-1].ProcessSessionLease.ReadersJoined = false
				return rows
			},
		},
		{
			name: "stopped lease lacks group absence",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[len(rows)-1].ProcessSessionLease.GroupAbsent = false
				return rows
			},
		},
		{
			name: "stopped lease lacks leader join",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[len(rows)-1].ProcessSessionLease.LeaderWaited = false
				return rows
			},
		},
		{
			name: "stopped lease belongs to another occurrence",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[len(rows)-1].ProcessSessionLease.Action.ToolCallID = "foreign-call"
				return rows
			},
		},
		{
			name: "stopped lease has wrong owner generation",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[len(rows)-1].ProcessSessionLease.OwnerGeneration = "foreign-generation"
				return rows
			},
		},
		{
			name: "stopped lease has a different native process identity",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[len(rows)-1].ProcessSessionLease.Identity.ProcessStartToken = "birth-b"
				return rows
			},
		},
		{
			name: "stopped lease has a different start argument digest",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[len(rows)-1].ProcessSessionLease.Action.ArgumentDigest = [32]byte{7}
				return rows
			},
		},
		{
			name: "stopped lease follows a failed receipt",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[5].ReceiptOutcome = core.ReceiptOutcomeError
				return rows
			},
		},
		{
			name: "stopped lease cannot resurrect to active",
			mutate: func(rows []SessionEntry) []SessionEntry {
				active := processLeaseRow("resurrected-active", "stopped-lease", uint64(len(rows)+1), *rows[len(rows)-1].ProcessSessionLease, tools.ProcessSessionLeaseActive)
				active.ParentID = rows[len(rows)-1].ID
				rows = append(rows, active)
				return rows
			},
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			rows := test.mutate(stoppedProcessSessionGroupRows())
			err := validateToolCallJournalTail("physical-session", rows)
			require.Error(t, err)
			require.NotErrorIs(t, err, errUnclosedToolCallGroup)
		})
	}
}

func TestDurablePreReceiptStoppedLeaseResolvesAfterReceiptUsingSameRow(t *testing.T) {
	prefix := openProcessSessionGroupRows()[:4]
	prefix[1].ToolCalls = prefix[1].ToolCalls[:1]
	prefix = append(prefix, stoppedProcessLeaseRow("stopped-lease", "gated-lease", 5))
	err := validateToolCallJournalTail("physical-session", prefix)
	require.ErrorIs(t, err, errUnclosedToolCallGroup, "the start intent must remain unresolved until its actual receipt")

	rows := completedPreReceiptStoppedGroupRows()
	require.NoError(t, validateToolCallJournalTail("physical-session", rows))
	var resolution *core.ExecutionResolution
	for _, row := range rows {
		if row.ExecutionResolution != nil {
			resolution = row.ExecutionResolution
		}
	}
	require.NotNil(t, resolution)
	require.Equal(t, "stopped-lease", resolution.EvidenceRef)
	require.Equal(t, "stopped-lease", resolution.ProcessSessionClosureRef, "receipt/action completion must reuse the exact earlier Stopped row")

	futureClosure := moveEntry(completedPreReceiptStoppedGroupRows(), "stopped-lease", 8)
	futureClosure[len(futureClosure)-1].ParentID = "start-resolution"
	futureClosure[len(futureClosure)-1].Seq = 10
	require.Error(t, validateToolCallJournalTail("physical-session", futureClosure), "a future closure row cannot prove an earlier resolution")
}

func TestOpenToolGroupAllowsExactPriorRunProcessSessionClosureMetadata(t *testing.T) {
	rows := priorProcessSessionClosureDuringOpenGroupRows()
	err := validateToolCallJournalTail("physical-session", rows)
	require.ErrorIs(t, err, errUnclosedToolCallGroup)

	tests := []struct {
		name   string
		mutate func([]SessionEntry) []SessionEntry
	}{
		{
			name: "different current run",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[8].RunID = "foreign-run"
				return rows
			},
		},
		{
			name: "unknown prior phase",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[7].ProcessSessionLease.Phase = tools.ProcessSessionLeaseUnknown
				return rows
			},
		},
		{
			name: "different native identity",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[9].ProcessSessionLease.Identity.ProcessStartToken = "birth-b"
				return rows
			},
		},
		{
			name: "wrong owner generation",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[9].ProcessSessionLease.OwnerGeneration = "foreign-generation"
				rows[9].ProcessSessionLease.Action.OwnerGeneration = "foreign-generation"
				rows[9].OwnerGeneration = "foreign-generation"
				return rows
			},
		},
		{
			name: "incomplete closure proof",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[9].ProcessSessionLease.ReadersJoined = false
				return rows
			},
		},
		{
			name: "wrong origin action",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[9].ProcessSessionLease.Action.ActionID = "foreign-action"
				rows[9].ActionID = "foreign-action"
				return rows
			},
		},
		{
			name: "resolution precedes stopped lease",
			mutate: func(rows []SessionEntry) []SessionEntry {
				return moveEntry(rows, "prior-start-resolution", 9)
			},
		},
		{
			name: "resolution references a missing closure",
			mutate: func(rows []SessionEntry) []SessionEntry {
				rows[10].ExecutionResolution.ProcessSessionClosureRef = "future-closure"
				rows[10].ExecutionResolution.EvidenceRef = "future-closure"
				return rows
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateToolCallJournalTail("physical-session", test.mutate(priorProcessSessionClosureDuringOpenGroupRows()))
			require.Error(t, err)
			require.NotErrorIs(t, err, errUnclosedToolCallGroup)
		})
	}
}

func priorProcessSessionClosureDuringOpenGroupRows() []SessionEntry {
	rows := openProcessSessionGroupRows()
	rows[1].ToolCalls = rows[1].ToolCalls[:1]
	ordinal := uint64(1)
	transcriptIndex := uint64(3)
	rows = append(rows, SessionEntry{
		ID: "next-assistant", ParentID: "active-lease", RecordVersion: 1, Seq: 9,
		Type: EntryTypeAssistant, SessionID: "physical-session", BranchID: "main", RunID: "run-a", WorkspaceID: "workspace-a",
		TranscriptIndex: &transcriptIndex, AssistantTranscriptOrdinal: &ordinal,
		ToolCalls: []llm.ToolCall{{ID: "read-call", Name: "read", Args: map[string]any{"path": "README.md"}}},
	})
	stopped := stoppedProcessLeaseRow("prior-stopped-lease", "next-assistant", 10)
	rows = append(rows, stopped)
	rows = append(rows, SessionEntry{
		ID: "prior-start-resolution", ParentID: stopped.ID, RecordVersion: 1, Seq: 11,
		Type: EntryTypeCheckpoint, SessionID: "physical-session", RunID: "run-a", WorkspaceID: "workspace-a",
		ActionBatchID: "batch-a", ActionID: "start-action", OwnerGeneration: "generation-a",
		AssistantTranscriptOrdinal: uint64Pointer(0), ToolCallID: "start-call", ToolName: "process_session",
		CheckpointKind: CheckpointExecutionResolution,
		ExecutionResolution: &core.ExecutionResolution{
			RunID: "run-a", ActionID: "start-action", Evidence: core.ResolutionLocalResourcesStopped,
			EvidenceRef: stopped.ID, ProcessSessionClosureRef: stopped.ID,
		},
	})
	return rows
}

func completedPreReceiptStoppedGroupRows() []SessionEntry {
	base := openProcessSessionGroupRows()
	rows := append([]SessionEntry(nil), base[:4]...)
	rows[1].ToolCalls = rows[1].ToolCalls[:1]
	stopped := stoppedProcessLeaseRow("stopped-lease", "gated-lease", 5)
	rows = append(rows, stopped)
	execution := base[4]
	execution.ParentID = stopped.ID
	execution.Seq = 6
	rows = append(rows, execution)
	receipt := base[5]
	receipt.ParentID = execution.ID
	receipt.Seq = 7
	rows = append(rows, receipt)
	action := base[6]
	action.ParentID = receipt.ID
	action.Seq = 8
	rows = append(rows, action)
	rows = append(rows, SessionEntry{
		ID: "start-resolution", ParentID: action.ID, RecordVersion: 1, Seq: 9,
		Type: EntryTypeCheckpoint, SessionID: "physical-session", RunID: "run-a", WorkspaceID: "workspace-a",
		ActionBatchID: "batch-a", ActionID: "start-action", OwnerGeneration: "generation-a",
		AssistantTranscriptOrdinal: uint64Pointer(0), ToolCallID: "start-call", ToolName: "process_session",
		CheckpointKind: CheckpointExecutionResolution,
		ExecutionResolution: &core.ExecutionResolution{
			RunID: "run-a", ActionID: "start-action", Evidence: core.ResolutionLocalResourcesStopped,
			EvidenceRef: "stopped-lease", ProcessSessionClosureRef: "stopped-lease",
		},
	})
	return rows
}

func stoppedProcessSessionGroupRows() []SessionEntry {
	rows := openProcessSessionGroupRows()
	rows = rows[:6]
	rows = append(rows, stoppedProcessLeaseRow("stopped-lease", "start-receipt", uint64(len(rows)+1)))
	return rows
}

func stoppedProcessLeaseRow(id, parent string, seq uint64) SessionEntry {
	rows := openProcessSessionGroupRows()
	lease := tools.CloneProcessSessionLeaseSnapshot(*rows[3].ProcessSessionLease)
	lease.Phase = tools.ProcessSessionLeaseStopped
	lease.Status = tools.ProcessSessionExited
	lease.GroupAbsent = true
	lease.LeaderWaited = true
	lease.ReadersJoined = true
	return SessionEntry{
		ID: id, ParentID: parent, RecordVersion: 1, Seq: seq, Type: EntryTypeCheckpoint,
		SessionID: "physical-session", RunID: "run-a", WorkspaceID: "workspace-a", ActionBatchID: "batch-a",
		ActionID: "start-action", OwnerGeneration: "generation-a", AssistantTranscriptOrdinal: uint64Pointer(0),
		ToolCallID: "start-call", ToolName: "process_session", CheckpointKind: CheckpointProcessSessionLease,
		ProcessSessionLease: &lease,
	}
}

func openProcessSessionGroupRows() []SessionEntry {
	ordinal := uint64(0)
	userIndex := uint64(0)
	assistantIndex := uint64(1)
	receiptIndex := uint64(2)
	action := tools.ProcessSessionActionRef{
		SessionID: "physical-session", WorkspaceID: "workspace-a", RunID: "run-a", ActionBatchID: "batch-a",
		ActionID: "start-action", OwnerGeneration: "generation-a", AssistantTranscriptOrdinal: ordinal,
		ToolCallID: "start-call", SchemaDigest: [32]byte{1}, ArgumentDigest: [32]byte{2},
	}
	identity := &tools.ProcessExecutionIdentity{
		Platform: "darwin", HostGeneration: "host-a", LeaderPID: 1234,
		ProcessStartToken: "birth-a", ProcessGroupID: 1234,
	}
	lease := tools.ProcessSessionLeaseSnapshot{
		SessionID: "opaque-process-session", OwnerID: "owner-a", WorkspaceID: "workspace-a", RunID: "run-a",
		OwnerGeneration: "generation-a", Action: action, Phase: tools.ProcessSessionLeaseActive, Identity: identity,
	}
	toolIdentity := &tools.TrustedToolIdentitySnapshot{
		RegistrationSource: tools.RegistrationSourceBuiltin, ToolName: "process_session", RegistrationGeneration: 1,
		ExecutorFingerprint: strings.Repeat("3", 64), ScopeFingerprint: strings.Repeat("4", 64),
	}
	argumentDigest := hex.EncodeToString(action.ArgumentDigest[:])
	schemaDigest := hex.EncodeToString(action.SchemaDigest[:])
	return []SessionEntry{
		{ID: "user", RecordVersion: 1, Seq: 1, Type: EntryTypeUser, SessionID: "physical-session", BranchID: "main", RunID: "run-a", WorkspaceID: "workspace-a", TranscriptIndex: &userIndex, Content: "start the service"},
		{ID: "assistant", ParentID: "user", RecordVersion: 1, Seq: 2, Type: EntryTypeAssistant, SessionID: "physical-session", BranchID: "main", RunID: "run-a", WorkspaceID: "workspace-a", TranscriptIndex: &assistantIndex, AssistantTranscriptOrdinal: &ordinal, ToolCalls: []llm.ToolCall{
			{ID: "start-call", Name: "process_session", Args: map[string]any{"operation": "start", "command": "exec sleep 30"}},
			{ID: "ordinary-call", Name: "bash", Args: map[string]any{"command": "printf done"}},
		}},
		{ID: "start-intent", ParentID: "assistant", RecordVersion: 1, Seq: 3, Type: EntryTypeTool, SessionID: "physical-session", BranchID: "main", RunID: "run-a", WorkspaceID: "workspace-a", ActionBatchID: "batch-a", ActionID: "start-action", OwnerGeneration: "generation-a", AssistantTranscriptOrdinal: &ordinal, ToolCallID: "start-call", ToolName: "process_session", ActionState: ActionStateIntent, ArgumentDigest: argumentDigest, SchemaDigest: schemaDigest, ToolIdentity: toolIdentity, EffectSet: []tools.TrustedToolEffect{tools.EffectControl}},
		processLeaseRow("gated-lease", "start-intent", 4, lease, tools.ProcessSessionLeaseGated),
		{ID: "start-execution", ParentID: "gated-lease", RecordVersion: 1, Seq: 5, Type: EntryTypeTool, SessionID: "physical-session", BranchID: "main", RunID: "run-a", WorkspaceID: "workspace-a", ActionBatchID: "batch-a", ActionID: "start-action", OwnerGeneration: "generation-a", AssistantTranscriptOrdinal: &ordinal, ToolCallID: "start-call", ToolName: "process_session", ActionState: ActionStateExecution, DispatchState: core.DispatchStateDispatched, ResourceState: core.RunExecutionDeliveredLive, ProcessSessionLease: leasePointer(lease, tools.ProcessSessionLeaseActive)},
		{ID: "start-receipt", ParentID: "start-execution", RecordVersion: 1, Seq: 6, Type: EntryTypeTool, SessionID: "physical-session", BranchID: "main", RunID: "run-a", WorkspaceID: "workspace-a", ActionBatchID: "batch-a", ActionID: "start-action", OwnerGeneration: "generation-a", AssistantTranscriptOrdinal: &ordinal, TranscriptIndex: &receiptIndex, ToolCallID: "start-call", ToolName: "process_session", DispatchState: core.DispatchStateDispatched, ReceiptOutcome: core.ReceiptOutcomeSuccess},
		{ID: "process-action", ParentID: "start-receipt", RecordVersion: 1, Seq: 7, Type: EntryTypeCheckpoint, SessionID: "physical-session", RunID: "run-a", WorkspaceID: "workspace-a", ActionBatchID: "batch-a", ActionID: "start-action", OwnerGeneration: "generation-a", AssistantTranscriptOrdinal: &ordinal, ToolCallID: "start-call", ToolName: "process_session", CheckpointKind: CheckpointProcessSessionAction, ProcessSessionAction: &tools.ProcessSessionActionRecord{LeaseSessionID: lease.SessionID, LeaseAction: action, Action: action, Operation: "start"}},
		processLeaseRow("active-lease", "process-action", 8, lease, tools.ProcessSessionLeaseActive),
	}
}

func processLeaseRow(id, parent string, seq uint64, lease tools.ProcessSessionLeaseSnapshot, phase tools.ProcessSessionLeasePhase) SessionEntry {
	lease = tools.CloneProcessSessionLeaseSnapshot(lease)
	lease.Phase = phase
	return SessionEntry{
		ID: id, ParentID: parent, RecordVersion: 1, Seq: seq, Type: EntryTypeCheckpoint,
		SessionID: "physical-session", RunID: "run-a", WorkspaceID: "workspace-a", ActionBatchID: "batch-a",
		ActionID: "start-action", OwnerGeneration: "generation-a", AssistantTranscriptOrdinal: uint64Pointer(0),
		ToolCallID: "start-call", ToolName: "process_session", CheckpointKind: CheckpointProcessSessionLease,
		ProcessSessionLease: &lease,
	}
}

func leasePointer(lease tools.ProcessSessionLeaseSnapshot, phase tools.ProcessSessionLeasePhase) *tools.ProcessSessionLeaseSnapshot {
	lease = tools.CloneProcessSessionLeaseSnapshot(lease)
	lease.Phase = phase
	return &lease
}

func moveEntry(rows []SessionEntry, id string, index int) []SessionEntry {
	entryIndex := -1
	for i := range rows {
		if rows[i].ID == id {
			entryIndex = i
			break
		}
	}
	if entryIndex < 0 || index < 0 || index >= len(rows) {
		return rows
	}
	entry := rows[entryIndex]
	rows = append(rows[:entryIndex], rows[entryIndex+1:]...)
	rows = append(rows, SessionEntry{})
	copy(rows[index+1:], rows[index:])
	rows[index] = entry
	return rows
}
