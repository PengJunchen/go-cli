package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverflowRecoveryMiddleware_Name(t *testing.T) {
	m := NewOverflowRecoveryMiddleware()
	assert.Equal(t, "overflow", m.Name())
}

func TestOverflowRecoveryMiddleware_NoOverflow_Success(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return &Message{Role: "assistant", Content: "ok"}, nil
		},
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			return nil, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(model)

	result, err := wrapped.Generate(t.Context(), []Message{{Role: "user", Content: "hi"}})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Content)
}

func TestOverflowRecoveryMiddleware_Overflow_RetrySuccess(t *testing.T) {
	calls := 0
	model := &mockModel{
		generateFn: func(_ context.Context, msgs []Message, _ ...Option) (*Message, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("context_length_exceeded: too many tokens")
			}
			return &Message{Role: "assistant", Content: "recovered"}, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(2), WithOverflowTrimRatio(0.5))
	wrapped := mw.WrapModel(model)

	// 4 messages: system + 3 user messages.
	msgs := []Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "msg1"},
		{Role: "user", Content: "msg2"},
		{Role: "user", Content: "msg3"},
	}

	result, err := wrapped.Generate(t.Context(), msgs)
	require.NoError(t, err)
	assert.Equal(t, "recovered", result.Content)
	assert.Equal(t, 2, calls, "should have retried once")

	// Verify the retry had fewer messages (system + 1 trimmed).
	// With trimRatio 0.5 and 3 non-system msgs, trimCount=1.
}

func TestOverflowRecoveryMiddleware_Overflow_AllRetriesFail(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errors.New("context_length_exceeded")
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(1))
	wrapped := mw.WrapModel(model)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "1"},
		{Role: "user", Content: "2"},
		{Role: "user", Content: "3"},
		{Role: "user", Content: "4"},
	}

	_, err := wrapped.Generate(t.Context(), msgs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overflow")
	assert.Contains(t, err.Error(), "max retries")
}

func TestOverflowRecoveryMiddleware_NonOverflowError_NoRetry(t *testing.T) {
	calls := 0
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			calls++
			return nil, errors.New("internal server error")
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(3))
	wrapped := mw.WrapModel(model)

	_, err := wrapped.Generate(t.Context(), []Message{{Role: "user", Content: "hi"}})
	require.Error(t, err)
	assert.Equal(t, "internal server error", err.Error())
	assert.Equal(t, 1, calls, "should not retry non-overflow errors")
}

func TestOverflowRecoveryMiddleware_Stream_Overflow_RetrySuccess(t *testing.T) {
	calls := 0
	model := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("context_length_exceeded")
			}
			ch := make(chan MessageChunk, 1)
			ch <- MessageChunk{Content: "recovered", Final: true}
			close(ch)
			return ch, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(2))
	wrapped := mw.WrapModel(model)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "1"},
		{Role: "user", Content: "2"},
	}

	ch, err := wrapped.Stream(t.Context(), msgs)
	require.NoError(t, err)

	var chunks []string
	for chunk := range ch {
		chunks = append(chunks, chunk.Content)
	}
	assert.Contains(t, strings.Join(chunks, ""), "recovered")
	assert.Equal(t, 2, calls)
}

func TestOverflowRecoveryMiddleware_TrimMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "1"},
		{Role: "user", Content: "2"},
		{Role: "user", Content: "3"},
		{Role: "user", Content: "4"},
	}

	// Trim 50% of non-system messages (2 of 4).
	trimmed := trimMessages(msgs, 0.5)
	assert.Equal(t, 3, len(trimmed), "should have system + 2 remaining")
	assert.Equal(t, "sys", trimmed[0].Content)

	// Trim with single message (no trimming).
	single := []Message{{Role: "user", Content: "only"}}
	assert.Equal(t, single, trimMessages(single, 0.5))

	// Trim with only system message.
	sysOnly := []Message{{Role: "system", Content: "sys"}}
	assert.Equal(t, sysOnly, trimMessages(sysOnly, 0.5))
}

func TestOverflowRecoveryMiddleware_OverflowCount(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errors.New("context_length_exceeded")
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(1))
	wrapped := mw.WrapModel(model)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "1"},
		{Role: "user", Content: "2"},
		{Role: "user", Content: "3"},
		{Role: "user", Content: "4"},
	}

	_, _ = wrapped.Generate(t.Context(), msgs)

	mw.mu.Lock()
	count := mw.overflowCount
	mw.mu.Unlock()

	assert.Equal(t, 2, count, "should have detected overflow twice (initial + 1 retry)")
}

func TestIsOverflowError(t *testing.T) {
	assert.True(t, isOverflowError(fmt.Errorf("context_length_exceeded")))
	assert.True(t, isOverflowError(fmt.Errorf("maximum context length exceeded")))
	assert.True(t, isOverflowError(fmt.Errorf("token limit exceeded")))
	assert.False(t, isOverflowError(fmt.Errorf("internal server error")))
	assert.False(t, isOverflowError(nil))
}

func TestOverflowRecoveryMiddleware_AllMessagesTrimmed(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errors.New("context_length_exceeded")
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(5), WithOverflowTrimRatio(1.0))
	wrapped := mw.WrapModel(model)

	// Single user message (no system to preserve).
	msgs := []Message{{Role: "user", Content: "only"}}

	_, err := wrapped.Generate(t.Context(), msgs)
	require.Error(t, err)
	// Should fail because trimming a single message leaves nothing.
}

func TestOverflowRecoveryMiddleware_Concurrent(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			time.Sleep(10 * time.Millisecond)
			return &Message{Role: "assistant", Content: "ok"}, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(model)

	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := wrapped.Generate(context.Background(), []Message{{Role: "user", Content: "hi"}})
			done <- err
		}()
	}

	for i := 0; i < 10; i++ {
		err := <-done
		assert.NoError(t, err)
	}
}
