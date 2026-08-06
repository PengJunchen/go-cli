package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// TestStopReasonDoesNotTriggerContinuation verifies that a normal finish_reason
// of "stop" does not cause any continuation requests.
func TestStopReasonDoesNotTriggerContinuation(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SL-01", "no-continuation",
		mock.ConversationTurn{
			AssistantContent: "hello from model",
			FinishReason:     "stop",
		},
	))
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)

	// Only one LLM call — no continuation.
	assert.Equal(t, 1, model.CallCount())

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "hello from model", messages[0])
}

// TestLengthReasonTriggersContinuation verifies that finish_reason="length"
// causes the loop to issue a continuation request.
func TestLengthReasonTriggersContinuation(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SL-02", "continuation",
		mock.ConversationTurn{
			AssistantContent: "partial",
			FinishReason:     "length",
		},
		mock.ConversationTurn{
			AssistantContent: " complete",
			FinishReason:     "stop",
		},
	))
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	// Two LLM calls: original + one continuation.
	assert.Equal(t, 2, model.CallCount())

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "partial complete", messages[0])
}

// TestContinuationMergesResponses verifies that the continuation content is
// merged with the original partial content.
func TestContinuationMergesResponses(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SL-03", "merge",
		mock.ConversationTurn{
			AssistantContent: "The answer is ",
			FinishReason:     "length",
		},
		mock.ConversationTurn{
			AssistantContent: "forty-two.",
			FinishReason:     "stop",
		},
	))
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "what is the answer"})
	require.NoError(t, err)
	assert.Equal(t, 2, model.CallCount())

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "The answer is forty-two.", messages[0])
}

// TestContinuationAppendsPartialContent verifies that the continuation request
// includes the partial assistant response in the conversation.
func TestContinuationAppendsPartialContent(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SL-04", "append-partial",
		mock.ConversationTurn{
			AssistantContent: "partial text",
			FinishReason:     "length",
		},
		mock.ConversationTurn{
			AssistantContent: " done",
			FinishReason:     "stop",
		},
	))
	loop := NewLoopAgent(WithLLM(model))

	_, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	require.Equal(t, 2, model.CallCount())

	// The second call's messages must include an assistant message carrying
	// the partial content, proving the continuation request was built correctly.
	secondCallMsgs := model.CallLog()[1].Messages
	var foundPartial bool
	for _, m := range secondCallMsgs {
		if m.Role == llm.RoleAssistant && m.Content == "partial text" {
			foundPartial = true
		}
	}
	assert.True(t, foundPartial, "continuation request must include the partial assistant response")
}

// TestMaxContinuationAttemptsEnforced verifies that at most 3 continuation
// attempts are issued even if the model keeps returning finish_reason="length".
func TestMaxContinuationAttemptsEnforced(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SL-05", "max-attempts",
		mock.ConversationTurn{AssistantContent: "p1", FinishReason: "length"},
		mock.ConversationTurn{AssistantContent: "p2", FinishReason: "length"},
		mock.ConversationTurn{AssistantContent: "p3", FinishReason: "length"},
		mock.ConversationTurn{AssistantContent: "p4", FinishReason: "length"},
	))
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	// 1 original + 3 continuations = 4 total calls.
	assert.Equal(t, 4, model.CallCount())

	// The merged content includes all four partial chunks.
	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "p1p2p3p4", messages[0])
}

// TestContinuationMultipleChunks verifies that multi-chunk continuation works:
// the first response is truncated, then two continuation requests complete it.
func TestContinuationMultipleChunks(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SL-06", "multi-continuation",
		mock.ConversationTurn{AssistantContent: "A", FinishReason: "length"},
		mock.ConversationTurn{AssistantContent: "B", FinishReason: "length"},
		mock.ConversationTurn{AssistantContent: "C", FinishReason: "stop"},
	))
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	// 1 original + 2 continuations = 3 total calls.
	assert.Equal(t, 3, model.CallCount())

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "ABC", messages[0])
}

// TestContinuationEmptyFinishReasonNoContinuation verifies that an empty
// finish_reason (e.g. from a provider that doesn't send one) does not trigger
// continuation.
func TestContinuationEmptyFinishReasonNoContinuation(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SL-07", "empty-finish",
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	assert.Equal(t, 1, model.CallCount())

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "done", messages[0])
}
