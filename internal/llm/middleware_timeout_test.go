package llm

import (
	"context"
	"errors"
	"sync"
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
	require.False(t, chunks[0].Final, "the final marker must be held until source close")
	require.Equal(t, "complete", chunks[0].Content)
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
	require.False(t, chunks[0].Final, "the final marker must be held until source close")
	require.Equal(t, "complete", chunks[0].Content)
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
	require.False(t, chunks[0].Final, "the final marker must be held until source close")
	require.Equal(t, "terminal", chunks[0].Content)
	require.ErrorContains(t, chunks[1].Error, "idle exceeded")
	select {
	case <-innerStopped:
	case <-time.After(time.Second):
		t.Fatal("inner producer did not stop after timeout cancellation")
	}
}

func TestTimeoutModelMiddleware_StreamTotalTimeoutIncludesSynchronousSetup(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	observedCause := make(chan error, 1)
	model := &mockModel{
		streamFn: func(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			close(started)
			select {
			case <-ctx.Done():
				observedCause <- context.Cause(ctx)
				return nil, ctx.Err()
			case <-release:
				return nil, errors.New("setup released")
			}
		},
	}
	wrapped := NewTimeoutModelMiddleware(
		WithTotalTimeout(15*time.Millisecond),
		WithStreamChunkTimeout(time.Second),
	).WrapModel(model)
	result := make(chan error, 1)
	go func() {
		_, err := wrapped.Stream(context.Background(), nil)
		result <- err
	}()
	<-started

	var streamErr error
	select {
	case streamErr = <-result:
	case <-time.After(100 * time.Millisecond):
		close(release)
		streamErr = <-result
	}
	require.ErrorIs(t, streamErr, context.DeadlineExceeded,
		"the configured total timeout must cover synchronous Stream setup")
	select {
	case cause := <-observedCause:
		var localAbort *LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.ErrorIs(t, cause, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("source setup did not observe the configured local deadline")
	}
}

func TestTimeoutModelMiddleware_StreamPreservesCallerDeadlineCause(t *testing.T) {
	observedCause := make(chan error, 1)
	model := &mockModel{
		streamFn: func(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			<-ctx.Done()
			observedCause <- context.Cause(ctx)
			return nil, ctx.Err()
		},
	}
	wrapped := NewTimeoutModelMiddleware(
		WithTotalTimeout(time.Second),
		WithStreamChunkTimeout(time.Second),
	).WrapModel(model)
	callerCtx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()

	_, err := wrapped.Stream(callerCtx, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	var localAbort *LocalStreamAbortCause
	require.NotErrorAs(t, err, &localAbort, "the caller's deadline must not be relabeled as a local timeout")
	select {
	case cause := <-observedCause:
		require.ErrorIs(t, cause, context.DeadlineExceeded)
		require.NotErrorAs(t, cause, &localAbort, "the source must retain the caller's deadline cause")
	case <-time.After(time.Second):
		t.Fatal("source did not observe the caller's deadline")
	}
}

func TestReview812cProbeLateCallerDeadlineDoesNotReplaceEarlierBudgetCause(t *testing.T) {
	started := make(chan struct{})
	observedCause := make(chan error, 1)
	sourceRelease := make(chan struct{})
	releaseSource := sync.OnceFunc(func() { close(sourceRelease) })
	defer releaseSource()
	setupErr := errors.New("late setup error")
	var callerCtx context.Context
	model := &mockModel{
		streamFn: func(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			close(started)
			<-ctx.Done()
			observedCause <- context.Cause(ctx)
			<-callerCtx.Done()
			ch := make(chan MessageChunk)
			go func() {
				<-sourceRelease
				close(ch)
			}()
			return ch, setupErr
		},
	}
	wrapped := NewTimeoutModelMiddleware(
		WithTotalTimeout(20*time.Millisecond),
		WithStreamChunkTimeout(time.Second),
	).WrapModel(model)
	var cancelCaller context.CancelFunc
	callerCtx, cancelCaller = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelCaller()
	result := make(chan error, 1)
	go func() {
		_, err := wrapped.Stream(callerCtx, nil)
		result <- err
	}()
	<-started
	select {
	case cause := <-observedCause:
		var localAbort *LocalStreamAbortCause
		require.ErrorAs(t, cause, &localAbort)
		require.ErrorIs(t, cause, context.DeadlineExceeded)
	case <-time.After(time.Second):
		t.Fatal("source did not observe the earlier configured deadline")
	}
	<-callerCtx.Done()
	select {
	case err := <-result:
		t.Fatalf("Stream returned before joining the setup-error source: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseSource()
	select {
	case err := <-result:
		var localAbort *LocalStreamAbortCause
		require.ErrorAs(t, err, &localAbort, "the earlier local deadline must remain the first cause")
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorIs(t, err, setupErr, "the late setup error should remain classifiable")
	case <-time.After(time.Second):
		t.Fatal("Stream did not return after joining the setup-error source")
	}
}

func TestTimeoutModelMiddleware_LateCallerDeadlineDoesNotReplaceLocalCauseForNilSetup(t *testing.T) {
	for _, test := range []struct {
		name     string
		setupErr error
	}{
		{name: "nil channel with setup error", setupErr: errors.New("late nil-channel setup error")},
		{name: "nil nil"},
	} {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			observedCause := make(chan error, 1)
			var callerCtx context.Context
			model := &mockModel{
				streamFn: func(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
					close(started)
					<-ctx.Done()
					observedCause <- context.Cause(ctx)
					<-callerCtx.Done()
					return nil, test.setupErr
				},
			}
			wrapped := NewTimeoutModelMiddleware(
				WithTotalTimeout(20*time.Millisecond),
				WithStreamChunkTimeout(time.Second),
			).WrapModel(model)
			var cancelCaller context.CancelFunc
			callerCtx, cancelCaller = context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancelCaller()
			result := make(chan error, 1)
			go func() {
				_, err := wrapped.Stream(callerCtx, nil)
				result <- err
			}()
			<-started
			select {
			case cause := <-observedCause:
				var localAbort *LocalStreamAbortCause
				require.ErrorAs(t, cause, &localAbort)
				require.ErrorIs(t, cause, context.DeadlineExceeded)
			case <-time.After(time.Second):
				t.Fatal("source did not observe the earlier configured deadline")
			}
			<-callerCtx.Done()
			select {
			case err := <-result:
				var localAbort *LocalStreamAbortCause
				require.ErrorAs(t, err, &localAbort, "the earlier local deadline must remain the first cause")
				require.ErrorIs(t, err, context.DeadlineExceeded)
				if test.setupErr != nil {
					require.ErrorIs(t, err, test.setupErr)
				} else {
					require.ErrorContains(t, err, "nil stream")
				}
			case <-time.After(time.Second):
				t.Fatal("Stream did not return after nil setup")
			}
		})
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
