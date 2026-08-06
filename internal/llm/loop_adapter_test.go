package llm

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// simpleTestModel is a minimal BaseChatModel for testing the adapter.
type simpleTestModel struct {
	calls int32
}

func (m *simpleTestModel) Generate(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
	atomic.AddInt32(&m.calls, 1)
	return &Message{Role: RoleAssistant, Content: "test"}, nil
}

func (m *simpleTestModel) Stream(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	atomic.AddInt32(&m.calls, 1)
	ch := make(chan MessageChunk, 1)
	ch <- MessageChunk{Role: RoleAssistant, Content: "test", Final: true}
	close(ch)
	return ch, nil
}

var _ BaseChatModel = (*simpleTestModel)(nil)

// trackingMiddleware is a minimal ModelMiddleware that wraps the model and
// tracks whether WrapModel was called.
type trackingMiddleware struct {
	name    string
	wrapped int32
}

func (m *trackingMiddleware) Name() string { return m.name }

func (m *trackingMiddleware) WrapModel(next BaseChatModel) BaseChatModel {
	atomic.AddInt32(&m.wrapped, 1)
	return &trackingWrappedModel{next: next}
}

var _ ModelMiddleware = (*trackingMiddleware)(nil)

type trackingWrappedModel struct {
	next BaseChatModel
}

func (m *trackingWrappedModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	return m.next.Generate(ctx, msgs, opts...)
}

func (m *trackingWrappedModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	return m.next.Stream(ctx, msgs, opts...)
}

var _ BaseChatModel = (*trackingWrappedModel)(nil)

func TestWrapModelForLoop(t *testing.T) {
	chain := NewModelMiddlewareChain()
	mw := &trackingMiddleware{name: "tracking"}
	err := chain.Register(mw)
	require.NoError(t, err)

	model := &simpleTestModel{}
	wrapper := WrapModelForLoop(chain)
	require.NotNil(t, wrapper)

	result := wrapper(model)
	assert.NotNil(t, result)
	assert.Equal(t, int32(1), atomic.LoadInt32(&mw.wrapped), "middleware WrapModel should have been called")

	// The result should be a BaseChatModel (the wrapped version).
	wrappedModel, ok := result.(BaseChatModel)
	assert.True(t, ok, "wrapped result should satisfy BaseChatModel")

	// Using the wrapped model should delegate to the original.
	resp, err := wrappedModel.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "test", resp.Content)
	assert.Equal(t, int32(1), atomic.LoadInt32(&model.calls), "underlying model should have been called")
}

func TestWrapModelForLoopEmptyChain(t *testing.T) {
	chain := NewModelMiddlewareChain()
	wrapper := WrapModelForLoop(chain)
	require.NotNil(t, wrapper)

	model := &simpleTestModel{}
	result := wrapper(model)
	assert.NotNil(t, result)

	// With an empty chain, the model should still be a BaseChatModel.
	_, ok := result.(BaseChatModel)
	assert.True(t, ok)
}

func TestWrapModelForLoopNonBaseChatModel(t *testing.T) {
	chain := NewModelMiddlewareChain()
	wrapper := WrapModelForLoop(chain)

	// Pass a non-BaseChatModel; should be returned unchanged.
	input := "not a model"
	result := wrapper(input)
	assert.Equal(t, input, result)
}

func TestWrapModelForLoopMultipleMiddlewares(t *testing.T) {
	chain := NewModelMiddlewareChain()
	mw1 := &trackingMiddleware{name: "mw1"}
	mw2 := &trackingMiddleware{name: "mw2"}
	err := chain.Register(mw1)
	require.NoError(t, err)
	err = chain.Register(mw2)
	require.NoError(t, err)

	model := &simpleTestModel{}
	wrapper := WrapModelForLoop(chain)
	result := wrapper(model)

	assert.NotNil(t, result)
	assert.Equal(t, int32(1), atomic.LoadInt32(&mw1.wrapped))
	assert.Equal(t, int32(1), atomic.LoadInt32(&mw2.wrapped))

	_, ok := result.(BaseChatModel)
	assert.True(t, ok)
}

func TestWrapModelForLoopReturnsFuncAnyAny(t *testing.T) {
	chain := NewModelMiddlewareChain()
	wrapper := WrapModelForLoop(chain)

	// Verify the return type is func(any) any, which is structurally
	// identical to core.ModelWrapper.
	assert.NotNil(t, wrapper)
}
