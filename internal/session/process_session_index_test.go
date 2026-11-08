package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestProcessSessionAdmissionDoesNotClearUnrelatedActionIDCollision(t *testing.T) {
	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	defer cancelOwner()
	manager, err := tools.NewLocalProcessSessionManager(ownerCtx, tools.ProcessSessionManagerConfig{
		OwnerID: "owner", WorkspaceID: "workspace", Workdir: t.TempDir(), Sandbox: tools.AllowAllSandbox{},
		ScopeResolver: func(context.Context) (tools.ProcessSessionScope, error) {
			return tools.ProcessSessionScope{OwnerID: "owner", WorkspaceID: "workspace", RunID: "run-a"}, nil
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, manager.Close(context.Background())) })

	startAction := tools.ProcessSessionActionRef{
		SessionID: "session-a", WorkspaceID: "workspace", RunID: "run-a", ActionBatchID: "batch-a",
		ActionID: "shared-action-id", OwnerGeneration: "owner-generation", AssistantTranscriptOrdinal: 1,
		ToolCallID: "call-a", SchemaDigest: [32]byte{1}, ArgumentDigest: [32]byte{2},
	}
	tool := tools.NewProcessSessionTool(manager)
	started, err := tool.Execute(tools.WithProcessSessionAction(context.Background(), startAction, nil), tools.ToolCall{
		Name: "process_session", Args: map[string]any{"operation": "start", "command": "exec sleep 30"},
	})
	if errors.Is(err, tools.ErrProcessSessionsUnsupported) {
		t.Skip(err)
	}
	require.NoError(t, err)
	require.NotNil(t, started.ProcessSessionLease)
	capability, ok := manager.ManagerAdmissionCapability("run-a", startAction.OwnerGeneration)
	require.True(t, ok)

	identity := &tools.TrustedToolIdentitySnapshot{
		RegistrationSource: tools.RegistrationSourceBuiltin, ToolName: "process_session", RegistrationGeneration: 1,
		ExecutorFingerprint: strings.Repeat("0", 64), ScopeFingerprint: strings.Repeat("1", 64),
	}
	startRef := pendingRefFromProcessSessionAction(startAction)
	state := &workspaceIndexState{
		processSessions: map[string]tools.ProcessSessionLeaseSnapshot{
			startAction.SessionID: tools.CloneProcessSessionLeaseSnapshot(*started.ProcessSessionLease),
		},
		actionsByKey: map[workspaceActionKey]*workspaceIndexedAction{
			workspaceActionFor(startRef): {
				ref: startRef,
				action: WorkspaceDispatchAction{
					Action: startRef, OwnerGeneration: startAction.OwnerGeneration, ToolIdentity: identity,
				},
			},
		},
	}
	batch := WorkspaceDispatchBatch{
		WorkspaceID: "workspace", SessionID: "session-a", RunID: "run-a", ActionBatchID: "batch-next",
		ProcessSessionAdmission: &capability,
		Actions: []WorkspaceDispatchAction{{
			Action: PendingActionRef{
				SessionID: "session-a", WorkspaceID: "workspace", RunID: "run-a", ActionBatchID: "batch-next",
				ActionID: "next-action", AssistantTranscriptOrdinal: 2, ToolCallID: "call-next",
			},
			OwnerGeneration: startAction.OwnerGeneration,
		}},
	}
	require.True(t, state.allowsProcessSessionAdmission(batch))

	foreignRef := startRef
	foreignRef.SessionID = "session-b"
	foreignRef.RunID = "run-b"
	foreignRef.ActionBatchID = "batch-b"
	foreignRef.ToolCallID = "call-b"
	state.actionsByKey[workspaceActionFor(foreignRef)] = &workspaceIndexedAction{
		ref:    foreignRef,
		action: WorkspaceDispatchAction{Action: foreignRef, OwnerGeneration: startAction.OwnerGeneration, ToolIdentity: identity},
	}
	require.False(t, state.allowsProcessSessionAdmission(batch), "a same-ActionID marker from another run/session must remain blocking")
}

func TestApplyProcessSessionLeaseReplayAfterReopenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	row := processSessionLeaseIndexTestRow()
	require.NoError(t, index.EnsureWorkspace(ctx, row.WorkspaceID))
	require.NoError(t, index.ApplyProcessSessionLease(ctx, row))

	reopened := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	require.NoError(t, reopened.EnsureWorkspace(ctx, row.WorkspaceID))
	require.NoError(t, reopened.ApplyProcessSessionLease(ctx, row))
}

func TestApplyProcessSessionLeaseReplayRejectsChangedPayload(t *testing.T) {
	ctx := context.Background()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	row := processSessionLeaseIndexTestRow()
	require.NoError(t, index.EnsureWorkspace(ctx, row.WorkspaceID))
	require.NoError(t, index.ApplyProcessSessionLease(ctx, row))

	conflicting := row.clone()
	conflicting.ProcessSessionLease.OutputBytes++
	require.ErrorContains(t, index.ApplyProcessSessionLease(ctx, *conflicting), "process-session workspace record ID conflicts with its durable row")
}

func processSessionLeaseIndexTestRow() SessionEntry {
	action := tools.ProcessSessionActionRef{
		SessionID: "physical-session", WorkspaceID: "workspace-a", RunID: "run-a", ActionBatchID: "batch-a",
		ActionID: "start-action", OwnerGeneration: "generation-a", AssistantTranscriptOrdinal: 1,
		ToolCallID: "start-call", SchemaDigest: [32]byte{1}, ArgumentDigest: [32]byte{2},
	}
	lease := tools.ProcessSessionLeaseSnapshot{
		SessionID: "opaque-process-session", OwnerID: "owner-a", WorkspaceID: action.WorkspaceID, RunID: action.RunID,
		OwnerGeneration: action.OwnerGeneration, Action: action, Phase: tools.ProcessSessionLeaseGated,
		Identity: &tools.ProcessExecutionIdentity{
			Platform: "darwin", HostGeneration: "host-a", LeaderPID: 1234,
			ProcessStartToken: "birth-a", ProcessGroupID: 1234,
		},
	}
	return SessionEntry{
		ID: "lease-gated", RecordVersion: 1, Seq: 27, Type: EntryTypeCheckpoint,
		SessionID: action.SessionID, RunID: action.RunID, WorkspaceID: action.WorkspaceID,
		ActionBatchID: action.ActionBatchID, ActionID: action.ActionID, OwnerGeneration: action.OwnerGeneration,
		ToolCallID: action.ToolCallID, ToolName: tools.ProcessSessionToolName,
		CheckpointKind: CheckpointProcessSessionLease, ProcessSessionLease: &lease,
	}
}

func pendingRefFromProcessSessionAction(action tools.ProcessSessionActionRef) PendingActionRef {
	return PendingActionRef{
		SessionID: action.SessionID, WorkspaceID: action.WorkspaceID, RunID: action.RunID,
		ActionBatchID: action.ActionBatchID, AssistantTranscriptOrdinal: action.AssistantTranscriptOrdinal,
		ToolCallID: action.ToolCallID, ActionID: action.ActionID,
	}
}
