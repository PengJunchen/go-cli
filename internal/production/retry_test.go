package production

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// newRetryTestCtx wires a MockTraceExporter + Tracer into a root context.
func newRetryTestCtx(t *testing.T) (context.Context, *mock.MockTraceExporter) {
	t.Helper()
	exporter := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("retry-trace", exporter)
	_, ctx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)
	return ctx, exporter
}

func TestClassifyCategorizedError(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	transient := NewError(ErrorTransient, errors.New("connection reset"))
	require.Equal(t, ErrorTransient, p.Classify(transient))

	rate := NewError(ErrorRateLimit, errors.New("throttled"))
	require.Equal(t, ErrorRateLimit, p.Classify(rate))

	timeout := NewError(ErrorTimeout, errors.New("slow"))
	require.Equal(t, ErrorTimeout, p.Classify(timeout))

	fatal := NewError(ErrorFatal, errors.New("denied"))
	require.Equal(t, ErrorFatal, p.Classify(fatal))

	require.Equal(t, ErrorFatal, p.Classify(nil))
}

func TestClassifyStringMatching(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	require.Equal(t, ErrorRateLimit, p.Classify(errors.New("429 too many requests")))
	require.Equal(t, ErrorTimeout, p.Classify(errors.New("context deadline exceeded")))
	require.Equal(t, ErrorTransient, p.Classify(errors.New("connection refused")))
	require.Equal(t, ErrorFatal, p.Classify(errors.New("permission denied")))
}

func TestErrorCategoryString(t *testing.T) {
	require.Equal(t, "transient", ErrorTransient.String())
	require.Equal(t, "rate_limit", ErrorRateLimit.String())
	require.Equal(t, "timeout", ErrorTimeout.String())
	require.Equal(t, "fatal", ErrorFatal.String())
}

func TestShouldRetryRespectsMaxAttempts(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 3})

	err := NewError(ErrorTransient, errors.New("boom"))
	require.True(t, p.ShouldRetry(ctx, err, 0))
	require.True(t, p.ShouldRetry(ctx, err, 1))
	require.True(t, p.ShouldRetry(ctx, err, 2))
	require.False(t, p.ShouldRetry(ctx, err, 3))
	require.False(t, p.ShouldRetry(ctx, err, 10))
}

func TestShouldRetryFatalNeverRetries(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 5})

	require.False(t, p.ShouldRetry(ctx, NewError(ErrorFatal, errors.New("denied")), 0))
}

func TestShouldRetryRetryableCategories(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 3})

	require.True(t, p.ShouldRetry(ctx, NewError(ErrorTransient, errors.New("x")), 0))
	require.True(t, p.ShouldRetry(ctx, NewError(ErrorRateLimit, errors.New("x")), 0))
	require.True(t, p.ShouldRetry(ctx, NewError(ErrorTimeout, errors.New("x")), 0))
}

func TestNextBackoffExponentialAndCapped(t *testing.T) {
	ctx := context.Background()
	p := NewDefaultRetryPolicy(RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    2 * time.Second,
		Jitter:      0, // deterministic
	})

	d0 := p.NextBackoff(ctx, 0)
	d1 := p.NextBackoff(ctx, 1)
	d2 := p.NextBackoff(ctx, 2)
	d3 := p.NextBackoff(ctx, 3)
	d4 := p.NextBackoff(ctx, 4)

	require.Equal(t, 100*time.Millisecond, d0)
	require.Equal(t, 200*time.Millisecond, d1)
	require.Equal(t, 400*time.Millisecond, d2)
	require.Equal(t, 800*time.Millisecond, d3)
	// 100ms * 2^4 = 1.6s below cap; 100ms*2^5 would cap but we only go to 4.
	require.Equal(t, 1600*time.Millisecond, d4)
	require.LessOrEqual(t, d4, p.cfg.MaxDelay)

	// attempt beyond the cap clamps to MaxDelay.
	dBig := p.NextBackoff(ctx, 20)
	require.Equal(t, 2*time.Second, dBig)
}

func TestNextBackoffNonNegativeWithJitter(t *testing.T) {
	ctx := context.Background()
	p := NewDefaultRetryPolicy(RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
		Jitter:      25 * time.Millisecond,
	})

	for attempt := 0; attempt < 10; attempt++ {
		d := p.NextBackoff(ctx, attempt)
		require.GreaterOrEqual(t, d, time.Duration(0))
		require.LessOrEqual(t, d, p.cfg.MaxDelay)
	}
}

func TestNextBackoffNegativeAttemptClamped(t *testing.T) {
	ctx := context.Background()
	p := NewDefaultRetryPolicy(RetryConfig{BaseDelay: 10 * time.Millisecond})
	require.GreaterOrEqual(t, p.NextBackoff(ctx, -5), time.Duration(0))
}

func TestRetryPolicyName(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	require.Equal(t, "default-retry-policy", p.Name())
	require.Equal(t, "custom-retry", NewDefaultRetryPolicy(RetryConfig{}, WithName("custom-retry")).Name())
}

func TestShouldRetryEmitsSpan(t *testing.T) {
	ctx, exporter := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 3})

	require.True(t, p.ShouldRetry(ctx, NewError(ErrorTransient, errors.New("boom")), 0))

	require.Eventually(t, func() bool {
		return exporter.SpanCount() >= 1
	}, time.Second, 5*time.Millisecond, "expected retry span to be exported")
	exporter.AssertSpanExists(t, "production.retry")
}

func TestRetryPolicyConcurrent(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 3, BaseDelay: 1 * time.Millisecond})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = p.ShouldRetry(ctx, NewError(ErrorRateLimit, errors.New("boom")), i%4)
				_ = p.NextBackoff(ctx, i%5)
				_ = p.Classify(errors.New("boom"))
			}
		}()
	}
	wg.Wait()
}
