package production

import (
	"context"
	"sync/atomic"
)

// CircuitBreakerWithoutFallback is an optional CircuitBreaker capability for
// callers that must observe an open-circuit refusal directly. Execute keeps
// its configured fallback behavior for existing generic callers.
type CircuitBreakerWithoutFallback interface {
	// ExecuteWithoutFallback runs fn subject to the breaker state, returning
	// ErrCircuitOpen instead of invoking a configured fallback when refused.
	ExecuteWithoutFallback(ctx context.Context, fn func() (any, error)) (any, error)
}

// ExecuteWithoutFallback runs fn like Execute but returns ErrCircuitOpen when
// admission is refused, even if a generic fallback is configured.
func (b *DefaultCircuitBreaker) ExecuteWithoutFallback(ctx context.Context, fn func() (any, error)) (any, error) {
	return b.execute(ctx, fn, false)
}

// StreamOutcome is the terminal classification of one admitted stream call.
type StreamOutcome uint8

const (
	// StreamSucceeded records one valid stream after its source has joined.
	StreamSucceeded StreamOutcome = iota + 1
	// StreamFailed records one provider or local stream failure after joining.
	StreamFailed
	// StreamCanceledNeutral releases admission without changing outcome counts.
	StreamCanceledNeutral
)

// CircuitBreakerStreamTicket settles one streaming admission at most once.
type CircuitBreakerStreamTicket interface {
	// Complete returns true only for the first completion in the admission's
	// breaker generation. Duplicate or stale completions return false.
	Complete(StreamOutcome) bool
}

// StreamingCircuitBreaker is an optional streaming capability for a
// CircuitBreaker. Stream admissions never invoke the generic fallback.
type StreamingCircuitBreaker interface {
	// BeginStream admits one logical stream call or returns a typed refusal.
	BeginStream(context.Context) (CircuitBreakerStreamTicket, error)
}

type defaultCircuitBreakerStreamTicket struct {
	breaker    *DefaultCircuitBreaker
	generation uint64
	ctx        context.Context
	completed  atomic.Bool
}

// BeginStream admits one stream against the existing breaker state machine.
// A HalfOpen admission consumes one bounded probe slot until it completes;
// neutral cancellation releases that slot for another probe.
func (b *DefaultCircuitBreaker) BeginStream(ctx context.Context) (CircuitBreakerStreamTicket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if b.state == CircuitOpen {
		if b.cfg.RecoveryTimeout <= 0 || b.now().Sub(b.openedAt) < b.cfg.RecoveryTimeout {
			return nil, ErrCircuitOpen
		}
		b.transitionLocked(ctx, CircuitHalfOpen)
		b.halfOpenCalls = 0
		b.halfOpenSuccesses = 0
	}

	if b.state == CircuitHalfOpen {
		if b.halfOpenCalls >= b.cfg.HalfOpenMaxCalls {
			return nil, ErrCircuitOpen
		}
		b.halfOpenCalls++
	}

	return &defaultCircuitBreakerStreamTicket{
		breaker:    b,
		generation: b.generation,
		ctx:        ctx,
	}, nil
}

func (t *defaultCircuitBreakerStreamTicket) Complete(outcome StreamOutcome) bool {
	if t == nil || t.breaker == nil {
		return false
	}
	switch outcome {
	case StreamSucceeded, StreamFailed, StreamCanceledNeutral:
	default:
		return false
	}
	if !t.completed.CompareAndSwap(false, true) {
		return false
	}

	b := t.breaker
	b.mu.Lock()
	defer b.mu.Unlock()
	if t.generation != b.generation {
		return false
	}

	switch b.state {
	case CircuitClosed:
		switch outcome {
		case StreamFailed:
			b.consecutiveFailures++
			if b.consecutiveFailures >= b.cfg.FailureThreshold {
				b.transitionLocked(t.ctx, CircuitOpen)
				b.openedAt = b.now()
				b.halfOpenCalls = 0
				b.halfOpenSuccesses = 0
			}
		case StreamSucceeded:
			b.consecutiveFailures = 0
		}
		return true
	case CircuitHalfOpen:
		switch outcome {
		case StreamFailed:
			b.transitionLocked(t.ctx, CircuitOpen)
			b.openedAt = b.now()
			b.halfOpenCalls = 0
			b.halfOpenSuccesses = 0
		case StreamSucceeded:
			b.halfOpenSuccesses++
			if b.halfOpenSuccesses >= b.cfg.SuccessThreshold {
				b.transitionLocked(t.ctx, CircuitClosed)
				b.consecutiveFailures = 0
				b.halfOpenCalls = 0
				b.halfOpenSuccesses = 0
			}
		case StreamCanceledNeutral:
			if b.halfOpenCalls > 0 {
				b.halfOpenCalls--
			}
		}
		return true
	default:
		return false
	}
}
