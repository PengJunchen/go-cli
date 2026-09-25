package production

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
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
	p := NewDefaultRetryPolicy(RetryConfig{}).(*DefaultRetryPolicy) //nolint:errcheck
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
	p := NewDefaultRetryPolicy(RetryConfig{}).(*DefaultRetryPolicy) //nolint:errcheck
	require.Equal(t, ErrorRateLimit, p.Classify(errors.New("429 too many requests")))
	require.Equal(t, ErrorTimeout, p.Classify(errors.New("context deadline exceeded")))
	require.Equal(t, ErrorTransient, p.Classify(errors.New("connection refused")))
	require.Equal(t, ErrorFatal, p.Classify(errors.New("permission denied")))
}

func TestClassifyWrappedProviderError(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{}).(*DefaultRetryPolicy) //nolint:errcheck
	tests := []struct {
		name   string
		typeOf llm.ErrorType
		status int
		want   ErrorCategory
	}{
		{name: "rate limit", typeOf: llm.ErrTypeRateLimit, status: 429, want: ErrorRateLimit},
		{name: "server", typeOf: llm.ErrTypeServer, status: 503, want: ErrorTransient},
		{name: "network", typeOf: llm.ErrTypeNetwork, want: ErrorTransient},
		{name: "auth", typeOf: llm.ErrTypeAuth, status: 401, want: ErrorFatal},
		{name: "overflow", typeOf: llm.ErrTypeOverflow, status: 400, want: ErrorFatal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &llm.ProviderError{ErrorType: tt.typeOf, StatusCode: tt.status, Provider: "fixture"}
			require.Equal(t, tt.want, p.Classify(fmt.Errorf("provider wrapper: %w", err)))
		})
	}
}

func TestClassifyWrappedTypedFatalBeforeTransientText(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{}).(*DefaultRetryPolicy) //nolint:errcheck

	providerErr := fmt.Errorf("connection reset while wrapping: %w", &llm.ProviderError{
		ErrorType:  llm.ErrTypeAuth,
		Provider:   "fixture",
		StatusCode: 401,
	})
	categorizedErr := fmt.Errorf("temporary wrapper: %w", NewError(ErrorFatal, errors.New("denied")))

	require.Equal(t, ErrorFatal, p.Classify(providerErr))
	require.Equal(t, ErrorFatal, p.Classify(categorizedErr))
}

func TestClassifyNormalizedProviderErrorBeforeGenericHarnessCode(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 3}).(*DefaultRetryPolicy) //nolint:errcheck
	tests := []struct {
		name       string
		typeOf     llm.ErrorType
		statusCode int
		wrapText   string
		wantCode   string
		want       ErrorCategory
		wantRetry  bool
	}{
		{name: "auth", typeOf: llm.ErrTypeAuth, statusCode: 401, wrapText: "temporary connection reset", wantCode: core.ErrCodeModelError, want: ErrorFatal},
		{name: "overflow", typeOf: llm.ErrTypeOverflow, statusCode: 400, wrapText: "temporary connection reset", wantCode: core.ErrCodeModelError, want: ErrorFatal},
		{name: "rate limit", typeOf: llm.ErrTypeRateLimit, statusCode: 429, wrapText: "temporary connection reset", wantCode: core.ErrCodeModelError, want: ErrorRateLimit, wantRetry: true},
		{name: "server", typeOf: llm.ErrTypeServer, statusCode: 503, wrapText: "temporary connection reset", wantCode: core.ErrCodeModelError, want: ErrorTransient, wantRetry: true},
		{name: "tool auth", typeOf: llm.ErrTypeAuth, statusCode: 401, wrapText: "tool execution failed", wantCode: core.ErrCodeToolError, want: ErrorFatal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providerErr := &llm.ProviderError{
				ErrorType:  tt.typeOf,
				StatusCode: tt.statusCode,
				Provider:   "fixture",
				Message:    "temporary connection reset",
			}
			normalized := core.NormalizeError(fmt.Errorf("%s: %w", tt.wrapText, providerErr))
			require.Equal(t, tt.wantCode, normalized.Code, "normalization keeps the generic harness code")
			require.Equal(t, tt.want, p.Classify(normalized))
			require.Equal(t, tt.wantRetry, p.ShouldRetry(ctx, normalized, 0))
		})
	}
}

func TestClassifyNormalizedHarnessTerminalCodeOverridesProviderCause(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 3}).(*DefaultRetryPolicy) //nolint:errcheck
	tests := []struct {
		name    string
		message string
		code    string
	}{
		{name: "busy", message: "harness is busy", code: core.ErrCodeBusy},
		{name: "hook rejected", message: "hook rejected the run", code: core.ErrCodeHookRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serverErr := &llm.ProviderError{
				ErrorType: llm.ErrTypeServer,
				Provider:  "fixture",
				Message:   "server unavailable",
			}
			normalized := core.NormalizeError(fmt.Errorf("%s: %w", tt.message, serverErr))
			require.Equal(t, tt.code, normalized.Code)
			require.Equal(t, ErrorFatal, p.Classify(normalized))
			require.False(t, p.ShouldRetry(ctx, normalized, 0))
		})
	}
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
	require.False(t, p.ShouldRetry(ctx, err, 2))
	require.False(t, p.ShouldRetry(ctx, err, 3))
	require.False(t, p.ShouldRetry(ctx, err, 10))
	require.False(t, p.ShouldRetry(ctx, err, -1))
}

func TestShouldRetryRefusesCanceledContextAndErrors(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 3})

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, p.ShouldRetry(canceledCtx, NewError(ErrorTransient, errors.New("temporary")), 0))
	require.False(t, p.ShouldRetry(context.Background(), fmt.Errorf("wrapped: %w", context.Canceled), 0))
	require.False(t, p.ShouldRetry(context.Background(), fmt.Errorf("wrapped: %w", context.DeadlineExceeded), 0))
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
	rp := NewDefaultRetryPolicy(RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    2 * time.Second,
		Jitter:      0, // deterministic
	})
	p := rp.(*DefaultRetryPolicy) //nolint:errcheck

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
	rp := NewDefaultRetryPolicy(RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
		Jitter:      25 * time.Millisecond,
	})
	p := rp.(*DefaultRetryPolicy) //nolint:errcheck

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
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 3, BaseDelay: 1 * time.Millisecond}).(*DefaultRetryPolicy) //nolint:errcheck

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

func TestCategorizedErrorUnwrap(t *testing.T) {
	sentinel := errors.New("root cause")
	wrapped := NewError(ErrorTransient, sentinel)

	require.True(t, errors.Is(wrapped, sentinel))
	require.Equal(t, "root cause", wrapped.Error())

	inner := errors.New("inner")
	mid := NewError(ErrorTransient, inner)
	outer := NewError(ErrorFatal, mid)
	require.True(t, errors.Is(outer, inner), "errors.Is must traverse nested CategorizedError wrappers")
}

func TestNewErrorNilReturnsNil(t *testing.T) {
	require.Nil(t, NewError(ErrorTransient, nil))
}

func TestClassifyHarnessError(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{}).(*DefaultRetryPolicy) //nolint:errcheck
	cases := []struct {
		code string
		want ErrorCategory
	}{
		{core.ErrCodeBusy, ErrorFatal},
		{core.ErrCodeHookRejected, ErrorFatal},
		{core.ErrCodeTimeout, ErrorTimeout},
		{core.ErrCodeModelError, ErrorTransient},
		{core.ErrCodeToolError, ErrorTransient},
	}
	for _, tc := range cases {
		err := &core.AgentHarnessError{Code: tc.code, Message: "test"}
		assert.Equal(t, tc.want, p.Classify(err), "code %s should map to %s", tc.code, tc.want)
	}
}

func TestNoRetryOnRejected(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 5})
	err := &core.AgentHarnessError{Code: core.ErrCodeHookRejected, Message: "hook rejected"}
	require.False(t, p.ShouldRetry(ctx, err, 0))
}

func TestRetryOnTransient(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 5})
	err := &core.AgentHarnessError{Code: core.ErrCodeModelError, Message: "model error"}
	require.True(t, p.ShouldRetry(ctx, err, 0))
}
