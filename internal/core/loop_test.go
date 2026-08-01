package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// findKind returns the contents of all events whose Kind equals k.
func findEvents(events []AgentEvent, kind string) []string {
	var out []string
	for _, ev := range events {
		if ev.Kind == kind {
			out = append(out, ev.Content)
		}
	}
	return out
}

func hasToolMessage(msgs []llm.Message, callID string) bool {
	for _, m := range msgs {
		if m.Role == llm.RoleTool && m.ToolCallID == callID {
			return true
		}
	}
	return false
}

func TestNewLoopAgentDefaults(t *testing.T) {
	loop := NewLoopAgent()
	require.NotNil(t, loop)
	assert.Equal(t, defaultMaxIterations, loop.maxIterations)
}

func TestLoopSingleTurnNoTools(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-01", "single",
		mock.ConversationTurn{AssistantContent: "hello from model"},
	))
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	assert.Equal(t, 1, model.CallCount())

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "hello from model", messages[0])
}

func TestLoopMultiTurnWithToolCall(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("file contents")
	require.NoError(t, err)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-02", "multiturn",
		mock.ConversationTurn{
			AssistantContent: "let me read the file",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call1", Name: "read_file", Args: map[string]any{"path": "a.go"}},
			},
		},
		mock.ConversationTurn{AssistantContent: "final answer"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "read a.go"})
	require.NoError(t, err)
	assert.Equal(t, 2, model.CallCount())

	// A tool_call event was emitted naming the tool.
	toolCalls := findEvents(events, "tool_call")
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "read_file", toolCalls[0])

	// A tool_result event was fed back.
	toolResults := findEvents(events, "tool_result")
	require.Len(t, toolResults, 1)

	// The final message event is present.
	messages := findEvents(events, "message")
	require.Len(t, messages, 2)
	assert.Equal(t, "final answer", messages[1])
}

func TestLoopToolResultFedBack(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("file contents")
	require.NoError(t, err)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-03", "feed-back",
		mock.ConversationTurn{
			AssistantContent: "calling",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-x", Name: "read_file", Args: nil},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	_, err = loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	// The second Generate call must have received the tool-role message
	// carrying the matching tool call id, proving the result was fed back.
	require.Equal(t, 2, model.CallCount())
	secondCall := model.CallLog()[1].Messages
	assert.True(t, hasToolMessage(secondCall, "call-x"))
}

func TestLoopMaxIterationsGuardsRunaway(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("x")
	require.NoError(t, err)

	turns := make([]mock.ConversationTurn, 0, 2)
	for i := 0; i < 2; i++ {
		turns = append(turns, mock.ConversationTurn{
			AssistantContent: "again",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c", Name: "read_file"},
			},
		})
	}

	model := mock.NewMockLLMServer(mock.NewConversationTemplate("L-04", "runaway", turns...))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv), WithMaxIterations(2))

	events, err := loop.Run(context.Background(), Submission{Content: "loop"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errMaxIterations)

	errs := findEvents(events, "error")
	assert.NotEmpty(t, errs)
}

func TestLoopContextCancellation(t *testing.T) {
	model := mock.NewMockLLMServer(nil)
	loop := NewLoopAgent(WithLLM(model))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events, err := loop.Run(ctx, Submission{Content: "hi"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))

	errs := findEvents(events, "error")
	assert.NotEmpty(t, errs)
}

func TestLoopNilModel(t *testing.T) {
	loop := NewLoopAgent()
	_, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errNilModel)
}

func TestLoopToolExecutionError(t *testing.T) {
	toolSrv := mock.NewMockToolServer()
	// No tool registered under the requested name -> Get fails.

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-05", "tool-error",
		mock.ConversationTurn{
			AssistantContent: "calling",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c1", Name: "missing_tool"},
			},
		},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.Error(t, err)
	errs := findEvents(events, "error")
	assert.NotEmpty(t, errs)
}
