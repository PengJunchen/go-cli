package llm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTimeoutModelMiddleware_Name verifies the middleware identifier.
func TestTimeoutModelMiddleware_Name(t *testing.T) {
	mw := NewTimeoutModelMiddleware()
	assert.Equal(t, "timeout", mw.Name())
}

// TestTimeoutModelMiddleware_GenerateNoTimeout verifies that a fast Generate
// call completes without error.
func TestTimeoutModelMiddleware_GenerateNoTimeout(t *testing.T) {
	model := &mockModel{}
	mw := NewTimeoutModelMiddleware(WithTotalTimeout(1 * time.Second))
	wrapped := mw.WrapModel(model)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
}

// TestTimeoutModelMiddleware_GenerateTimeout verifies that a slow Generate call
// is cancelled by the total timeout.
func TestTimeoutModelMiddleware_GenerateTimeout(t *testing.T) {
	model := &mockModel{
		generateFn: func(ctx context.Context, _ []Message, _ ...Option) (*Message, error) {
			select {
			case <-time.After(5 * time.Second):
				return &Message{Role: RoleAssistant, Content: "late"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	mw := NewTimeoutModelMiddleware(WithTotalTimeout(50 * time.Millisecond))
	wrapped := mw.WrapModel(model)

	start := time.Now()
	_, err := wrapped.Generate(context.Background(), nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 500*time.Millisecond, "should timeout well before 5s")
}

// TestTimeoutModelMiddleware_StreamNoTimeout verifies that a fast Stream call
// completes without error.
func TestTimeoutModelMiddleware_StreamNoTimeout(t *testing.T) {
	model := &mockModel{}
	mw := NewTimeoutModelMiddleware(
		WithTotalTimeout(1*time.Second),
		WithStreamChunkTimeout(500*time.Millisecond),
	)
	wrapped := mw.WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var content string
	for chunk := range ch {
		content += chunk.Content
	}
	assert.Equal(t, "ok", content)
}

// TestTimeoutModelMiddleware_StreamChunkTimeout verifies that the chunk timeout
// fires when chunks arrive too slowly.
func TestTimeoutModelMiddleware_StreamChunkTimeout(t *testing.T) {
	model := &mockModel{
		streamFn: func(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk, 1)
			go func() {
				defer close(ch)
				ch <- MessageChunk{Role: RoleAssistant, Content: "first"}
				select {
				case <-time.After(2 * time.Second):
					select {
					case ch <- MessageChunk{Role: RoleAssistant, Content: "second"}:
					case <-ctx.Done():
					}
				case <-ctx.Done():
				}
			}()
			return ch, nil
		},
	}
	mw := NewTimeoutModelMiddleware(
		WithTotalTimeout(10*time.Second),
		WithStreamChunkTimeout(50*time.Millisecond),
	)
	wrapped := mw.WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	start := time.Now()
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	elapsed := time.Since(start)

	// Should receive only the first chunk, then the channel closes due to
	// the chunk timeout.
	require.Len(t, chunks, 1, "should receive only the first chunk before timeout")
	assert.Equal(t, "first", chunks[0].Content)
	assert.Less(t, elapsed, 1*time.Second, "should timeout well before the 2s delay")
}

// TestTimeoutModelMiddleware_GeneratePropagatesNonTimeoutError verifies that
// non-timeout errors are returned as-is.
func TestTimeoutModelMiddleware_GeneratePropagatesNonTimeoutError(t *testing.T) {
	errSentinel := errors.New("model error")
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errSentinel
		},
	}
	mw := NewTimeoutModelMiddleware(WithTotalTimeout(1 * time.Second))
	wrapped := mw.WrapModel(model)

	_, err := wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errSentinel)
}

// TestTimeoutModelMiddleware_StreamMultipleChunks verifies that multiple chunks
// arriving within the chunk timeout are all forwarded.
func TestTimeoutModelMiddleware_StreamMultipleChunks(t *testing.T) {
	model := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk, 3)
			ch <- MessageChunk{Role: RoleAssistant, Content: "a"}
			ch <- MessageChunk{Role: RoleAssistant, Content: "b"}
			ch <- MessageChunk{Role: RoleAssistant, Content: "c", Final: true}
			close(ch)
			return ch, nil
		},
	}
	mw := NewTimeoutModelMiddleware(
		WithTotalTimeout(1*time.Second),
		WithStreamChunkTimeout(500*time.Millisecond),
	)
	wrapped := mw.WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var content string
	for chunk := range ch {
		content += chunk.Content
	}
	assert.Equal(t, "abc", content)
}
