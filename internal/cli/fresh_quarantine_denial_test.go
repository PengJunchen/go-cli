package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestPromptKnownQuarantineDenialClosesFreshSession(t *testing.T) {
	for _, test := range []struct {
		name     string
		parallel bool
		count    int
		known    bool
	}{{"single", false, 1, false}, {"sequential-suffix", false, 3, false}, {"parallel-all", true, 3, false}, {"parallel-mixed", true, 2, true}} {
		t.Run(test.name, func(t *testing.T) {
			configureDurableCLISessionTest(t)
			t.Chdir(t.TempDir())
			store := filepath.Join(t.TempDir(), "history")
			original, err := newDurableCLISession(t.Context(), store, "orphan marker fixture", "", "")
			require.NoError(t, err)
			old, err := original.snapshotSelected()
			require.NoError(t, err)
			ref := session.PendingActionRef{SessionID: old.SessionID, WorkspaceID: original.workspaceID, RunID: "original-run", ActionBatchID: "original-batch", ToolCallID: "original-call", ActionID: "original-action"}
			identity := &tools.TrustedToolIdentitySnapshot{RegistrationSource: "builtin", ToolName: "write", RegistrationGeneration: 1, ExecutorFingerprint: strings.Repeat("a", 64), ScopeFingerprint: strings.Repeat("b", 64)}
			_, err = original.index.BeginDispatchBatch(t.Context(), session.WorkspaceDispatchBatch{WorkspaceID: original.workspaceID, SessionID: old.SessionID, RunID: ref.RunID, ActionBatchID: ref.ActionBatchID, Actions: []session.WorkspaceDispatchAction{{Action: ref, ToolIdentity: identity, EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceWrite}}}})
			require.NoError(t, err)
			require.NoError(t, original.requestClose(t.Context()))
			var calls []any
			if test.known {
				calls = append(calls, map[string]any{"id": "known-denial", "type": "function", "function": map[string]any{"name": "not_registered", "arguments": "{}"}})
			}
			var paths []string
			for index := 0; index < test.count; index++ {
				path := filepath.Join(t.TempDir(), "must-not-write")
				paths = append(paths, path)
				args, err := json.Marshal(map[string]any{"path": path, "content": "forbidden"})
				require.NoError(t, err)
				calls = append(calls, map[string]any{"id": path, "type": "function", "function": map[string]any{"name": "write", "arguments": string(args)}})
			}
			response, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": calls}}}})
			require.NoError(t, err)
			server, script := newDurablePromptServer(t, string(response), `{"choices":[{"message":{"role":"assistant","content":"fresh exact resume succeeded"}}]}`)
			defer server.Close()
			cfg := durablePromptConfig(t, server.URL, store)
			cfg.Approval.Mode = "auto"
			cfg.Tools.Parallel = &test.parallel
			var out bytes.Buffer
			require.Error(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"attempt blocked writes"}))
			fresh := physicalSessionIDFromOutput(t, out.String())
			snapshot, err := session.InspectPhysicalSession(t.Context(), store, fresh)
			require.NoError(t, err)
			pending, err := session.InspectPendingActions(snapshot)
			require.NoError(t, err)
			require.Empty(t, pending, "a known default-index denial before marker append must close its own fresh action")
			receipts, resolutions := 0, 0
			for _, row := range snapshot.DurableEntries {
				if row.Type == session.EntryTypeTool && row.ActionState == "" {
					receipts++
					require.Equal(t, session.DispatchStateNotDispatched, row.DispatchState)
					require.NotEqual(t, core.ReceiptOutcomeUnknown, row.ReceiptOutcome)
				}
				if row.CheckpointKind == session.CheckpointExecutionResolution {
					resolutions++
					require.Equal(t, core.ResolutionNeverDispatched, row.ExecutionResolution.Evidence)
					require.NotEmpty(t, row.ExecutionResolution.EvidenceRef)
				}
			}
			require.Equal(t, len(calls), receipts)
			expectedReserved := test.count
			if !test.parallel {
				expectedReserved = 1
			}
			require.Equal(t, expectedReserved, resolutions)
			last := snapshot.DurableEntries[len(snapshot.DurableEntries)-1]
			require.Equal(t, session.CheckpointRunTerminal, last.CheckpointKind)
			require.Equal(t, core.RunOutcomeError, last.RunOutcome)
			require.NoError(t, newPromptCmd(&out).Run(t.Context(), cfg, []string{"--resume-session", fresh, "continue safely"}))
			require.Len(t, script.requestBodies(), 2, "new run can continue without replaying denied calls")
			for _, path := range paths {
				_, err := os.Stat(path)
				require.True(t, os.IsNotExist(err))
			}
			decisions, err := original.index.InspectDispatchMarkers(t.Context(), []session.PendingActionRef{ref})
			require.NoError(t, err)
			require.Equal(t, session.DispatchMarkerPresent, decisions[0].MarkerState, "another session's unresolved marker must remain quarantined")
		})
	}
}
