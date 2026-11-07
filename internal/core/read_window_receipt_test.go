package core_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
)

func TestReadWindowPublishesTransientSourceTruncated(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	workspaceID := "workspace-window-source"
	_, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "windowed read")
	defer prepared.Close()
	path := filepath.Join(root, "fixture.txt")
	require.NoError(t, os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600))
	registry := newRunCommitStreamingRegistry(context.Background(), t, nil)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("window-read", "window read",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{ID: "window-live", Name: "read", Args: map[string]any{"path": path, "offset": 2, "limit": 1}}}},
		mock.ConversationTurn{AssistantContent: "done"},
	))
	agent := core.NewAgentImpl("window-live", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry)))
	harness := core.NewHarnessImpl(agent)
	stream, err := harness.Submit(core.WithRunCommit(context.Background(), sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID}), "read the middle")
	require.NoError(t, err)
	events := <-collectAgentEvents(stream)
	result, err := stream.Result()
	require.NoError(t, err)
	require.Equal(t, "done", result.Content)
	var receipt *core.AgentEvent
	for i := range events {
		if events[i].Kind == "tool_result" && events[i].ToolCallID == "window-live" {
			receipt = &events[i]
		}
	}
	require.NotNil(t, receipt)
	require.Equal(t, core.ReceiptOutcomeSuccess, receipt.ReceiptOutcome)
	require.NotNil(t, receipt.SourceTruncated)
	require.True(t, *receipt.SourceTruncated)
	require.Contains(t, receipt.Content, "second\n")
	require.NotContains(t, receipt.Content, "first\n")
}
