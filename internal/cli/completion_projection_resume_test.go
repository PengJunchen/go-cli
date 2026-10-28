package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestInteractiveResumeProjectsCompletedMCPWorkerCustomAndStateReceipts(t *testing.T) {
	workerConfig, workerCalls, _ := durableWorkerConfig(t, "normal")
	peer := newMCPGenerationPeer(t, []string{"lookup"}, "completed MCP response", false, false, false)
	cfg := newMCPListTestConfig(t, []config.MCPServerConfig{{Name: "catalog", URL: peer.server.URL}})
	cfg.Extensions = workerConfig.Extensions
	cfg.Session.StorePath = filepath.Join(t.TempDir(), "sessions")
	customMarker := filepath.Join(t.TempDir(), "custom-calls")
	cfg.Tools.CustomTools = []config.CustomToolConfig{{Name: "resume_custom", Command: []string{"/bin/sh", "-c", "printf called >> '" + customMarker + "'; printf completed-custom"}, WorkingDir: cfg.Git.WorkDir}}
	type action struct {
		name string
		args map[string]any
	}
	actions := []action{
		{"mcp_discover", map[string]any{"server": "catalog"}},
		{"mcp__catalog__lookup", map[string]any{}},
		{"worker_echo", map[string]any{}},
		{"resume_custom", map[string]any{}},
		{"task_create", map[string]any{"title": "completed task receipt"}},
		{"goal_create", map[string]any{"title": "completed goal receipt"}},
		{"todo_write", map[string]any{"action": "add", "content": "completed todo receipt"}},
	}
	responses := make([]string, 0, len(actions)+2)
	for index, action := range actions {
		responses = append(responses, durableApprovalToolResponse(t, fmt.Sprintf("projection-call-%d", index), action.name, action.args))
	}
	responses = append(responses,
		`{"choices":[{"message":{"role":"assistant","content":"all receipt types completed"}}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"interactive continuation completed"}}]}`)
	provider, script := newDurablePromptServer(t, responses...)
	defer provider.Close()
	cfg.Provider.BaseURL = provider.URL
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	var out bytes.Buffer
	require.NoError(t, newPromptCmd(&out).Run(ctx, cfg, []string{"--approve", "auto", "complete every receipt type"}))
	physical := physicalSessionIDFromOutput(t, out.String())
	readback, err := newDurableCLISession(ctx, cfg.Session.StorePath, "inspect before interactive", physical, "")
	require.NoError(t, err)
	snapshot, err := readback.snapshotSelected()
	require.NoError(t, err)
	proofs := map[string]tools.PreparedCompletionProof{}
	for _, row := range snapshot.DurableEntries {
		if row.ActionState == session.ActionStateExecution && row.CompletionProof != "" {
			proofs[row.ToolName] = row.CompletionProof
		}
	}
	for _, action := range actions {
		require.NotEmpty(t, proofs[action.name], "fixture must persist actual completed proof for %s", action.name)
	}
	require.Equal(t, tools.CompletionMCPRequestJoined, proofs["mcp_discover"])
	require.Equal(t, tools.CompletionMCPRequestJoined, proofs["mcp__catalog__lookup"])
	require.Equal(t, tools.CompletionWorkerRequestJoined, proofs["worker_echo"])
	require.Equal(t, tools.CompletionInlinePipelineJoined, proofs["resume_custom"])
	require.NoError(t, readback.requestClose(ctx))
	// Use the public interactive entrypoint after the prompt's Assembly and
	// owner have closed. Both full/headless builds consume this same test.
	out.Reset()
	command := newInteractiveCmd(nil, &out)
	command.lineEditor = &scriptedLineEditor{results: []lineReadResult{{line: "continue completed history"}, {line: "exit"}}}
	require.NoError(t, command.Run(ctx, cfg, []string{"--approve", "auto", "--resume-session", physical, "--no-onboarding"}))
	require.Contains(t, out.String(), "interactive continuation completed")
	require.NotContains(t, out.String(), "invalid completion proof")
	require.Len(t, script.requestBodies(), len(actions)+2)
	customData, err := os.ReadFile(customMarker)
	require.NoError(t, err)
	require.Equal(t, "called", string(customData), "history projection must not replay custom effects")
	workerData, err := os.ReadFile(workerCalls)
	require.NoError(t, err)
	require.Equal(t, "call\n", string(workerData), "history projection must not replay worker effects")
	require.EqualValues(t, 1, peer.calls.Load(), "history projection must not replay remote effects")
	latest, err := newDurableCLISession(ctx, cfg.Session.StorePath, "inspect after interactive", physical, "")
	require.NoError(t, err)
	defer func() { require.NoError(t, latest.requestClose(context.Background())) }()
	after, err := latest.snapshotSelected()
	require.NoError(t, err)
	require.Equal(t, physical, after.SessionID)
	pending, err := session.InspectPendingActions(after)
	require.NoError(t, err)
	require.Empty(t, pending)
}
