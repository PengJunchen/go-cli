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

// mockModel is a configurable BaseChatModel for middleware tests. The
// generateFn and streamFn fields can be set to inject custom behavior.
type mockModel struct {
	generateCalls int32
	streamCalls   int32
	generateFn    func(ctx context.Context, msgs []Message, opts ...Option) (*Message, error)
	streamFn      func(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error)
}

func (m *mockModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	atomic.AddInt32(&m.generateCalls, 1)
	if m.generateFn != nil {
		return m.generateFn(ctx, msgs, opts...)
	}
	return &Message{Role: RoleAssistant, Content: "ok"}, nil
}

func (m *mockModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	atomic.AddInt32(&m.streamCalls, 1)
	if m.streamFn != nil {
		return m.streamFn(ctx, msgs, opts...)
	}
	ch := make(chan MessageChunk, 1)
	ch <- MessageChunk{Role: RoleAssistant, Content: "ok", Final: true}
	close(ch)
	return ch, nil
}

// Compile-time assertion that mockModel satisfies BaseChatModel.
var _ BaseChatModel = (*mockModel)(nil)

// testRetryPolicy is a deterministic RetryPolicy for tests: no jitter, simple
// exponential backoff. maxRetries is the number of retries allowed (not counting
// the initial call).
type testRetryPolicy struct {
	maxRetries int
	baseDelay  time.Duration
}

func (p *testRetryPolicy) ShouldRetry(_ context.Context, _ error, attempt int) bool {
	return attempt < p.maxRetries
}

func (p *testRetryPolicy) NextBackoff(_ context.Context, attempt int) time.Duration {
	d := p.baseDelay
	for i := 0; i < attempt; i++ {
		d *= 2
	}
	return d
}

func (p *testRetryPolicy) Name() string { return "test-retry" }

// Compile-time assertion that testRetryPolicy satisfies RetryPolicy.
var _ RetryPolicy = (*testRetryPolicy)(nil)

// TestRetryModelMiddleware_Name verifies the middleware identifier.
func TestRetryModelMiddleware_Name(t *testing.T) {
	mw := NewRetryModelMiddleware()
	assert.Equal(t, "retry", mw.Name())
}

// TestRetryModelMiddleware_NoRetryOnSuccess verifies that a successful Generate
// call is not retried.
func TestRetryModelMiddleware_NoRetryOnSuccess(t *testing.T) {
	model := &mockModel{}
	mw := NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 3, baseDelay: 1 * time.Millisecond}))
	wrapped := mw.WrapModel(model)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Content)
	assert.Equal(t, int32(1), atomic.LoadInt32(&model.generateCalls))
}

// TestRetryModelMiddleware_RetryOnError verifies that Generate is retried after
// errors and eventually succeeds.
func TestRetryModelMiddleware_RetryOnError(t *testing.T) {
	var calls int32
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			n := atomic.AddInt32(&calls, 1)
			if n < 3 {
				return nil, errors.New("transient error")
			}
			return &Message{Role: RoleAssistant, Content: "recovered"}, nil
		},
	}
	mw := NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 5, baseDelay: 1 * time.Millisecond}))
	wrapped := mw.WrapModel(model)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "recovered", resp.Content)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls))
}

// TestRetryModelMiddleware_MaxAttemptsReached verifies that the middleware gives
// up after maxRetries retries and returns the last error.
func TestRetryModelMiddleware_MaxAttemptsReached(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errors.New("persistent transient error")
		},
	}
	mw := NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 2, baseDelay: 1 * time.Millisecond}))
	wrapped := mw.WrapModel(model)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "persistent transient error")
	// 1 initial + 2 retries = 3 total calls.
	assert.Equal(t, int32(3), atomic.LoadInt32(&model.generateCalls))
}

// TestRetryModelMiddleware_ExponentialBackoff verifies that the backoff duration
// doubles between retries.
func TestRetryModelMiddleware_ExponentialBackoff(t *testing.T) {
	baseDelay := 30 * time.Millisecond
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errors.New("transient error")
		},
	}
	mw := NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 2, baseDelay: baseDelay}))
	wrapped := mw.WrapModel(model)

	start := time.Now()
	_, err := wrapped.Generate(context.Background(), nil)
	elapsed := time.Since(start)

	require.Error(t, err)
	// Expected backoff: attempt 0 -> 30ms, attempt 1 -> 60ms. Total = 90ms.
	assert.GreaterOrEqual(t, elapsed, 90*time.Millisecond, "total backoff should be at least 90ms")
	// Upper bound to allow for scheduling overhead.
	assert.Less(t, elapsed, 300*time.Millisecond, "total backoff should be reasonable")
}

// TestRetryModelMiddleware_StreamRetryOnError verifies that Stream is retried
// when the initial call returns an error.
func TestRetryModelMiddleware_StreamRetryOnError(t *testing.T) {
	var calls int32
	model := &mockModel{
		streamFn: func(_ context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
			n := atomic.AddInt32(&calls, 1)
			if n < 2 {
				return nil, errors.New("transient stream error")
			}
			ch := make(chan MessageChunk, 1)
			ch <- MessageChunk{Role: RoleAssistant, Content: "recovered", Final: true}
			close(ch)
			return ch, nil
		},
	}
	mw := NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 3, baseDelay: 1 * time.Millisecond}))
	wrapped := mw.WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var content string
	for chunk := range ch {
		content += chunk.Content
	}
	assert.Equal(t, "recovered", content)
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
}

// TestRetryModelMiddleware_StreamNoRetryOnSuccess verifies that a successful
// Stream call is not retried.
func TestRetryModelMiddleware_StreamNoRetryOnSuccess(t *testing.T) {
	model := &mockModel{}
	mw := NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 3, baseDelay: 1 * time.Millisecond}))
	wrapped := mw.WrapModel(model)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var content string
	for chunk := range ch {
		content += chunk.Content
	}
	assert.Equal(t, "ok", content)
	assert.Equal(t, int32(1), atomic.LoadInt32(&model.streamCalls))
}

// TestRetryModelMiddleware_ContextCancel verifies that the middleware respects
// context cancellation during the backoff wait.
func TestRetryModelMiddleware_ContextCancel(t *testing.T) {
	model := &mockModel{
		generateFn: func(_ context.Context, _ []Message, _ ...Option) (*Message, error) {
			return nil, errors.New("transient error")
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	mw := NewRetryModelMiddleware(WithRetryPolicy(&testRetryPolicy{maxRetries: 10, baseDelay: 1 * time.Second}))
	wrapped := mw.WrapModel(model)

	// Cancel after a short delay to interrupt the backoff.
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	_, err := wrapped.Generate(ctx, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestRetryModelMiddleware_DefaultPolicy verifies the default policy's
// ShouldRetry and NextBackoff behavior.
func TestRetryModelMiddleware_DefaultPolicy(t *testing.T) {
	p := newDefaultRetryPolicy(RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
	})

	assert.Equal(t, "default-retry", p.Name())
	assert.True(t, p.ShouldRetry(context.Background(), errors.New("err"), 0))
	assert.True(t, p.ShouldRetry(context.Background(), errors.New("err"), 1))
	assert.True(t, p.ShouldRetry(context.Background(), errors.New("err"), 2))
	assert.False(t, p.ShouldRetry(context.Background(), errors.New("err"), 3))
	assert.False(t, p.ShouldRetry(context.Background(), nil, 0))

	// Without jitter, backoff is pure exponential.
	p2 := newDefaultRetryPolicy(RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
	})
	assert.Equal(t, 100*time.Millisecond, p2.NextBackoff(context.Background(), 0))
	assert.Equal(t, 200*time.Millisecond, p2.NextBackoff(context.Background(), 1))
	assert.Equal(t, 400*time.Millisecond, p2.NextBackoff(context.Background(), 2))
}
