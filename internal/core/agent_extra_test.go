package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestLastMessageEventSkipsEmpty(t *testing.T) {
	events := []AgentEvent{
		{Kind: "message", Content: "first"},
		{Kind: "message", Content: ""},
		{Kind: "status", Content: "x"},
	}
	// Trailing empty message must not overwrite first.
	assert.Equal(t, "first", lastMessageEvent(events))
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
