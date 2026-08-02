package production

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// Resilience hardening: additional circuit-breaker and retry edge cases that
// are not exercised by the baseline suites (half-open exhaustion without a
// fallback, success resets the failure streak, default configuration values,
// backoff cap boundaries, and error-classification precedence).

// TestCircuitDefaultsAppliesSensibleDefaults verifies the documented default
// thresholds applied when the config is zero-valued.
func TestCircuitDefaultsAppliesSensibleDefaults(t *testing.T) {
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{})
	require.Equal(t, 5, b.cfg.FailureThreshold)
	require.Equal(t, 30*time.Second, b.cfg.RecoveryTimeout)
	require.Equal(t, 1, b.cfg.HalfOpenMaxCalls)
	require.Equal(t, 1, b.cfg.SuccessThreshold)
	require.Equal(t, CircuitClosed, b.State())
}

// TestCircuitHalfOpenExhaustionWithoutFallback verifies that once the bounded
// HalfOpen probes are consumed and no fallback is wired, further calls are
// refused with ErrCircuitOpen rather than running the wrapped function.
func TestCircuitHalfOpenExhaustionWithoutFallback(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockVal, clock := manualClock()
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 3, // stays in HalfOpen across several successful probes
	}, WithClock(clock))

	sentinel := errors.New("boom")
	_, err := b.Execute(ctx, failFn(sentinel))
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, CircuitOpen, b.State())

	// Enter HalfOpen; the single probe slot is consumed successfully.
	clockVal.Add(int64(2 * time.Second))
	out, err := b.Execute(ctx, okFn("probe-1"))
	require.NoError(t, err)
	require.Equal(t, "probe-1", out)
	require.Equal(t, CircuitHalfOpen, b.State())

	// The slot is exhausted and there is no fallback -> ErrCircuitOpen.
	called := false
	_, err = b.Execute(ctx, func() (any, error) { called = true; return "x", nil })
	require.ErrorIs(t, err, ErrCircuitOpen)
	require.False(t, called, "wrapped fn must not run when half-open slots are exhausted")
}

// TestCircuitSuccessResetsFailureStreak verifies that a single success between
// failures clears the consecutive-failure counter, so the breaker does not open
// prematurely.
func TestCircuitSuccessResetsFailureStreak(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	_, clock := manualClock()
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 3,
		RecoveryTimeout:  time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 1,
	}, WithClock(clock))

	sentinel := errors.New("boom")
	for i := 0; i < 2; i++ {
		_, err := b.Execute(ctx, failFn(sentinel))
		require.ErrorIs(t, err, sentinel)
	}
	// Success resets the streak.
	out, err := b.Execute(ctx, okFn("ok"))
	require.NoError(t, err)
	require.Equal(t, "ok", out)

	// Only two more consecutive failures now, below the threshold of 3.
	for i := 0; i < 2; i++ {
		_, err := b.Execute(ctx, failFn(sentinel))
		require.ErrorIs(t, err, sentinel)
	}
	require.Equal(t, CircuitClosed, b.State(), "2 failures after a reset must not open a threshold-3 breaker")
}

// TestCircuitHalfOpenSuccessAccumulatesToClose verifies that multiple successful
// probes accumulate toward SuccessThreshold before the breaker closes.
func TestCircuitHalfOpenSuccessAccumulatesToClose(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockVal, clock := manualClock()
	boom := errors.New("boom")

	// HalfOpenMaxCalls must be >= SuccessThreshold to allow accumulation.
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Second,
		HalfOpenMaxCalls: 3,
		SuccessThreshold: 3,
	}, WithClock(clock))

	_, err := b.Execute(ctx, failFn(boom))
	require.ErrorIs(t, err, boom)
	require.Equal(t, CircuitOpen, b.State())

	clockVal.Add(int64(2 * time.Second))

	// First probe succeeds -> HalfOpen (not immediately closed).
	out, err := b.Execute(ctx, okFn(1))
	require.NoError(t, err)
	require.Equal(t, 1, out)
	require.Equal(t, CircuitHalfOpen, b.State())

	// Second probe succeeds -> still HalfOpen.
	out, err = b.Execute(ctx, okFn(2))
	require.NoError(t, err)
	require.Equal(t, 2, out)
	require.Equal(t, CircuitHalfOpen, b.State())

	// Third success reaches SuccessThreshold -> Closed.
	out, err = b.Execute(ctx, okFn(3))
	require.NoError(t, err)
	require.Equal(t, 3, out)
	require.Equal(t, CircuitClosed, b.State())
}

// TestCircuitBackwardClockKeepsOpen verifies that a clock that never advances
// (or runs backwards relative to openedAt) keeps the breaker Open, because the
// recovery timeout has not elapsed.
func TestCircuitBackwardClockKeepsOpen(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockVal, clock := manualClock()
	boom := errors.New("boom")
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Second,
	}, WithClock(clock))

	_, err := b.Execute(ctx, failFn(boom))
	require.ErrorIs(t, err, boom)
	require.Equal(t, CircuitOpen, b.State())

	// Advance the clock backwards: still before the recovery timeout.
	clockVal.Add(int64(-5 * time.Second))
	_, err = b.Execute(ctx, okFn("x"))
	require.ErrorIs(t, err, ErrCircuitOpen, "backward clock must keep the breaker open")
}

// TestCircuitConcurrentOpenAndProbe race-hardens an aggressive mixed workload
// that frequently toggles between Open and HalfOpen via the injected clock.
func TestCircuitConcurrentOpenAndProbe(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newCircuitTestCtx(t)
	clockVal, clock := manualClock()
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  50 * time.Millisecond,
		HalfOpenMaxCalls: 2,
		SuccessThreshold: 1,
	}, WithClock(clock), WithFallback(func() (any, error) { return "cached", nil }))

	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				clockVal.Store(int64(i) * int64(10*time.Millisecond))
				if (g+i)%3 == 0 {
					_, _ = b.Execute(ctx, failFn(errors.New("boom"))) //nolint:errcheck // intentional race workload
				} else {
					_, _ = b.Execute(ctx, okFn(i)) //nolint:errcheck // intentional race workload
				}
				_ = b.State()
			}
		}(g)
	}
	wg.Wait()
}

// TestRetryBackoffCapsAtMaxDelay verifies that the exponential backoff clamps
// precisely to MaxDelay at the boundary where 2^attempt would exceed it.
func TestRetryBackoffCapsAtMaxDelay(t *testing.T) {
	ctx := context.Background()
	p := NewDefaultRetryPolicy(RetryConfig{
		BaseDelay: 100 * time.Millisecond,
		MaxDelay:  300 * time.Millisecond, // cap strictly between 2x and 4x
	})

	// attempt 1 -> 200ms (below cap); attempt 2 -> 100*4=400 -> clamps to 300ms.
	require.Equal(t, 200*time.Millisecond, p.NextBackoff(ctx, 1))
	require.Equal(t, 300*time.Millisecond, p.NextBackoff(ctx, 2))
	require.Equal(t, 300*time.Millisecond, p.NextBackoff(ctx, 100))
}

// TestRetryBackoffWithOnlyJitter verifies that a zero BaseDelay with jitter
// still yields a bounded, non-negative delay without panicking.
func TestRetryBackoffWithOnlyJitter(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{MaxDelay: 50 * time.Millisecond})
	ctx := context.Background()
	for attempt := 0; attempt < 5; attempt++ {
		d := p.NextBackoff(ctx, attempt)
		require.GreaterOrEqual(t, d, time.Duration(0))
		require.LessOrEqual(t, d, 50*time.Millisecond)
	}
}

// TestRetryMaxAttemptsZeroDefaults verify the constructor fills defaults.
func TestRetryMaxAttemptsZeroDefaults(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	require.Equal(t, 3, p.cfg.MaxAttempts)
	require.Equal(t, 100*time.Millisecond, p.cfg.BaseDelay)
	require.Equal(t, "default-retry-policy", p.Name())
}

// TestClassifyRateLimitVariants verifies additional rate-limit string shapes are
// classified ErrorRateLimit.
func TestClassifyRateLimitVariants(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	for _, msg := range []string{
		"rate limit exceeded",
		"too many requests, slow down",
		"HTTP 429 Too Many Requests",
		"throttled by the upstream service",
	} {
		require.Equal(t, ErrorRateLimit, p.Classify(errors.New(msg)), "msg=%q", msg)
	}
}

// TestClassifyTimeoutVariants verifies additional timeout/deadline shapes.
func TestClassifyTimeoutVariants(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	for _, msg := range []string{
		"operation timeout occurred",
		"deadline exceeded waiting for worker",
		"context deadline exceeded in the request",
	} {
		require.Equal(t, ErrorTimeout, p.Classify(errors.New(msg)), "msg=%q", msg)
	}
}

// TestClassifyTransientVariants verifies additional transient shapes.
func TestClassifyTransientVariants(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	for _, msg := range []string{
		"transient failure, please retry",
		"temporary outage",
		"connection reset by peer",
		"connection refused",
	} {
		require.Equal(t, ErrorTransient, p.Classify(errors.New(msg)), "msg=%q", msg)
	}
}

// TestClassifyPrecedence verifies the string-match precedence across the switch:
// the first matching branch (rate limit before timeout before transient) wins.
func TestClassifyPrecedence(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	// Contains both "timeout" and "connection reset": timeout wins (checked first).
	require.Equal(t, ErrorTimeout, p.Classify(errors.New("connection reset timeout occurred")))
	// Contains "temporary" and "429": rate limit wins (checked first).
	require.Equal(t, ErrorRateLimit, p.Classify(errors.New("temporary 429 too many requests")))
}

// TestWrappedErrorChainClassification verifies classification descends a
// multi-layer fmt.Errorf %w chain via errContains.
func TestWrappedErrorChainClassification(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	root := errors.New("connection reset occurred")
	mid := newWrappedErr("outer", root)
	outer := newWrappedErr("top", mid)

	require.Equal(t, ErrorTransient, p.Classify(outer))
	// Explicit category on the outermost wrapper must override the deep string.
	require.Equal(t, ErrorFatal, p.Classify(NewError(ErrorFatal, outer)))
}

// newWrappedErr is a tiny helper mirroring fmt.Errorf %w behavior without
// needing a package-scoped named error.
func newWrappedErr(msg string, inner error) error {
	return &chainError{msg: msg, inner: inner}
}

// chainError is a minimal multi-layer error supporting Unwrap for classification
// tests (errContains walks errors.Unwrap, not the inner field directly).
type chainError struct {
	msg   string
	inner error
}

func (e *chainError) Error() string { return e.msg + ": " + e.inner.Error() }
func (e *chainError) Unwrap() error { return e.inner }

// TestCategorizedErrorStringForm verifies the canonical String() helper.
func TestCategorizedErrorStringForm(t *testing.T) {
	require.Equal(t, "transient", ErrorTransient.String())
	require.Equal(t, "rate_limit", ErrorRateLimit.String())
	require.Equal(t, "timeout", ErrorTimeout.String())
	require.Equal(t, "fatal", ErrorFatal.String())
}

// TestRetryShouldRetryEmitNegativeAttempt verifies ShouldRetry treats a negative
// attempt as below MaxAttempts but still classifies the error.
func TestRetryShouldRetryEmitNegativeAttempt(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 2})
	err := NewError(ErrorTransient, errors.New("boom"))
	assert.True(t, p.ShouldRetry(ctx, err, -1))
	assert.False(t, p.ShouldRetry(ctx, err, 2))
}
