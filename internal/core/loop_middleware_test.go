package core

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// countingWrapperModel wraps a BaseChatModel and counts Generate/Stream calls
// to verify that the wrapped model is actually used by the loop.
type countingWrapperModel struct {
	inner llm.BaseChatModel
	calls int32
}

func (w *countingWrapperModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	atomic.AddInt32(&w.calls, 1)
	return w.inner.Generate(ctx, msgs, opts...)
}

func (w *countingWrapperModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	atomic.AddInt32(&w.calls, 1)
	return w.inner.Stream(ctx, msgs, opts...)
}

func TestWithModelWrapper(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "wrapper-test",
		mock.ConversationTurn{AssistantContent: "wrapped response"},
	))

	wrapper := &countingWrapperModel{inner: model}

	var wrapCalled bool
	loop := NewLoopAgent(
		WithLLM(model),
		WithModelWrapper(func(m any) any {
			wrapCalled = true
			return wrapper
		}),
	)

	events, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	assert.True(t, wrapCalled, "model wrapper should have been called")
	assert.Equal(t, int32(1), atomic.LoadInt32(&wrapper.calls), "wrapped model should have been used")

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "wrapped response", messages[0])
}

func TestWithModelWrapperPassThrough(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "passthrough-test",
		mock.ConversationTurn{AssistantContent: "hello"},
	))

	var wrapCalled bool
	loop := NewLoopAgent(
		WithLLM(model),
		WithModelWrapper(func(m any) any {
			wrapCalled = true
			return m // pass-through
		}),
	)

	events, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	assert.True(t, wrapCalled)

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "hello", messages[0])
}

func TestWithoutModelWrapper(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "no-wrapper-test",
		mock.ConversationTurn{AssistantContent: "no wrapper"},
	))

	loop := NewLoopAgent(WithLLM(model))

	events, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "no wrapper", messages[0])
}

func TestWithModelWrapperReturnsNonBaseChatModel(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"T", "bad-wrapper-test",
		mock.ConversationTurn{AssistantContent: "fallback"},
	))

	// Wrapper returns a non-BaseChatModel; the loop should fall back to the
	// original model.
	loop := NewLoopAgent(
		WithLLM(model),
		WithModelWrapper(func(m any) any {
			return "not a model"
		}),
	)

	events, err := loop.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)

	messages := findEvents(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "fallback", messages[0])
}
