package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPendingInspectionUsesExactOffBranchResolutionWithoutClearingOtherAction(t *testing.T) {
	fixture := createRecoveryFixture(t, true)
	appendFixtureRows(t, fixture,
		SessionEntry{ID: "known-execution", ParentID: fixture.intent.ID, Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1", ActionBatchID: "batch-1", ActionID: "action-1", ActionState: ActionStateExecution, AssistantTranscriptOrdinal: ptrUint64(0), ToolCallID: "call-1", ToolName: "read", DispatchState: DispatchStateDispatched, ResourceState: core.RunExecutionQuiescent, CompletionProof: tools.CompletionInlinePipelineJoined},
		SessionEntry{ID: "known-receipt", ParentID: "known-execution", Type: EntryTypeTool, RunID: "run-1", WorkspaceID: "workspace-1", ActionBatchID: "batch-1", ActionID: "action-1", AssistantTranscriptOrdinal: ptrUint64(0), TranscriptIndex: ptrUint64(2), ToolCallID: "call-1", ToolName: "read", DispatchState: DispatchStateDispatched, ReceiptOutcome: core.ReceiptOutcomeSuccess, Content: "known effect"},
		SessionEntry{ID: "known-resolution", ParentID: "known-receipt", Type: EntryTypeCheckpoint, RunID: "run-1", WorkspaceID: "workspace-1", ActionBatchID: "batch-1", ActionID: "action-1", AssistantTranscriptOrdinal: ptrUint64(0), ToolCallID: "call-1", CheckpointKind: CheckpointExecutionResolution, ExecutionResolution: &core.ExecutionResolution{RunID: "run-1", ActionID: "action-1", Evidence: core.ResolutionLocalResourcesStopped, EvidenceRef: "known-execution"}},
		SessionEntry{ID: "known-error-terminal", ParentID: "known-resolution", Type: EntryTypeCheckpoint, RunID: "run-1", CheckpointKind: CheckpointRunTerminal, RunOutcome: core.RunOutcomeError})
	prepared, err := NewDurableJSONLSessionPreparer(fixture.directory, recoveryAuthorityFunc{}).PrepareResume(t.Context(), fixture.sessionID)
	require.NoError(t, err)
	defer func() { require.NoError(t, prepared.Close()) }()
	before, err := prepared.Snapshot()
	require.NoError(t, err)
	fork, err := prepared.(HistoryEditor).EditHistory(t.Context(), HistoryEdit{Expected: HistoryCoordinatesFromSnapshot(before), Kind: HistoryFork, TargetEntryID: "known-receipt", NewBranchID: "from-receipt"})
	require.NoError(t, err)
	pending, err := InspectPendingActions(fork)
	require.NoError(t, err)
	require.Empty(t, pending)
	found, ok, err := FindActionByID(fork, "action-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.True(t, found.Resolved)
	_, err = prepared.DurableWriter().AppendAndSync(t.Context(),
		SessionEntry{ID: "other-user", ParentID: fork.ActiveLeafEntryID, Type: EntryTypeUser, RunID: "run-2", TranscriptIndex: ptrUint64(0), Content: "different work"},
		SessionEntry{ID: "other-assistant", ParentID: "other-user", Type: EntryTypeAssistant, RunID: "run-2", WorkspaceID: "workspace-1", TranscriptIndex: ptrUint64(1), AssistantTranscriptOrdinal: ptrUint64(0), ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read", Args: map[string]any{"path": "README.md"}}}},
		SessionEntry{ID: "other-intent", ParentID: "other-assistant", Type: EntryTypeTool, RunID: "run-2", WorkspaceID: "workspace-1", ActionBatchID: "batch-2", ActionID: "action-2", ActionState: ActionStateIntent, AssistantTranscriptOrdinal: ptrUint64(0), ToolCallID: "call-1", ToolName: "read", ArgumentDigest: fixture.intent.ArgumentDigest, SchemaDigest: fixture.intent.SchemaDigest, ToolIdentity: fixture.intent.ToolIdentity, EffectSet: fixture.intent.EffectSet})
	require.NoError(t, err)
	after, err := prepared.Snapshot()
	require.NoError(t, err)
	pending, err = InspectPendingActions(after)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, "action-2", pending[0].Action.ActionID)
	require.False(t, pending[0].Resolved, "same provider call ID must not borrow another action's resolution")
	bad := after.Clone()
	for index := range bad.DurableEntries {
		if bad.DurableEntries[index].ID == "known-execution" {
			bad.DurableEntries[index].CompletionProof = "looks-joined"
		}
	}
	_, err = InspectPendingActions(bad)
	require.Error(t, err, "off-branch invalid proof must still fail closed")
}
