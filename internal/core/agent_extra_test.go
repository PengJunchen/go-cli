package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// returningNilEventsLoop returns no events and no error.
type returningNilEventsLoop struct{}

func (returningNilEventsLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return nil, nil
}

func TestAgentRunWithNilEventSlice(t *testing.T) {
	agent := NewAgentImpl("nil-events", returningNilEventsLoop{})

	res, err := agent.Run(context.Background(), Submission{Content: "hello"})
	require.NoError(t, err)
	assert.True(t, res.Success)
	assert.Empty(t, res.Message)

	// History holds only the user message (no assistant message since the
	// final message was empty).
	msgs := agent.Messages()
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0].Role)

	// Events is an empty non-nil slice.
	assert.Empty(t, agent.Events())
}

// errorAfterUserLoop fails Run after the user message is recorded.
type errorAfterUserLoop struct{}

func (errorAfterUserLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return nil, errMaxIterations
}

func TestAgentRunErrorStillRecordsUserMessage(t *testing.T) {
	agent := NewAgentImpl("err", errorAfterUserLoop{})
	res, err := agent.Run(context.Background(), Submission{Content: "boom"})
	require.ErrorIs(t, err, errMaxIterations)
	assert.False(t, res.Success)

	// Even on error, the user message was appended and no assistant message.
	msgs := agent.Messages()
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "boom", msgs[0].Content)
}

func TestLastMessageEventReturnsTrailingEmpty(t *testing.T) {
	events := []AgentEvent{
		{Kind: "message", Content: "first"},
		{Kind: "message", Content: ""},
		{Kind: "status", Content: "x"},
	}
	// Trailing empty message overwrites first (matches by Kind only, so
	// pure tool call responses with empty Content are returned).
	assert.Equal(t, "", lastMessageEvent(events))
}

func TestLastMessageEventIgnoresNonMessage(t *testing.T) {
	events := []AgentEvent{
		{Kind: "tool", Content: "data"},
		{Kind: "done", Content: "done"},
	}
	assert.Equal(t, "", lastMessageEvent(events))
}

func TestLastMessageEventEmpty(t *testing.T) {
	assert.Equal(t, "", lastMessageEvent(nil))
	assert.Equal(t, "", lastMessageEvent([]AgentEvent{}))
}

func TestAgentEventsAndMessagesReturnCopies(t *testing.T) {
	agent := NewAgentImpl("copy", returningNilEventsLoop{})
	_, err := agent.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	// Mutating the returned history copy must not affect the agent.
	hist := agent.Messages()
	hist[0].Content = "mutated"
	assert.Equal(t, "go", agent.Messages()[0].Content)

	evs := agent.Events()
	assert.Empty(t, evs)
}

func TestAgentWithHistoryPreservesOrder(t *testing.T) {
	agent := NewAgentImpl("hist", returningNilEventsLoop{}, WithHistory([]AgentMessage{
		{Role: "system", Content: "ping"},
		{Role: "assistant", Content: "pong"},
	}))
	require.Len(t, agent.Messages(), 2)
	assert.Equal(t, "system", agent.Messages()[0].Role)
	assert.Equal(t, "assistant", agent.Messages()[1].Role)
}

func TestAgentWithMultipleHistoryOptions(t *testing.T) {
	agent := NewAgentImpl("multi",
		returningNilEventsLoop{},
		WithHistory([]AgentMessage{{Role: "system", Content: "a"}}),
		WithHistory([]AgentMessage{{Role: "user", Content: "b"}}),
	)
	require.Len(t, agent.Messages(), 2)
	assert.Equal(t, "a", agent.Messages()[0].Content)
	assert.Equal(t, "b", agent.Messages()[1].Content)
}

// --- Task 35-2 and 35-1 test cases ---

// pureToolCallEventLoop returns a single message event with empty Content and
// non-empty ToolCalls, simulating a pure tool call response.
type pureToolCallEventLoop struct{}

func (pureToolCallEventLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return []AgentEvent{
		{Kind: "message", Content: "", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
	}, nil
}

// textAndToolCallEventLoop returns a single message event with both Content
// and ToolCalls, simulating a mixed text + tool call response.
type textAndToolCallEventLoop struct{}

func (textAndToolCallEventLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return []AgentEvent{
		{Kind: "message", Content: "let me read", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "read_file"}}},
	}, nil
}

// usageEventLoop returns a single message event carrying Usage data.
type usageEventLoop struct{}

func (usageEventLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return []AgentEvent{
		{Kind: "message", Content: "hello", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}},
	}, nil
}

// TestLastMessageEventReturnsEmptyContent verifies that lastMessageEvent
// returns the empty content of a pure tool call message event (matches by
// Kind only, not by Content non-empty).
func TestLastMessageEventReturnsEmptyContent(t *testing.T) {
	events := []AgentEvent{
		{Kind: "message", Content: "", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "tool"}}},
	}
	assert.Equal(t, "", lastMessageEvent(events))
}

// TestHistoryIncludesPureToolCallTurn verifies that a pure tool call response
// (empty Content, non-empty ToolCalls) enters the agent's history.
func TestHistoryIncludesPureToolCallTurn(t *testing.T) {
	agent := NewAgentImpl("test", pureToolCallEventLoop{})
	_, err := agent.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	msgs := agent.Messages()
	require.Len(t, msgs, 2) // user + assistant
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Empty(t, msgs[1].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read_file", msgs[1].ToolCalls[0].Name)
}

// TestAgentHistorySavesTextAndToolCalls verifies that an assistant response
// with both text and tool calls saves both Content and ToolCalls to history.
func TestAgentHistorySavesTextAndToolCalls(t *testing.T) {
	agent := NewAgentImpl("test", textAndToolCallEventLoop{})
	res, err := agent.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	assert.Equal(t, "let me read", res.Message)

	msgs := agent.Messages()
	require.Len(t, msgs, 2) // user + assistant
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "let me read", msgs[1].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "read_file", msgs[1].ToolCalls[0].Name)
}

// TestAgentHistoryPureToolCallSaved verifies that a pure tool call response
// (no text content) is saved to history with its ToolCalls intact.
func TestAgentHistoryPureToolCallSaved(t *testing.T) {
	agent := NewAgentImpl("test", pureToolCallEventLoop{})
	res, err := agent.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	assert.Empty(t, res.Message)

	msgs := agent.Messages()
	require.Len(t, msgs, 2) // user + assistant
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Empty(t, msgs[1].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "c1", msgs[1].ToolCalls[0].ID)
	assert.Equal(t, "read_file", msgs[1].ToolCalls[0].Name)
}

// TestAgentHistoryMultipleToolCallTurnsAccumulate verifies that multiple Run
// calls with tool call responses accumulate in history.
func TestAgentHistoryMultipleToolCallTurnsAccumulate(t *testing.T) {
	agent := NewAgentImpl("test", pureToolCallEventLoop{})

	_, err := agent.Run(context.Background(), Submission{Content: "first"})
	require.NoError(t, err)

	_, err = agent.Run(context.Background(), Submission{Content: "second"})
	require.NoError(t, err)

	msgs := agent.Messages()
	// user1 + assistant1 + user2 + assistant2
	require.Len(t, msgs, 4)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "first", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	require.Len(t, msgs[1].ToolCalls, 1)
	assert.Equal(t, "user", msgs[2].Role)
	assert.Equal(t, "second", msgs[2].Content)
	assert.Equal(t, "assistant", msgs[3].Role)
	require.Len(t, msgs[3].ToolCalls, 1)
}

// TestLastAssistantMessageReturnsLastEvent verifies that lastAssistantMessage
// returns a pointer to the last event with Kind=="message".
func TestLastAssistantMessageReturnsLastEvent(t *testing.T) {
	events := []AgentEvent{
		{Kind: "message", Content: "first"},
		{Kind: "tool_call", Content: "read_file"},
		{Kind: "message", Content: "", ToolCalls: []llm.ToolCall{{ID: "c1", Name: "tool"}}},
		{Kind: "status", Content: "x"},
	}
	last := lastAssistantMessage(events)
	require.NotNil(t, last)
	assert.Equal(t, "", last.Content)
	require.Len(t, last.ToolCalls, 1)
	assert.Equal(t, "tool", last.ToolCalls[0].Name)

	// Returns nil when no message events exist.
	assert.Nil(t, lastAssistantMessage(nil))
	assert.Nil(t, lastAssistantMessage([]AgentEvent{}))
	assert.Nil(t, lastAssistantMessage([]AgentEvent{{Kind: "tool", Content: "x"}}))
}

// TestUsageStillPropagated verifies that Usage info is still propagated to
// history after the lastAssistantMessage refactor.
func TestUsageStillPropagated(t *testing.T) {
	agent := NewAgentImpl("test", usageEventLoop{})
	_, err := agent.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	msgs := agent.Messages()
	require.Len(t, msgs, 2) // user + assistant
	require.NotNil(t, msgs[1].Usage)
	assert.Equal(t, 10, msgs[1].Usage.InputTokens)
	assert.Equal(t, 5, msgs[1].Usage.OutputTokens)
	assert.Equal(t, 15, msgs[1].Usage.TotalTokens)
}
