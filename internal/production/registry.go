package production

import (
	"log/slog"
	"sync"
)

// This file provides a minimal process-wide registry for the production
// resilience components. It lets callers swap in custom LoopDetector /
// CircuitBreaker / RetryPolicy implementations while keeping a safe nil-default
// otherwise.

var (
	registryMu sync.RWMutex

	defaultLoopDetector   LoopDetector
	defaultCircuitBreaker CircuitBreaker
	defaultRetryPolicy    RetryPolicy
	defaultIdempotent     IdempotentCache
	defaultAudit          AuditLog
	defaultTelemetry      Telemetry

	// once guards ensure lazy defaults are created only once so stateful
	// components (circuit breaker, loop detector, etc.) accumulate state
	// across calls instead of being silently reset.
	loopDetectorOnce   sync.Once
	circuitBreakerOnce sync.Once
	retryPolicyOnce    sync.Once
	idempotentOnce     sync.Once
	auditOnce          sync.Once
	telemetryOnce      sync.Once

	loopDetectorInstance   LoopDetector
	circuitBreakerInstance CircuitBreaker
	retryPolicyInstance    RetryPolicy
	idempotentInstance     IdempotentCache
	auditInstance          AuditLog
	telemetryInstance      Telemetry
)

// ResetDefaults clears all registered production components, resetting the
// registry to its initial state. This is primarily intended for test isolation
// to prevent state leakage between test cases.
func ResetDefaults() {
	registryMu.Lock()
	defer registryMu.Unlock()
	outputGuardMu.Lock()
	defer outputGuardMu.Unlock()
	defaultLoopDetector = nil
	defaultCircuitBreaker = nil
	defaultRetryPolicy = nil
	defaultIdempotent = nil
	defaultAudit = nil
	defaultTelemetry = nil
	defaultOutputGuard = nil
	// Reset once guards so lazy defaults are recreated after a reset.
	loopDetectorOnce = sync.Once{}
	circuitBreakerOnce = sync.Once{}
	retryPolicyOnce = sync.Once{}
	idempotentOnce = sync.Once{}
	auditOnce = sync.Once{}
	telemetryOnce = sync.Once{}
}

// RegisterLoopDetector sets the active LoopDetector. A nil value resets to a
// fresh DefaultLoopDetector.
func RegisterLoopDetector(d LoopDetector) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if d == nil {
		d = NewDefaultLoopDetector(LoopDetectionConfig{})
	}
	slog.Info("production.register.loop_detector", "name", d.Name())
	defaultLoopDetector = d
}

// GetLoopDetector returns the active LoopDetector, lazily defaulting to a
// DefaultLoopDetector when none has been registered.
func GetLoopDetector() LoopDetector {
	registryMu.RLock()
	if defaultLoopDetector != nil {
		d := defaultLoopDetector
		registryMu.RUnlock()
		return d
	}
	registryMu.RUnlock()

	loopDetectorOnce.Do(func() {
		loopDetectorInstance = NewDefaultLoopDetector(LoopDetectionConfig{})
	})
	return loopDetectorInstance
}

// RegisterCircuitBreaker sets the active CircuitBreaker. A nil value resets to
// a fresh DefaultCircuitBreaker.
func RegisterCircuitBreaker(b CircuitBreaker) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if b == nil {
		b = NewDefaultCircuitBreaker(CircuitBreakerConfig{})
	}
	slog.Info("production.register.circuit_breaker", "name", b.Name())
	defaultCircuitBreaker = b
}

// GetCircuitBreaker returns the active CircuitBreaker, lazily defaulting to a
// DefaultCircuitBreaker when none has been registered.
func GetCircuitBreaker() CircuitBreaker {
	registryMu.RLock()
	if defaultCircuitBreaker != nil {
		b := defaultCircuitBreaker
		registryMu.RUnlock()
		return b
	}
	registryMu.RUnlock()

	circuitBreakerOnce.Do(func() {
		circuitBreakerInstance = NewDefaultCircuitBreaker(CircuitBreakerConfig{})
	})
	return circuitBreakerInstance
}

// RegisterRetryPolicy sets the active RetryPolicy. A nil value resets to a
// fresh DefaultRetryPolicy.
func RegisterRetryPolicy(p RetryPolicy) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if p == nil {
		p = NewDefaultRetryPolicy(RetryConfig{})
	}
	slog.Info("production.register.retry_policy", "name", p.Name())
	defaultRetryPolicy = p
}

// GetRetryPolicy returns the active RetryPolicy, lazily defaulting to a
// DefaultRetryPolicy when none has been registered.
func GetRetryPolicy() RetryPolicy {
	registryMu.RLock()
	if defaultRetryPolicy != nil {
		p := defaultRetryPolicy
		registryMu.RUnlock()
		return p
	}
	registryMu.RUnlock()

	retryPolicyOnce.Do(func() {
		retryPolicyInstance = NewDefaultRetryPolicy(RetryConfig{})
	})
	return retryPolicyInstance
}

// RegisterIdempotentCache sets the active IdempotentCache. A nil value resets
// to a fresh FIFOIdempotentCache.
func RegisterIdempotentCache(c IdempotentCache) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if c == nil {
		c = NewFIFOIdempotentCache(0)
	}
	slog.Info("production.register.idempotent_cache", "name", c.Name())
	defaultIdempotent = c
}

// GetIdempotentCache returns the active IdempotentCache, lazily defaulting to
// a FIFOIdempotentCache when none has been registered.
func GetIdempotentCache() IdempotentCache {
	registryMu.RLock()
	if defaultIdempotent != nil {
		c := defaultIdempotent
		registryMu.RUnlock()
		return c
	}
	registryMu.RUnlock()

	idempotentOnce.Do(func() {
		idempotentInstance = NewFIFOIdempotentCache(0)
	})
	return idempotentInstance
}

// RegisterAuditLog sets the active AuditLog. A nil value resets to a
// no-persistence DefaultAuditLog with an empty path.
func RegisterAuditLog(l AuditLog) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if l == nil {
		l = NewDefaultAuditLog("")
	}
	slog.Info("production.register.audit_log", "name", l.Name())
	defaultAudit = l
}

// GetAuditLog returns the active AuditLog, lazily defaulting to a
// DefaultAuditLog when none has been registered.
func GetAuditLog() AuditLog {
	registryMu.RLock()
	if defaultAudit != nil {
		a := defaultAudit
		registryMu.RUnlock()
		return a
	}
	registryMu.RUnlock()

	auditOnce.Do(func() {
		auditInstance = NewDefaultAuditLog("")
	})
	return auditInstance
}

// RegisterTelemetry sets the active Telemetry. A nil value resets to a fresh
// DefaultTelemetry.
func RegisterTelemetry(t Telemetry) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if t == nil {
		t = NewDefaultTelemetry()
	}
	slog.Info("production.register.telemetry", "name", t.Name())
	defaultTelemetry = t
}

// GetTelemetry returns the active Telemetry, lazily defaulting to a
// DefaultTelemetry when none has been registered.
func GetTelemetry() Telemetry {
	registryMu.RLock()
	if defaultTelemetry != nil {
		t := defaultTelemetry
		registryMu.RUnlock()
		return t
	}
	registryMu.RUnlock()

	telemetryOnce.Do(func() {
		telemetryInstance = NewDefaultTelemetry()
	})
	return telemetryInstance
}
