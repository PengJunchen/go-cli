package core_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestHostAndPosthookQuarantineDenialAreKnown(t *testing.T) {
	for _, hook := range []bool{false, true} {
		t.Run(map[bool]string{false: "host-write", true: "read-then-write-posthook"}[hook], func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, ".config"))
			canonical, err := filepath.EvalSymlinks(root)
			require.NoError(t, err)
			workspace, err := session.WorkspaceIDForCanonicalRoot(canonical)
			require.NoError(t, err)
			index, prepared, sink := prepareRunCommitSession(t, root, workspace, "independent host denial")
			defer func() { require.NoError(t, prepared.Close()) }()
			prior := session.PendingActionRef{SessionID: "prior-session", WorkspaceID: workspace, RunID: "prior-run", ActionBatchID: "prior-batch", ToolCallID: "prior-call", ActionID: "prior-action"}
			identity := &tools.TrustedToolIdentitySnapshot{RegistrationSource: "builtin", ToolName: "write", RegistrationGeneration: 1, ExecutorFingerprint: strings.Repeat("c", 64), ScopeFingerprint: strings.Repeat("d", 64)}
			_, err = index.BeginDispatchBatch(t.Context(), session.WorkspaceDispatchBatch{WorkspaceID: workspace, SessionID: prior.SessionID, RunID: prior.RunID, ActionBatchID: prior.ActionBatchID, Actions: []session.WorkspaceDispatchAction{{Action: prior, ToolIdentity: identity, EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceWrite}}}})
			require.NoError(t, err)
			registry := tools.NewMiddlewareToolRegistry(tools.NewDefaultToolRegistry())
			require.NoError(t, tools.RegisterDefaults(t.Context(), registry, tools.WithRegisteredCanonicalWorkspaceRoot(canonical), tools.WithRegisteredBuiltinWhitelist([]string{"read", "write"})))
			require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, req tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
				return tools.PreparedPolicyDecision{Allowed: true, Generations: req.Generations}, nil
			}))
			queue, ok := tools.NewDefaultFileMutationQueue().(*tools.DefaultFileMutationQueue)
			require.True(t, ok)
			defer func() { require.NoError(t, queue.Close()) }()
			require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{MutationQueue: queue}))
			source := core.NewLoopAgent(core.WithTools(registry))
			marker := filepath.Join(canonical, "forbidden-write")
			call := tools.ToolCall{Name: "write", Args: map[string]any{"path": marker, "content": "forbidden"}}
			if hook {
				readpath := filepath.Join(canonical, "existing.txt")
				require.NoError(t, os.WriteFile(readpath, []byte("source-read-completed"), 0600))
				call = tools.ToolCall{Name: "read", Args: map[string]any{"path": readpath}}
				source.SetPostToolActions(func(_ llm.ToolCall, _ *tools.ToolResult) ([]core.PostToolAction, error) {
					return []core.PostToolAction{{Call: llm.ToolCall{Name: "write", Args: map[string]any{"path": marker, "content": "forbidden-hook"}}}}, nil
				})
			}
			runner := core.NewEinoTurnRunner(source)
			runner.SetRunSlotGuard(core.NewDefaultRunSlotGuard())
			ctx := core.WithRunCommit(t.Context(), sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspace, CanonicalWorkspaceRoot: canonical})
			result, err := runner.RunHostToolAction(ctx, source, core.HostToolActionRequest{Description: "independent known denial", Call: call})
			require.ErrorContains(t, err, "quarantined")
			require.Equal(t, core.RunOutcomeError, result.Outcome)
			require.Equal(t, core.RunExecutionQuiescent, result.ExecutionState)
			require.Equal(t, core.DurablePublicationSynced, result.DurablePublication.State)
			snap, err := prepared.Snapshot()
			require.NoError(t, err)
			pending, err := session.InspectPendingActions(snap)
			require.NoError(t, err)
			require.Empty(t, pending)
			noio, success := 0, 0
			for _, row := range snap.DurableEntries {
				if row.CheckpointKind == session.CheckpointExecutionResolution && row.ExecutionResolution.Evidence == core.ResolutionNeverDispatched {
					noio++
				}
				if row.Type == session.EntryTypeTool && row.ReceiptOutcome == core.ReceiptOutcomeSuccess {
					success++
				}
			}
			require.Equal(t, 1, noio)
			if hook {
				require.Equal(t, 1, success)
			} else {
				require.Zero(t, success)
			}
			_, statErr := os.Stat(marker)
			require.True(t, os.IsNotExist(statErr))
			decisions, err := index.InspectDispatchMarkers(t.Context(), []session.PendingActionRef{prior})
			require.NoError(t, err)
			require.Equal(t, session.DispatchMarkerPresent, decisions[0].MarkerState)
		})
	}
}
