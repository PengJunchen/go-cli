package llm

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoopDetectStreamRealTimeForwarding verifies that chunks are forwarded
// in real-time, not buffered until the stream completes.
func TestLoopDetectStreamRealTimeForwarding(t *testing.T) {
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

	mw := NewLoopDetectionModelMiddleware(WithLoopThreshold(3), WithLoopWindowSize(5))
	wrapped := mw.WrapModel(inner)

	start := time.Now()
	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	firstChunk := <-ch
	elapsed := time.Since(start)

	// First chunk should arrive well before the full 100ms stream completes.
	assert.Less(t, elapsed, 100*time.Millisecond,
		"first chunk should arrive in real-time")
	assert.Equal(t, "first", firstChunk.Content)

	// Drain remaining chunks.
	for range ch {
	}
}

// TestLoopDetectAfterCompletion verifies that after streaming the same content
// threshold times, the loop detection triggers. Since chunks are forwarded
// in real-time, the loop is detected post-hoc (logged and recorded in
// history). The next Generate call with the same content returns an error.
func TestLoopDetectAfterCompletion(t *testing.T) {
	inner := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk, 1)
			ch <- MessageChunk{Role: RoleAssistant, Content: "repeat", Final: true}
			close(ch)
			return ch, nil
		},
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return &Message{Role: RoleAssistant, Content: "repeat"}, nil
		},
	}

	mw := NewLoopDetectionModelMiddleware(WithLoopThreshold(2), WithLoopWindowSize(5))
	wrapped := mw.WrapModel(inner)

	// Stream twice: both times forward chunks in real-time.
	// Post-hoc checkLoop records "repeat" twice, which triggers loop detection.
	for i := 0; i < 2; i++ {
		ch, err := wrapped.Stream(context.Background(), nil)
		require.NoError(t, err)
		var contents []string
		for chunk := range ch {
			if chunk.Content != "" {
				contents = append(contents, chunk.Content)
			}
		}
		assert.Contains(t, contents, "repeat")
	}

	// After the loop is detected post-hoc (threshold=2, two "repeat" recorded),
	// the next Generate call with the same content should return an error
	// because checkLoop detects the loop from the accumulated history.
	_, err := wrapped.Generate(context.Background(), nil)
	assert.Error(t, err, "Generate should return loop detection error after threshold")
	if err != nil {
		assert.Contains(t, strings.ToLower(err.Error()), "loopdetection",
			"error should mention loopdetection")
	}
}

// TestLoopDetectNoLoopWorksNormally verifies that streaming unique content
// works without errors and all chunks are forwarded.
func TestLoopDetectNoLoopWorksNormally(t *testing.T) {
	var callCount atomic.Int32
	inner := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			callCount.Add(1)
			ch := make(chan MessageChunk, 3)
			ch <- MessageChunk{Role: RoleAssistant, Content: "unique-a"}
			ch <- MessageChunk{Role: RoleAssistant, Content: "unique-b"}
			ch <- MessageChunk{Role: RoleAssistant, Content: "unique-c", Final: true}
			close(ch)
			return ch, nil
		},
	}

	mw := NewLoopDetectionModelMiddleware(WithLoopThreshold(3), WithLoopWindowSize(5))
	wrapped := mw.WrapModel(inner)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var contents []string
	for chunk := range ch {
		if chunk.Content != "" {
			contents = append(contents, chunk.Content)
		}
	}
	assert.Equal(t, []string{"unique-a", "unique-b", "unique-c"}, contents)
	assert.Equal(t, int32(1), callCount.Load())
}

// TestLoopDetectInitErrorReturnsError verifies that when the inner Stream
// returns an initialization error, the middleware propagates it without
// panic.
func TestLoopDetectInitErrorReturnsError(t *testing.T) {
	initErr := errors.New("stream init failed")
	inner := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			return nil, initErr
		},
	}

	mw := NewLoopDetectionModelMiddleware()
	wrapped := mw.WrapModel(inner)

	_, err := wrapped.Stream(context.Background(), nil)
	require.Error(t, err)
	assert.Equal(t, initErr, err)
}
