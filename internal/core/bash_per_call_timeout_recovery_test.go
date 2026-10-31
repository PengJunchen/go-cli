//go:build darwin || linux

package core_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestRunCommitBashPerCallTimeoutContinuesWithoutReplay(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	const workspaceID = "workspace-per-call-bash-timeout"
	_, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "per-call own timeout recovery")
	defer prepared.Close()
	check := filepath.Join(root, "seeded.txt")
	require.NoError(t, os.WriteFile(check, []byte("independent seeded value\n"), 0o600))
	parent, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	require.NoError(t, tools.RegisterDefaults(parent, base, tools.WithRegisteredBuiltinWhitelist([]string{"bash", "read"}), tools.WithRegisteredBashSandbox(tools.AllowAllSandbox{}), tools.WithRegisteredBashTimeout(5*time.Second)))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, r tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: r.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("per-call-timeout", "failed then read",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "shorter-bash", Name: "bash", Args: map[string]any{"command": "exec sleep 10", "timeout_ms": 2000}}}},
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "read-after-shorter", Name: "read", Args: map[string]any{"path": check}}}},
		mock.ConversationTurn{AssistantContent: "done after managed per-call timeout"},
	))
	agent := core.NewAgentImpl("per-call-timeout", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	stream, err := core.NewHarnessImpl(agent).Submit(core.WithRunCommit(parent, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID}), "run bounded action then inspect seed")
	require.NoError(t, err)
	for range stream.Events() {
	}
	result, err := stream.Result()
	require.NoError(t, err)
	require.NoError(t, parent.Err())
	require.Equal(t, "done after managed per-call timeout", result.Content)
	require.Equal(t, 3, model.CallCount(), "one failed Bash, one Read, final response; no automatic replay")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var receipt, proof, resolution session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.ToolCallID == "shorter-bash" && row.ReceiptOutcome != "" {
			receipt = row
		}
		if row.ToolCallID == "shorter-bash" && row.CompletionProof == tools.CompletionOwnedProcessGroupJoined {
			proof = row
		}
	}
	for _, row := range snapshot.DurableEntries {
		if row.ExecutionResolution != nil && row.ActionID == proof.ActionID {
			resolution = row
		}
	}
	require.NotEmpty(t, proof.ID)
	require.Equal(t, core.ReceiptOutcomeError, receipt.ReceiptOutcome)
	require.Contains(t, receipt.Content, "timed out after 2s")
	require.NotEmpty(t, resolution.ID)
	require.Equal(t, proof.ID, resolution.ExecutionResolution.EvidenceRef)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
	data, err := os.ReadFile(check)
	require.NoError(t, err)
	require.Equal(t, "independent seeded value\n", string(data))
}
