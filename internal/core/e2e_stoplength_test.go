//go:build e2e

package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// TestET_stop_length_continuation verifies that when the LLM returns
// finish_reason="length", the loop automatically continues and merges the
// response. It also verifies that at most 3 continuation attempts are issued.
func TestET_stop_length_continuation(t *testing.T) {
	// Subtest 1: A truncated first response (finish_reason="length") causes the
	// loop to issue a continuation request. The continuation response
	// (finish_reason="stop") completes the answer. The final output must be the
	// merged content of both responses.
	t.Run("merges_truncated_response", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		model := mock.NewMockLLMServer(mock.NewConversationTemplate(
			"E2E-SL-01", "continuation",
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

		events, err := loop.Run(ctx, Submission{Content: "what is the answer"})
		require.NoError(t, err)

		// Two LLM calls: original + one continuation.
		assert.Equal(t, 2, model.CallCount())

		// The final output is the merged content.
		messages := findEvents(events, "message")
		require.Len(t, messages, 1)
		assert.Equal(t, "The answer is forty-two.", messages[0])

		// The continuation request must include the partial assistant response
		// so the model can pick up where it left off.
		secondCallMsgs := model.CallLog()[1].Messages
		var foundPartial bool
		for _, m := range secondCallMsgs {
			if m.Role == llm.RoleAssistant && m.Content == "The answer is " {
				foundPartial = true
			}
		}
		assert.True(t, foundPartial,
			"continuation request must include the partial assistant response")
	})

	// Subtest 2: When the model keeps returning finish_reason="length", the
	// loop issues at most 3 continuation attempts (1 original + 3 continuations
	// = 4 total calls). The merged content includes all partial chunks.
	t.Run("max_3_continuation_attempts", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		model := mock.NewMockLLMServer(mock.NewConversationTemplate(
			"E2E-SL-02", "max-attempts",
			mock.ConversationTurn{AssistantContent: "p1", FinishReason: "length"},
			mock.ConversationTurn{AssistantContent: "p2", FinishReason: "length"},
			mock.ConversationTurn{AssistantContent: "p3", FinishReason: "length"},
			mock.ConversationTurn{AssistantContent: "p4", FinishReason: "length"},
		))
		loop := NewLoopAgent(WithLLM(model))

		events, err := loop.Run(ctx, Submission{Content: "go"})
		require.NoError(t, err)

		// 1 original + 3 continuations = 4 total calls. The loop must not
		// issue a 5th call even though the 4th response is still truncated.
		assert.Equal(t, 4, model.CallCount())

		// The merged content includes all four partial chunks.
		messages := findEvents(events, "message")
		require.Len(t, messages, 1)
		assert.Equal(t, "p1p2p3p4", messages[0])
	})

	// Subtest 3: Multiple continuation chunks are merged correctly. The first
	// response is truncated, then two continuation requests complete it.
	t.Run("multiple_continuation_chunks", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		model := mock.NewMockLLMServer(mock.NewConversationTemplate(
			"E2E-SL-03", "multi-continuation",
			mock.ConversationTurn{AssistantContent: "A", FinishReason: "length"},
			mock.ConversationTurn{AssistantContent: "B", FinishReason: "length"},
			mock.ConversationTurn{AssistantContent: "C", FinishReason: "stop"},
		))
		loop := NewLoopAgent(WithLLM(model))

		events, err := loop.Run(ctx, Submission{Content: "go"})
		require.NoError(t, err)

		// 1 original + 2 continuations = 3 total calls.
		assert.Equal(t, 3, model.CallCount())

		messages := findEvents(events, "message")
		require.Len(t, messages, 1)
		assert.Equal(t, "ABC", messages[0])
	})
}
