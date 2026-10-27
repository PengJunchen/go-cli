package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type delegationIndexDispatcher struct{}

func (*delegationIndexDispatcher) Dispatch(context.Context, tools.SubagentTask) (tools.SubagentResult, error) {
	return tools.SubagentResult{}, nil
}
func (*delegationIndexDispatcher) ParallelDispatch(context.Context, []tools.SubagentTask) ([]tools.SubagentResult, error) {
	return nil, nil
}
func (*delegationIndexDispatcher) ListRunning() []tools.SubagentTask { return nil }

func preparedDelegationBatch(t *testing.T, workspace, root, id string, owned bool) WorkspaceDispatchBatch {
	t.Helper()
	ctx := context.Background()
	registry := tools.NewMiddlewareToolRegistry(tools.NewDefaultToolRegistry())
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	var definition tools.ToolDefinition = tools.NewSubagentTool(&delegationIndexDispatcher{})
	if owned {
		definition = tools.NewOwnedSubagentTool(&delegationIndexDispatcher{})
	}
	require.NoError(t, registry.RegisterTrustedInlineControl(ctx, definition))
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{ID: id, Name: "dispatch_subagent", Args: map[string]any{"prompt": "independent child"}})
	require.NoError(t, err)
	proof, ok := tools.ManagedDelegationProofFor(invocation, root)
	require.Equal(t, owned, ok)
	batch := workspaceTestBatch(workspace, "session-"+id, "run-"+id, "batch-"+id, "action-"+id, 0, id)
	identity := invocation.ToolIdentity().Snapshot()
	batch.Actions[0].ToolIdentity = &identity
	batch.Actions[0].EffectSet = invocation.EffectSet().Values()
	batch.Actions[0].ManagedDelegationProof = proof
	batch.CanonicalWorkspaceRoot = root
	return batch
}

func TestManagedDelegationMarkerNeverExemptsUnknownChildAcrossRestart(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workspace, err := WorkspaceIDForCanonicalRoot(root)
	require.NoError(t, err)
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspace))
	delegation := preparedDelegationBatch(t, workspace, root, "parent", true)
	result, err := index.BeginDispatchBatch(ctx, delegation)
	require.NoError(t, err)
	require.Equal(t, WorkspaceDispatchCommittedNow, result)
	child := workspaceTestBatch(workspace, "child-session", "child-run", "child-batch", "unknown-child-action", 0, "child-write")
	result, err = index.BeginDispatchBatch(ctx, child)
	require.NoError(t, err)
	require.Equal(t, WorkspaceDispatchCommittedNow, result, "the exact parent orchestration marker cannot block its child's own journaled effect")
	// Neither the parent marker nor a new independent Index instance may hide
	// the child's unresolved effect after a crash/restart.
	reopened := newWorkspaceQuarantineIndex(index.root, workspaceIndexIO{})
	newCall := workspaceTestBatch(workspace, "other-session", "other-run", "other-batch", "other-action", 0, "other-write")
	_, err = reopened.BeginDispatchBatch(ctx, newCall)
	require.ErrorIs(t, err, ErrWorkspaceQuarantined)
	_, err = reopened.BeginDispatchBatch(ctx, preparedDelegationBatch(t, workspace, root, "another-parent", true))
	require.ErrorIs(t, err, ErrWorkspaceQuarantined, "a fresh delegation is not an admission exemption beside an unrelated unknown")
}

func TestGenericSameNameDelegationRemainsWorkspaceBlocker(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workspace, err := WorkspaceIDForCanonicalRoot(root)
	require.NoError(t, err)
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	require.NoError(t, index.EnsureWorkspace(ctx, workspace))
	_, err = index.BeginDispatchBatch(ctx, preparedDelegationBatch(t, workspace, root, "generic", false))
	require.NoError(t, err)
	_, err = index.BeginDispatchBatch(ctx, workspaceTestBatch(workspace, "child", "run", "batch", "action", 0, "write"))
	require.ErrorIs(t, err, ErrWorkspaceQuarantined)
	forged := workspaceTestBatch(workspace, "forged", "run2", "batch2", "action2", 0, "forged")
	forged.Actions[0].NonBlockingDelegation = true
	_, err = index.BeginDispatchBatch(ctx, forged)
	require.Error(t, err, "a durable label cannot be used as runtime delegation authority")
}
