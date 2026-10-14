package production

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func beginCircuitStream(t *testing.T, breaker CircuitBreaker) CircuitBreakerStreamTicket {
	t.Helper()
	streaming, ok := breaker.(StreamingCircuitBreaker)
	require.True(t, ok)
	ticket, err := streaming.BeginStream(context.Background())
	require.NoError(t, err)
	require.NotNil(t, ticket)
	return ticket
}

func TestCircuitOpenExecuteWithoutFallbackKeepsGenericFallbackCompatibility(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	var fallbackCalls atomic.Int32
	breaker := NewDefaultCircuitBreaker(baseCircuitConfig(), WithFallback(func() (any, error) {
		fallbackCalls.Add(1)
		return "cached", nil
	}))
	openBreaker(ctx, t, breaker, baseCircuitConfig(), errors.New("provider failed"))

	called := false
	out, err := breaker.Execute(ctx, func() (any, error) {
		called = true
		return "real", nil
	})
	require.NoError(t, err)
	require.Equal(t, "cached", out)
	require.False(t, called)

	strict := breaker.(CircuitBreakerWithoutFallback)
	called = false
	out, err = strict.ExecuteWithoutFallback(ctx, func() (any, error) {
		called = true
		return "real", nil
	})
	require.Nil(t, out)
	require.ErrorIs(t, err, ErrCircuitOpen)
	require.False(t, called)
	require.EqualValues(t, 1, fallbackCalls.Load())
}

func TestCircuitStreamAdmissionNeverUsesFallbackWhenOpen(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	var fallbackCalls atomic.Int32
	breaker := NewDefaultCircuitBreaker(baseCircuitConfig(), WithFallback(func() (any, error) {
		fallbackCalls.Add(1)
		return "cached", nil
	}))
	openBreaker(ctx, t, breaker, baseCircuitConfig(), errors.New("provider failed"))

	streaming := breaker.(StreamingCircuitBreaker)
	ticket, err := streaming.BeginStream(ctx)
	require.Nil(t, ticket)
	require.ErrorIs(t, err, ErrCircuitOpen)
	require.Zero(t, fallbackCalls.Load())
}

func TestCircuitStreamHalfOpenProbeStaysUntilCompletion(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockValue, clock := manualClock()
	cfg := CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 1,
	}
	breaker := NewDefaultCircuitBreaker(cfg, WithClock(clock))
	openBreaker(ctx, t, breaker, cfg, errors.New("provider failed"))
	clockValue.Add(int64(time.Second))
	streaming := breaker.(StreamingCircuitBreaker)

	first, err := streaming.BeginStream(ctx)
	require.NoError(t, err)
	second, err := streaming.BeginStream(ctx)
	require.Nil(t, second)
	require.ErrorIs(t, err, ErrCircuitOpen)
	require.Equal(t, CircuitHalfOpen, breaker.State())

	require.True(t, first.Complete(StreamSucceeded))
	require.Equal(t, CircuitClosed, breaker.State())
}

func TestCircuitStreamConcurrentHalfOpenFailureInvalidatesOtherTickets(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockValue, clock := manualClock()
	cfg := CircuitBreakerConfig{FailureThreshold: 1, RecoveryTimeout: time.Second, HalfOpenMaxCalls: 2}
	breaker := NewDefaultCircuitBreaker(cfg, WithClock(clock))
	openBreaker(ctx, t, breaker, cfg, errors.New("provider failed"))
	clockValue.Add(int64(time.Second))
	streaming := breaker.(StreamingCircuitBreaker)
	first, err := streaming.BeginStream(ctx)
	require.NoError(t, err)
	second, err := streaming.BeginStream(ctx)
	require.NoError(t, err)
	third, err := streaming.BeginStream(ctx)
	require.Nil(t, third)
	require.ErrorIs(t, err, ErrCircuitOpen)

	require.True(t, first.Complete(StreamFailed))
	require.Equal(t, CircuitOpen, breaker.State())
	require.False(t, second.Complete(StreamSucceeded), "an older probe must not close a later Open generation")
	require.Equal(t, CircuitOpen, breaker.State())
}

func TestCircuitStreamNeutralHalfOpenCompletionReleasesProbeSlot(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockValue, clock := manualClock()
	cfg := CircuitBreakerConfig{FailureThreshold: 1, RecoveryTimeout: time.Second, HalfOpenMaxCalls: 1}
	breaker := NewDefaultCircuitBreaker(cfg, WithClock(clock))
	openBreaker(ctx, t, breaker, cfg, errors.New("provider failed"))
	clockValue.Add(int64(time.Second))
	streaming := breaker.(StreamingCircuitBreaker)

	first, err := streaming.BeginStream(ctx)
	require.NoError(t, err)
	require.True(t, first.Complete(StreamCanceledNeutral))
	second, err := streaming.BeginStream(ctx)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Equal(t, CircuitHalfOpen, breaker.State())
	require.True(t, second.Complete(StreamSucceeded))
	require.Equal(t, CircuitClosed, breaker.State())
}

func TestCircuitStreamUsesHalfOpenSuccessThreshold(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockValue, clock := manualClock()
	cfg := CircuitBreakerConfig{FailureThreshold: 1, RecoveryTimeout: time.Second, HalfOpenMaxCalls: 2, SuccessThreshold: 2}
	breaker := NewDefaultCircuitBreaker(cfg, WithClock(clock))
	openBreaker(ctx, t, breaker, cfg, errors.New("provider failed"))
	clockValue.Add(int64(time.Second))
	streaming := breaker.(StreamingCircuitBreaker)
	first, err := streaming.BeginStream(ctx)
	require.NoError(t, err)
	second, err := streaming.BeginStream(ctx)
	require.NoError(t, err)

	require.True(t, first.Complete(StreamSucceeded))
	require.Equal(t, CircuitHalfOpen, breaker.State(), "one success is below the configured probe threshold")
	require.True(t, second.Complete(StreamSucceeded))
	require.Equal(t, CircuitClosed, breaker.State())
}

func TestCircuitStreamNeutralOutcomeDoesNotResetClosedFailureCount(t *testing.T) {
	breaker := NewDefaultCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 2})
	first := beginCircuitStream(t, breaker)
	require.True(t, first.Complete(StreamFailed))
	neutral := beginCircuitStream(t, breaker)
	require.True(t, neutral.Complete(StreamCanceledNeutral))
	last := beginCircuitStream(t, breaker)
	require.True(t, last.Complete(StreamFailed))
	require.Equal(t, CircuitOpen, breaker.State(), "neutral cancellation must not clear consecutive failures")
}

func TestCircuitStreamTicketsAreOneShotAndEpochBound(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	breaker := NewDefaultCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 1})
	first := beginCircuitStream(t, breaker)
	stale := beginCircuitStream(t, breaker)

	require.True(t, first.Complete(StreamFailed))
	require.Equal(t, CircuitOpen, breaker.State())
	require.False(t, first.Complete(StreamSucceeded), "duplicate completion must be ignored")
	require.False(t, stale.Complete(StreamSucceeded), "a completion from the older closed generation must not change the open generation")
	require.Equal(t, CircuitOpen, breaker.State())

	require.NoError(t, breaker.Reset(ctx))
	fresh := beginCircuitStream(t, breaker)
	require.NoError(t, breaker.Reset(ctx))
	require.False(t, fresh.Complete(StreamFailed), "Reset invalidates tickets admitted before it")
	require.Equal(t, CircuitClosed, breaker.State())
}

func TestCircuitStreamConcurrentFailuresCountUnderOneGeneration(t *testing.T) {
	breaker := NewDefaultCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 12})
	const count = 24
	tickets := make([]CircuitBreakerStreamTicket, count)
	for i := range tickets {
		tickets[i] = beginCircuitStream(t, breaker)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, ticket := range tickets {
		wg.Add(1)
		go func(ticket CircuitBreakerStreamTicket) {
			defer wg.Done()
			<-start
			ticket.Complete(StreamFailed)
		}(ticket)
	}
	close(start)
	wg.Wait()

	require.Equal(t, CircuitOpen, breaker.State())
	for _, ticket := range tickets {
		require.False(t, ticket.Complete(StreamFailed), "every ticket must remain one-shot after concurrent completion")
	}
}
