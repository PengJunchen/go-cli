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
// is canceled by the total timeout.
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

func TestTimeoutModelMiddleware_StreamRejectsDataAfterFinal(t *testing.T) {
	model := &mockModel{
		streamFn: func(context.Context, []Message, ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk, 2)
			ch <- MessageChunk{Role: RoleAssistant, Content: "complete", Final: true}
			ch <- MessageChunk{Role: RoleAssistant, Content: "late"}
			close(ch)
			return ch, nil
		},
	}
	wrapped := NewTimeoutModelMiddleware(
		WithTotalTimeout(time.Second),
		WithStreamChunkTimeout(time.Second),
	).WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	chunks := make([]MessageChunk, 0, 2)
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	require.Len(t, chunks, 2)
	require.True(t, chunks[0].Final)
	require.ErrorContains(t, chunks[1].Error, "emitted data after its final chunk")
}

func TestTimeoutModelMiddleware_StreamPreservesErrorAfterFinal(t *testing.T) {
	lateErr := errors.New("late provider failure")
	model := &mockModel{
		streamFn: func(context.Context, []Message, ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk, 2)
			ch <- MessageChunk{Role: RoleAssistant, Content: "complete", Final: true}
			ch <- MessageChunk{Error: lateErr}
			close(ch)
			return ch, nil
		},
	}
	wrapped := NewTimeoutModelMiddleware(
		WithTotalTimeout(time.Second),
		WithStreamChunkTimeout(time.Second),
	).WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	chunks := make([]MessageChunk, 0, 2)
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	require.Len(t, chunks, 2)
	require.True(t, chunks[0].Final)
	require.ErrorIs(t, chunks[1].Error, lateErr)
}

func TestTimeoutModelMiddleware_FinalMustBeFollowedByChannelClose(t *testing.T) {
	innerStopped := make(chan struct{})
	model := &mockModel{
		streamFn: func(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			ch := make(chan MessageChunk, 1)
			ch <- MessageChunk{Role: RoleAssistant, Content: "terminal", Final: true}
			go func() {
				<-ctx.Done()
				close(innerStopped)
				close(ch)
			}()
			return ch, nil
		},
	}
	wrapped := NewTimeoutModelMiddleware(
		WithTotalTimeout(time.Second),
		WithStreamChunkTimeout(25*time.Millisecond),
	).WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)
	chunks := make([]MessageChunk, 0, 2)
	for chunk := range ch {
		chunks = append(chunks, chunk)
	}
	require.Len(t, chunks, 2)
	require.True(t, chunks[0].Final)
	require.ErrorContains(t, chunks[1].Error, "idle exceeded")
	select {
	case <-innerStopped:
	case <-time.After(time.Second):
		t.Fatal("inner producer did not stop after timeout cancellation")
	}
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

	// The first chunk remains visible, followed by an explicit terminal error.
	require.Len(t, chunks, 2, "timeout must follow the already delivered chunk")
	assert.Equal(t, "first", chunks[0].Content)
	require.Error(t, chunks[1].Error)
	assert.False(t, chunks[1].Final)
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
