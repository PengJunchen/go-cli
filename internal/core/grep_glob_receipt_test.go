package core_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestRunCommitGrepInvalidGlobReceiptReachesNextModelRequest(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("PATH", t.TempDir())
	const workspaceID = "workspace-invalid-grep-glob"
	index, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "invalid grep glob")
	opened := true
	t.Cleanup(func() {
		if opened {
			_ = prepared.Close()
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	deferred := tools.NewDeferredToolRegistryAdapter(base)
	require.NoError(t, tools.RegisterDefaults(ctx, deferred,
		tools.WithRegisteredBuiltinWhitelist([]string{"grep"}),
		tools.WithRegisteredPathWhitelist([]string{root}),
	))
	registry := tools.NewMiddlewareToolRegistry(deferred)
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("invalid-glob", "invalid glob",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: "grep-invalid-glob", Name: "grep", Args: map[string]any{"pattern": "needle", "path": root, "glob": "["},
		}}},
		mock.ConversationTurn{AssistantContent: "filter needs correction"},
	))
	loop := core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry))
	harness := core.NewHarnessImpl(core.NewAgentImpl("glob-receipt", loop))
	runCtx := core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID})
	stream, err := harness.Submit(runCtx, "search the directory")
	require.NoError(t, err)
	<-collectAgentEvents(stream)
	_, err = stream.Result()
	require.NoError(t, err)

	requests := model.CallLog()
	require.Len(t, requests, 2, "one tool request is followed by one ordinary model request")
	var visible string
	for _, message := range requests[1].Messages {
		if message.Role == llm.RoleTool && message.ToolCallID == "grep-invalid-glob" {
			visible = message.Content
		}
	}
	require.Contains(t, visible, "invalid glob filter")
	require.Contains(t, visible, "Correct or remove the glob argument")

	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var receiptContent string
	for _, row := range snapshot.DurableEntries {
		if row.Type == session.EntryTypeTool && row.ToolCallID == "grep-invalid-glob" && row.ReceiptOutcome != "" {
			require.Equal(t, core.ReceiptOutcomeError, row.ReceiptOutcome)
			receiptContent = row.Content
		}
	}
	require.Equal(t, visible, receiptContent)
	sessionID := prepared.View().SessionID
	require.NoError(t, prepared.Close())
	opened = false
	reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(ctx, sessionID)
	require.NoError(t, err)
	defer reopened.Close()
	restored, err := reopened.Snapshot()
	require.NoError(t, err)
	var restoredReceipt string
	for _, row := range restored.DurableEntries {
		if row.Type == session.EntryTypeTool && row.ToolCallID == "grep-invalid-glob" && row.ReceiptOutcome != "" {
			require.Equal(t, core.ReceiptOutcomeError, row.ReceiptOutcome)
			restoredReceipt = row.Content
		}
	}
	require.Equal(t, visible, restoredReceipt)
}
