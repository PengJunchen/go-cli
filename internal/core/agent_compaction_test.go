package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
)

func TestWithCompactionHook(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "compaction-test",
		mock.ConversationTurn{AssistantContent: "response"},
	))

	loop := NewLoopAgent(WithLLM(model))

	var hookCalled bool
	var receivedMsgs []AgentMessage
	hook := func(_ context.Context, messages []AgentMessage) ([]AgentMessage, error) {
		hookCalled = true
		receivedMsgs = messages
		// Keep only the last 2 messages.
		if len(messages) > 2 {
			return messages[len(messages)-2:], nil
		}
		return messages, nil
	}

	agent := NewAgentImpl("test", loop, WithCompactionHook(hook))

	result, err := agent.Run(context.Background(), Submission{Content: "hello"})
	require.NoError(t, err)
	assert.True(t, result.Success)

	assert.True(t, hookCalled, "compaction hook should have been called")
	assert.NotEmpty(t, receivedMsgs, "hook should receive the history")

	// After compaction, history should have at most 2 messages.
	msgs := agent.Messages()
	assert.LessOrEqual(t, len(msgs), 2, "history should be compacted to at most 2 messages")
}

func TestCompactionHookError(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "compaction-error-test",
		mock.ConversationTurn{AssistantContent: "response"},
	))

	loop := NewLoopAgent(WithLLM(model))

	hook := func(_ context.Context, messages []AgentMessage) ([]AgentMessage, error) {
		return nil, errors.New("compaction failed")
	}

	agent := NewAgentImpl("test", loop, WithCompactionHook(hook))

	result, err := agent.Run(context.Background(), Submission{Content: "hello"})
	require.NoError(t, err)
	assert.True(t, result.Success)

	// History should be preserved when compaction fails.
	msgs := agent.Messages()
	assert.Equal(t, 2, len(msgs), "history should be preserved on compaction error")
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "hello", msgs[0].Content)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "response", msgs[1].Content)
}

func TestWithoutCompactionHook(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "no-compaction-test",
		mock.ConversationTurn{AssistantContent: "response"},
	))

	loop := NewLoopAgent(WithLLM(model))

	agent := NewAgentImpl("test", loop)

	result, err := agent.Run(context.Background(), Submission{Content: "hello"})
	require.NoError(t, err)
	assert.True(t, result.Success)

	// History should have user + assistant messages.
	msgs := agent.Messages()
	assert.Equal(t, 2, len(msgs))
}

func TestCompactionHookReceivesUpdatedHistory(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "compaction-receives-history",
		mock.ConversationTurn{AssistantContent: "first response"},
		mock.ConversationTurn{AssistantContent: "second response"},
	))

	loop := NewLoopAgent(WithLLM(model))

	var hookCallCount int
	var lastReceivedCount int
	hook := func(_ context.Context, messages []AgentMessage) ([]AgentMessage, error) {
		hookCallCount++
		lastReceivedCount = len(messages)
		return messages, nil // no compaction
	}

	agent := NewAgentImpl("test", loop, WithCompactionHook(hook))

	// First run: user message + assistant response = 2 messages.
	_, err := agent.Run(context.Background(), Submission{Content: "first"})
	require.NoError(t, err)
	assert.Equal(t, 1, hookCallCount)
	assert.Equal(t, 2, lastReceivedCount)

	// Second run: previous 2 + new user + assistant = 4 messages.
	_, err = agent.Run(context.Background(), Submission{Content: "second"})
	require.NoError(t, err)
	assert.Equal(t, 2, hookCallCount)
	assert.Equal(t, 4, lastReceivedCount)
}
