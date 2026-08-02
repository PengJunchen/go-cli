package production

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// CircuitState is the current state of a CircuitBreaker.
type CircuitState string

// CircuitBreaker states forming the three-state machine.
const (
	// CircuitClosed allows all calls; consecutive failures eventually open it.
	CircuitClosed CircuitState = "closed"
	// CircuitOpen refuses or falls back calls until the recovery timeout elapses.
	CircuitOpen CircuitState = "open"
	// CircuitHalfOpen probes a bounded number of calls to decide whether to
	// close again or reopen.
	CircuitHalfOpen CircuitState = "half_open"
)

// String returns the canonical string form of the state.
func (s CircuitState) String() string { return string(s) }

// CircuitBreakerConfig tunes the three-state transition thresholds.
type CircuitBreakerConfig struct {
	// FailureThreshold is the number of consecutive failures in Closed state
	// that transition the breaker to Open.
	FailureThreshold int
	// RecoveryTimeout is how long the breaker stays Open before probing.
	RecoveryTimeout time.Duration
	// HalfOpenMaxCalls bounds the calls permitted while probing in HalfOpen.
	HalfOpenMaxCalls int
	// SuccessThreshold is the consecutive successes in HalfOpen needed to
	// transition back to Closed.
	SuccessThreshold int
}

// ErrCircuitOpen is returned by Execute when the breaker is Open and no
// fallback is configured.
var ErrCircuitOpen = errors.New("circuit breaker is open")

// CircuitBreaker guards an arbitrary callable, refusing (or falling back) once
// repeated failures indicate the dependency is unhealthy.
type CircuitBreaker interface {
	// Execute runs fn subject to the current breaker state (Closed or HalfOpen)
	// or refuses/falls back when Open.
	Execute(ctx context.Context, fn func() (any, error)) (any, error)
	// State returns the current breaker state.
	State() CircuitState
	// Reset returns the breaker to Closed and clears its failure counters.
	Reset(ctx context.Context) error
	// Name returns the breaker identifier.
	Name() string
}

// DefaultCircuitBreaker is a three-state (Closed / Open / HalfOpen) circuit
// breaker around a generic func()(any, error) callable.
type DefaultCircuitBreaker struct {
	mu   sync.RWMutex
	cfg  CircuitBreakerConfig
	now  func() time.Time
	name string

	state               CircuitState
	consecutiveFailures int
	halfOpenCalls       int
	halfOpenSuccesses   int
	openedAt            time.Time
	fallback            func() (any, error)
}

// Compile-time assertion that DefaultCircuitBreaker satisfies CircuitBreaker.
var _ CircuitBreaker = (*DefaultCircuitBreaker)(nil)

// NewDefaultCircuitBreaker returns a DefaultCircuitBreaker backed by cfg with
// sensible defaults for zero-valued fields. It starts in the Closed state.
func NewDefaultCircuitBreaker(cfg CircuitBreakerConfig, opts ...Option) CircuitBreaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.RecoveryTimeout <= 0 {
		cfg.RecoveryTimeout = 30 * time.Second
	}
	if cfg.HalfOpenMaxCalls <= 0 {
		cfg.HalfOpenMaxCalls = 1
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = 1
	}

	o := applyOptions(opts)
	now := o.now
	if now == nil {
		now = time.Now
	}
	name := o.name
	if name == "" {
		name = "circuit-breaker"
	}

	return &DefaultCircuitBreaker{
		cfg:      cfg,
		now:      now,
		name:     name,
		state:    CircuitClosed,
		fallback: o.fallback,
	}
}

// Execute runs fn in the Closed or HalfOpen state, and refuses (or invokes the
// configured fallback, when provided) while Open. It records the outcome to
// drive state transitions.
func (b *DefaultCircuitBreaker) Execute(ctx context.Context, fn func() (any, error)) (any, error) {
	b.mu.Lock()

	allowed, fallback := b.allowLocked(ctx)
	b.mu.Unlock()

	if !allowed {
		if fallback != nil {
			return fallback()
		}
		return nil, ErrCircuitOpen
	}

	result, err := fn()
	b.record(ctx, err)
	return result, err
}

// allowLocked decides whether a call may run given the current state, applying
// timed transitions (Open -> HalfOpen) while holding b.mu. It returns the
// fallback (nil if none) when the call is refused.
func (b *DefaultCircuitBreaker) allowLocked(ctx context.Context) (bool, func() (any, error)) {
	switch b.state {
	case CircuitOpen:
		if b.cfg.RecoveryTimeout > 0 && b.now().Sub(b.openedAt) >= b.cfg.RecoveryTimeout {
			b.transitionLocked(ctx, CircuitHalfOpen)
			b.halfOpenCalls = 1
			b.halfOpenSuccesses = 0
			return true, nil
		}
		if b.fallbackEnabled() {
			return false, b.fallbackFn()
		}
		return false, nil
	case CircuitHalfOpen:
		if b.halfOpenCalls >= b.cfg.HalfOpenMaxCalls {
			if b.fallbackEnabled() {
				return false, b.fallbackFn()
			}
			return false, nil
		}
		b.halfOpenCalls++
		return true, nil
	default: // CircuitClosed
		return true, nil
	}
}

// fallbackEnabled reports whether a fallback option is wired.
func (b *DefaultCircuitBreaker) fallbackEnabled() bool { return b.fallback != nil }

// fallbackFn returns the configured fallback callable.
func (b *DefaultCircuitBreaker) fallbackFn() func() (any, error) { return b.fallback }

// record applies the outcome of a call to the state machine.
func (b *DefaultCircuitBreaker) record(ctx context.Context, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case CircuitClosed:
		if err != nil {
			b.consecutiveFailures++
			if b.consecutiveFailures >= b.cfg.FailureThreshold {
				b.transitionLocked(ctx, CircuitOpen)
				b.openedAt = b.now()
			}
		} else {
			b.consecutiveFailures = 0
		}
	case CircuitHalfOpen:
		if err != nil {
			b.transitionLocked(ctx, CircuitOpen)
			b.openedAt = b.now()
			b.halfOpenCalls = 0
			b.halfOpenSuccesses = 0
		} else {
			b.halfOpenSuccesses++
			if b.halfOpenSuccesses >= b.cfg.SuccessThreshold {
				b.transitionLocked(ctx, CircuitClosed)
				b.consecutiveFailures = 0
				b.halfOpenCalls = 0
				b.halfOpenSuccesses = 0
			}
		}
	default:
		// No outcome is recorded outside Closed / HalfOpen.
	}
}

// transitionLocked moves the breaker to next and emits a circuit span.
// It must be called while b.mu is held.
func (b *DefaultCircuitBreaker) transitionLocked(ctx context.Context, next CircuitState) {
	if b.state == next {
		return
	}
	prev := b.state
	b.state = next

	span, ctx := tracing.SpanFromContext(ctx, "production.circuit", tracing.SpanKindInternal)
	defer span.End()
	span.SetAttributes(
		tracing.Attribute{Key: "state", Value: next.String()},
		tracing.Attribute{Key: "transition", Value: prev.String() + "->" + next.String()},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.InfoContext(ctx, "circuit_state_change",
		"from", prev.String(),
		"to", next.String(),
	)
}

// State returns the current breaker state.
func (b *DefaultCircuitBreaker) State() CircuitState {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.state
}

// Reset returns the breaker to Closed and clears failure counters.
func (b *DefaultCircuitBreaker) Reset(_ context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = CircuitClosed
	b.consecutiveFailures = 0
	b.halfOpenCalls = 0
	b.halfOpenSuccesses = 0
	b.openedAt = time.Time{}
	return nil
}

// Name returns the breaker identifier.
func (b *DefaultCircuitBreaker) Name() string { return b.name }
