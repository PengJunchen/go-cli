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
)

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
	defer registryMu.RUnlock()
	if defaultLoopDetector == nil {
		return NewDefaultLoopDetector(LoopDetectionConfig{})
	}
	return defaultLoopDetector
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
	defer registryMu.RUnlock()
	if defaultCircuitBreaker == nil {
		return NewDefaultCircuitBreaker(CircuitBreakerConfig{})
	}
	return defaultCircuitBreaker
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
	defer registryMu.RUnlock()
	if defaultRetryPolicy == nil {
		return NewDefaultRetryPolicy(RetryConfig{})
	}
	return defaultRetryPolicy
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
	defer registryMu.RUnlock()
	if defaultIdempotent == nil {
		return NewFIFOIdempotentCache(0)
	}
	return defaultIdempotent
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
	defer registryMu.RUnlock()
	if defaultAudit == nil {
		return NewDefaultAuditLog("")
	}
	return defaultAudit
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
	defer registryMu.RUnlock()
	if defaultTelemetry == nil {
		return NewDefaultTelemetry()
	}
	return defaultTelemetry
}
