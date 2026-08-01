package production

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// newCircuitTestCtx wires a MockTraceExporter + Tracer into a root context and
// returns the derived context and exporter.
func newCircuitTestCtx(t *testing.T) (context.Context, *mock.MockTraceExporter) {
	t.Helper()
	exporter := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("circuit-trace", exporter)
	_, ctx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)
	return ctx, exporter
}

// manualClock returns a now func backed by a nanosecond-settable atomic counter.
func manualClock() (*atomic.Int64, func() time.Time) {
	var v atomic.Int64
	return &v, func() time.Time { return time.Unix(0, v.Load()) }
}

// baseCircuitConfig returns a fast, deterministic breaker configuration.
func baseCircuitConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryTimeout:  1 * time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 1,
	}
}

func failFn(err error) func() (any, error) {
	return func() (any, error) { return nil, err }
}

func okFn(val any) func() (any, error) {
	return func() (any, error) { return val, nil }
}

// openBreaker drives the breaker to the Open state by failing it past
// FailureThreshold, asserting each failure returns the sentinel error.
func openBreaker(ctx context.Context, t *testing.T, b *DefaultCircuitBreaker, sentinel error) {
	t.Helper()
	for i := 0; i < b.cfg.FailureThreshold; i++ {
		_, err := b.Execute(ctx, failFn(sentinel))
		require.ErrorIs(t, err, sentinel)
	}
	require.Equal(t, CircuitOpen, b.State())
}

func TestCircuitClosedToOpenOnFailures(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	_, clock := manualClock()
	b := NewDefaultCircuitBreaker(baseCircuitConfig(), WithClock(clock))

	sentinel := errors.New("boom")
	for i := 0; i < 2; i++ {
		_, err := b.Execute(ctx, failFn(sentinel))
		require.ErrorIs(t, err, sentinel)
	}
	require.Equal(t, CircuitOpen, b.State())
}

func TestCircuitOpenRefusesWithoutFallback(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	_, clock := manualClock()
	b := NewDefaultCircuitBreaker(baseCircuitConfig(), WithClock(clock))

	sentinel := errors.New("boom")
	openBreaker(ctx, t, b, sentinel)

	called := false
	_, err := b.Execute(ctx, func() (any, error) { called = true; return "ignored", nil })
	require.ErrorIs(t, err, ErrCircuitOpen)
	require.False(t, called, "the wrapped fn must not run while the breaker is open")
}

func TestCircuitOpenUsesFallback(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	_, clock := manualClock()
	b := NewDefaultCircuitBreaker(baseCircuitConfig(), WithClock(clock), WithFallback(func() (any, error) {
		return "cached", nil
	}))

	sentinel := errors.New("boom")
	openBreaker(ctx, t, b, sentinel)

	called := false
	out, err := b.Execute(ctx, func() (any, error) { called = true; return "real", nil })
	require.NoError(t, err)
	require.Equal(t, "cached", out)
	require.False(t, called, "fallback must be used instead of the wrapped fn when open")
}

func TestCircuitRecoveryToHalfOpenThenClosed(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockVal, clock := manualClock()
	b := NewDefaultCircuitBreaker(baseCircuitConfig(), WithClock(clock))

	sentinel := errors.New("boom")
	openBreaker(ctx, t, b, sentinel)

	// Advance past RecoveryTimeout and issue a successful probe.
	clockVal.Add(int64(2 * time.Second))
	out, err := b.Execute(ctx, okFn("recovered"))
	require.NoError(t, err)
	require.Equal(t, "recovered", out)

	// SuccessThreshold=1 in HalfOpen -> Closed.
	require.Equal(t, CircuitClosed, b.State())
}

func TestCircuitHalfOpenFailureReopens(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockVal, clock := manualClock()
	b := NewDefaultCircuitBreaker(baseCircuitConfig(), WithClock(clock))

	sentinel := errors.New("boom")
	openBreaker(ctx, t, b, sentinel)

	// Advance into HalfOpen but the probe fails -> back to Open.
	clockVal.Add(int64(2 * time.Second))
	_, err := b.Execute(ctx, failFn(sentinel))
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, CircuitOpen, b.State())

	// Still open: a call is refused.
	_, err = b.Execute(ctx, okFn("nope"))
	require.ErrorIs(t, err, ErrCircuitOpen)
}

func TestCircuitResetReturnsToClosed(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	b := NewDefaultCircuitBreaker(baseCircuitConfig())

	sentinel := errors.New("boom")
	openBreaker(ctx, t, b, sentinel)

	require.NoError(t, b.Reset(ctx))
	require.Equal(t, CircuitClosed, b.State())

	out, err := b.Execute(ctx, okFn("fresh"))
	require.NoError(t, err)
	require.Equal(t, "fresh", out)
}

func TestCircuitName(t *testing.T) {
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{})
	require.Equal(t, "circuit-breaker", b.Name())
	require.Equal(t, "custom-cb", NewDefaultCircuitBreaker(CircuitBreakerConfig{}, WithName("custom-cb")).Name())
}

func TestCircuitEmitsSpan(t *testing.T) {
	ctx, exporter := newCircuitTestCtx(t)
	b := NewDefaultCircuitBreaker(baseCircuitConfig())

	sentinel := errors.New("boom")
	openBreaker(ctx, t, b, sentinel)

	require.Eventually(t, func() bool {
		return exporter.SpanCount() >= 1
	}, time.Second, 5*time.Millisecond, "expected circuit span to be exported")
	exporter.AssertSpanExists(t, "production.circuit")
}

func TestCircuitWrapGenericAdapter(t *testing.T) {
	// Demonstrates the llm-decoupled contract: the breaker wraps any
	// func()(any, error) (e.g. a model call) without knowing the provider.
	ctx, _ := newCircuitTestCtx(t)
	b := NewDefaultCircuitBreaker(baseCircuitConfig())

	gen := func(payload string) (any, error) { return "model:" + payload, nil }
	breakerAdapted := func() (any, error) { return gen("hi") }

	out, err := b.Execute(ctx, breakerAdapted)
	require.NoError(t, err)
	require.Equal(t, "model:hi", out)
}

func TestCircuitConcurrent(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	_, clock := manualClock()
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 20,
		RecoveryTimeout:  time.Hour,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 1,
	}, WithClock(clock))

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				var err error
				if (g+i)%3 == 0 {
					_, err = b.Execute(ctx, failFn(errors.New("boom")))
				} else {
					_, err = b.Execute(ctx, okFn(i))
				}
				_ = err
				_ = b.State()
			}
		}(g)
	}
	wg.Wait()
}
