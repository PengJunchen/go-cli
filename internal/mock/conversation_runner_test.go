package mock

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestConversationRunnerMultiTurnToolFlow(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("S-01", "multi-turn",
		ConversationTurn{AssistantToolCalls: []ExpectedToolCall{
			{ID: "c1", Name: "read_file", Args: map[string]any{"path": "/main.go"}},
		}},
		ConversationTurn{AssistantContent: "task complete"},
	))

	toolServer := NewMockToolServer()
	_, err := toolServer.RegisterReadFileTool("package main")
	require.NoError(t, err)

	traceExporter := NewMockTraceExporter()
	runner := NewConversationRunner(server, toolServer, traceExporter)

	err = runner.Run(context.Background(), []string{"read the file and summarize"})
	require.NoError(t, err)

	// Tool was invoked and the final model response has no tool calls: one
	// call for the tool round-trip and one for the final response.
	assert.Equal(t, 2, server.CallCount())
	runner.AssertToolCalled(t, "read_file", 1)
	runner.AssertNoLLMError(t)
	runner.AssertTraceComplete(t)

	last := server.CallLog()[len(server.CallLog())-1].Response
	assert.NotNil(t, last)
	assert.Empty(t, last.ToolCalls)
}

func TestConversationRunnerMultipleUserMessages(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("S-02", "two-round",
		ConversationTurn{AssistantToolCalls: []ExpectedToolCall{
			{ID: "c1", Name: "bash", Args: map[string]any{"command": "pwd"}},
		}},
		ConversationTurn{AssistantContent: "round one done"},
		ConversationTurn{AssistantContent: "round two done"},
	))

	toolServer := NewMockToolServer()
	_, err := toolServer.RegisterBashTool("PASS", 0)
	require.NoError(t, err)

	runner := NewConversationRunner(server, toolServer, NewMockTraceExporter())
	err = runner.Run(context.Background(), []string{"run a command", "and then finish"})
	require.NoError(t, err)

	assert.Equal(t, 3, server.CallCount())
	runner.AssertToolCalled(t, "bash", 1)
	runner.AssertNoLLMError(t)
}

func TestConversationRunnerWithoutTracerSkipsTrace(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("S-03", "no-trace",
		ConversationTurn{AssistantContent: "ok"},
	))
	toolServer := NewMockToolServer()

	runner := NewConversationRunner(server, toolServer, nil)
	err := runner.Run(context.Background(), []string{"hi"})
	require.NoError(t, err)
	runner.AssertNoLLMError(t)

	// Trace assertion is skipped when no exporter is configured.
	runner.AssertTraceComplete(t)
}

func TestConversationRunnerCreatesExpectedSpans(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("S-04", "spans",
		ConversationTurn{AssistantContent: "first"},
		ConversationTurn{AssistantContent: "second"},
	))

	toolServer := NewMockToolServer()
	traceExporter := NewMockTraceExporter()
	runner := NewConversationRunner(server, toolServer, traceExporter)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, runner.Run(ctx, []string{"a", "b"}))

	// Run settles spans before returning; verify both names present.
	traceExporter.AssertSpanExists(t, "cli.invocation")
	traceExporter.AssertSpanExists(t, "llm.request")
	traceExporter.AssertSpanChain(t)

	// Expect one invocation span + one llm.request per Generate call.
	assert.Equal(t, 1+server.CallCount(), traceExporter.SpanCount())
}

func TestConversationRunnerToolErrorPropagates(t *testing.T) {
	server := NewMockLLMServer(NewConversationTemplate("S-05", "tool-error",
		ConversationTurn{AssistantToolCalls: []ExpectedToolCall{
			{ID: "c1", Name: "boom", Args: map[string]any{}},
		}},
	))

	toolServer := NewMockToolServer()
	_, err := toolServer.RegisterMockTool("boom", func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		return nil, errors.New("boom handler failed")
	})
	require.NoError(t, err)

	runner := NewConversationRunner(server, toolServer, nil)
	err = runner.Run(context.Background(), []string{"trigger"})
	require.Error(t, err)
}
