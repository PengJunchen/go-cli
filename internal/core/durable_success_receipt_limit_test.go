package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/session"
)

const durableReceiptNotice = "\n[tool output truncated for durable receipt]"

func TestDurableSuccessfulToolReceiptFitsAndRemainsConsistent(t *testing.T) {
	tests := []struct {
		name            string
		input           string
		parallel        bool
		bash            bool
		wantClipped     bool
		wantSourceProof bool
	}{
		{name: "small read unchanged", input: "small <value>\n中文", wantClipped: false},
		{name: "small invalid UTF-8", input: "prefix\xff\xfemiddle", wantClipped: false},
		{name: "maximum ASCII read", input: strings.Repeat("x", 1<<20), wantClipped: true},
		{name: "escaped Unicode parallel read", input: strings.Repeat("<&\"\\\x01中\u2028", 60000), parallel: true, wantClipped: true},
		{name: "source truncated Bash", bash: true, wantClipped: true, wantSourceProof: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			const workspaceID = "workspace-large-success-receipt"
			index, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "synthetic successful output")
			preparedOpen := true
			t.Cleanup(func() {
				if preparedOpen {
					_ = prepared.Close()
				}
			})
			inputPath := filepath.Join(root, "input.txt")
			require.NoError(t, os.WriteFile(inputPath, []byte(test.input), 0o600))
			calls := []mock.ExpectedToolCall{{ID: "large-output", Name: "read", Args: map[string]any{"path": inputPath}}}
			if test.bash {
				calls[0] = mock.ExpectedToolCall{ID: "large-output", Name: "bash", Args: map[string]any{"command": "for ((i=0;i<1100;i++)); do printf '%01024d\\n' 0; done"}}
			}
			if test.parallel {
				peerPath := filepath.Join(root, "peer.txt")
				require.NoError(t, os.WriteFile(peerPath, []byte("peer unchanged"), 0o600))
				calls = append(calls, mock.ExpectedToolCall{ID: "peer-output", Name: "read", Args: map[string]any{"path": peerPath}})
			}
			var dispatches atomic.Int32
			registry := newRunCommitStreamingRegistry(context.Background(), t, &dispatches)
			model := mock.NewMockLLMServer(mock.NewConversationTemplate("successful-receipt", "successful receipt",
				mock.ConversationTurn{AssistantToolCalls: calls},
				mock.ConversationTurn{AssistantContent: "done"},
			))
			options := []core.LoopOption{core.WithLLM(model), core.WithTools(registry)}
			if test.parallel {
				options = append(options, core.WithExecutionMode(core.ExecutionModeParallel))
			}
			harness := core.NewHarnessImpl(core.NewAgentImpl("receipt-limit", core.NewLoopAgent(options...)))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID}), "inspect synthetic output")
			require.NoError(t, err)
			events := <-collectAgentEvents(stream)
			result, runErr := stream.Result()
			require.NoError(t, runErr, "a successful tool output must fit its durable receipt")
			require.Equal(t, "done", result.Content)
			require.Equal(t, int32(len(calls)), dispatches.Load(), "each tool dispatches once")
			require.Equal(t, 2, model.CallCount(), "one tool batch and one final response")

			var event *core.AgentEvent
			for i := range events {
				if events[i].Kind == "tool_result" && events[i].ToolCallID == "large-output" {
					require.Nil(t, event, "a tool call has one final receipt")
					event = &events[i]
				}
			}
			require.NotNil(t, event)
			require.Equal(t, core.ReceiptOutcomeSuccess, event.ReceiptOutcome)
			require.Equal(t, test.wantClipped, event.ReceiptOutputTruncated)
			if test.wantSourceProof {
				require.NotNil(t, event.SourceTruncated)
				require.True(t, *event.SourceTruncated)
			} else {
				require.Nil(t, event.SourceTruncated)
			}
			if test.wantClipped {
				require.True(t, utf8.ValidString(event.Content))
				require.Contains(t, event.Content, durableReceiptNotice)
				if !test.bash {
					require.True(t, strings.HasPrefix(test.input, strings.TrimSuffix(event.Content, durableReceiptNotice)))
				} else {
					require.True(t, strings.HasPrefix(event.Content, strings.Repeat("0", 1024)))
				}
			} else {
				require.Equal(t, string([]rune(test.input)), event.Content)
			}
			if test.name == "maximum ASCII read" {
				require.Greater(t, len(event.Content), 900000, "ordinary output should not be gratuitously reduced")
			}
			var providerOutput string
			for _, message := range model.CallLog()[1].Messages {
				if message.ToolCallID == "large-output" {
					providerOutput = message.Content
				}
			}
			require.Equal(t, event.Content, providerOutput)
			var transcriptOutput string
			for _, message := range prepared.View().Messages {
				if message.ToolCallID == "large-output" {
					transcriptOutput = message.Content
				}
			}
			require.Equal(t, event.Content, transcriptOutput)

			snapshot, err := prepared.Snapshot()
			require.NoError(t, err)
			var receipt session.SessionEntry
			var proof session.SessionEntry
			for _, row := range snapshot.DurableEntries {
				if row.ToolCallID == "large-output" && row.ReceiptOutcome != "" {
					receipt = row
				}
				if row.ToolCallID == "large-output" && row.CompletionProof != "" {
					proof = row
				}
			}
			require.NotEmpty(t, receipt.ID)
			require.NotEmpty(t, proof.CompletionProof)
			require.Equal(t, core.ReceiptOutcomeSuccess, receipt.ReceiptOutcome)
			require.Equal(t, event.Content, receipt.Content)
			encoded, err := json.Marshal(receipt)
			require.NoError(t, err)
			require.LessOrEqual(t, len(encoded), 1<<20)
			pending, err := session.InspectPendingActions(snapshot)
			require.NoError(t, err)
			require.Empty(t, pending)
			id := prepared.View().SessionID
			require.NoError(t, prepared.Close())
			preparedOpen = false
			reopened, err := session.NewDurableJSONLSessionPreparer(filepath.Join(root, "durable"), index).PrepareResume(context.Background(), id)
			require.NoError(t, err)
			defer reopened.Close()
			reopenedSnapshot, err := reopened.Snapshot()
			require.NoError(t, err)
			found := false
			for _, row := range reopenedSnapshot.DurableEntries {
				if row.ID == receipt.ID {
					require.Equal(t, event.Content, row.Content)
					found = true
				}
			}
			require.True(t, found, "receipt survives reopening")
		})
	}
}

func TestDurableToolReceiptAccountsForLongCallID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	const workspaceID = "workspace-long-receipt-id"
	_, prepared, sink := prepareRunCommitSession(t, root, workspaceID, "long tool call identity")
	defer prepared.Close()
	inputPath := filepath.Join(root, "input.txt")
	require.NoError(t, os.WriteFile(inputPath, []byte(strings.Repeat("x", 1<<20)), 0o600))
	var dispatches atomic.Int32
	registry := newRunCommitStreamingRegistry(context.Background(), t, &dispatches)
	callID := strings.Repeat("i", 100000)
	model := mock.NewMockLLMServer(mock.NewConversationTemplate("long-id", "long id",
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{{
			ID: callID, Name: "read", Args: map[string]any{"path": inputPath},
		}}},
		mock.ConversationTurn{AssistantContent: "done"},
	))
	harness := core.NewHarnessImpl(core.NewAgentImpl("long-id", core.NewLoopAgent(core.WithLLM(model), core.WithTools(registry))))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := harness.Submit(core.WithRunCommit(ctx, sink, core.RunCommitBinding{SessionID: prepared.View().SessionID, WorkspaceID: workspaceID}), "read synthetic data")
	require.NoError(t, err)
	events := <-collectAgentEvents(stream)
	result, runErr := stream.Result()
	require.NoError(t, runErr)
	require.Equal(t, "done", result.Content)
	require.Equal(t, int32(1), dispatches.Load())
	require.Equal(t, 2, model.CallCount())
	var event *core.AgentEvent
	for i := range events {
		if events[i].Kind == "tool_result" && events[i].ToolCallID == callID {
			event = &events[i]
		}
	}
	require.NotNil(t, event)
	require.True(t, event.ReceiptOutputTruncated)
	require.Contains(t, event.Content, durableReceiptNotice)
	require.Greater(t, len(event.Content), 800000)
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	var receipt session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.ToolCallID == callID && row.ReceiptOutcome != "" {
			receipt = row
		}
	}
	require.NotEmpty(t, receipt.ID)
	require.Equal(t, event.Content, receipt.Content)
	encoded, err := json.Marshal(receipt)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), 1<<20)
	pending, err := session.InspectPendingActions(snapshot)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestDurableWriterStillRejectsOversizedNonContentFields(t *testing.T) {
	prepared, err := session.NewDurableJSONLSessionPreparer(t.TempDir(), nil).PrepareNew(context.Background(), "oversized non-content field")
	require.NoError(t, err)
	defer prepared.Close()
	index := uint64(0)
	row := session.SessionEntry{
		ID: "synthetic-entry", Type: session.EntryTypeUser, Content: "small",
		RunID: strings.Repeat("r", 1<<20), TranscriptIndex: &index,
	}
	_, err = prepared.DurableWriter().AppendAndSync(context.Background(), row)
	require.ErrorContains(t, err, "serialized entry exceeds 1048576 bytes")
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.Empty(t, snapshot.DurableEntries)
}
