package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOverflowStreamRealTimeForwarding verifies that chunks are forwarded
// in real-time, not buffered until the stream completes. The inner model
// sends chunks with delays; the first chunk should arrive before all chunks
// are produced.
func TestOverflowStreamRealTimeForwarding(t *testing.T) {
	inner := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk, 3)
			go func() {
				ch <- MessageChunk{Role: RoleAssistant, Content: "first"}
				time.Sleep(50 * time.Millisecond)
				ch <- MessageChunk{Role: RoleAssistant, Content: "second"}
				time.Sleep(50 * time.Millisecond)
				ch <- MessageChunk{Role: RoleAssistant, Content: "third", Final: true}
				close(ch)
			}()
			return ch, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(inner)

	start := time.Now()
	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	firstChunk := <-ch
	elapsed := time.Since(start)

	// First chunk should arrive well before the full 100ms stream completes.
	assert.Less(t, elapsed, 100*time.Millisecond,
		"first chunk should arrive in real-time, not after full stream")
	assert.Equal(t, "first", firstChunk.Content)

	// Drain remaining chunks.
	for range ch {
	}
}

// TestOverflowStreamRetriesOnOverflow verifies that when the inner Stream
// returns an overflow error, the middleware trims messages and retries.
// The retry should succeed and forward chunks.
func TestOverflowStreamRetriesOnOverflow(t *testing.T) {
	var callCount atomic.Int32
	inner := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			n := callCount.Add(1)
			if n == 1 {
				// First call: overflow error.
				return nil, errors.New("context_length_exceeded")
			}
			// Second call: success.
			ch := make(chan MessageChunk, 1)
			ch <- MessageChunk{Role: RoleAssistant, Content: "recovered", Final: true}
			close(ch)
			return ch, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(2), WithOverflowTrimRatio(0.5))
	wrapped := mw.WrapModel(inner)

	// Provide enough messages for trimming.
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "msg1"},
		{Role: "user", Content: "msg2"},
		{Role: "user", Content: "msg3"},
		{Role: "user", Content: "msg4"},
	}

	ch, err := wrapped.Stream(context.Background(), msgs)
	require.NoError(t, err)

	var contents []string
	for chunk := range ch {
		if chunk.Content != "" {
			contents = append(contents, chunk.Content)
		}
	}
	assert.Contains(t, contents, "recovered")
	assert.Equal(t, int32(2), callCount.Load(), "should have retried once")
}

// TestOverflowStreamExhaustsRetriesReturnsError verifies that when all retry
// attempts overflow, an error is returned.
func TestOverflowStreamExhaustsRetriesReturnsError(t *testing.T) {
	inner := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			return nil, errors.New("context_length_exceeded")
		},
	}

	mw := NewOverflowRecoveryMiddleware(WithOverflowMaxRetries(1), WithOverflowTrimRatio(0.5))
	wrapped := mw.WrapModel(inner)

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "msg1"},
		{Role: "user", Content: "msg2"},
		{Role: "user", Content: "msg3"},
		{Role: "user", Content: "msg4"},
	}

	_, err := wrapped.Stream(context.Background(), msgs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overflow")
}

// TestOverflowStreamNoOverflowWorksNormally verifies that a normal stream
// (no overflow) forwards all chunks without retry.
func TestOverflowStreamNoOverflowWorksNormally(t *testing.T) {
	var callCount atomic.Int32
	inner := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			callCount.Add(1)
			ch := make(chan MessageChunk, 3)
			ch <- MessageChunk{Role: RoleAssistant, Content: "a"}
			ch <- MessageChunk{Role: RoleAssistant, Content: "b"}
			ch <- MessageChunk{Role: RoleAssistant, Content: "c", Final: true}
			close(ch)
			return ch, nil
		},
	}

	mw := NewOverflowRecoveryMiddleware()
	wrapped := mw.WrapModel(inner)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var contents []string
	for chunk := range ch {
		if chunk.Content != "" {
			contents = append(contents, chunk.Content)
		}
	}
	assert.Equal(t, []string{"a", "b", "c"}, contents)
	assert.Equal(t, int32(1), callCount.Load(), "should not retry on success")
}
