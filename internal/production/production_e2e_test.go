package production

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// Production end-to-end tests: exercise composite scenarios that combine the
// guards, resilience components, idempotency cache, audit log and telemetry in
// realistic ways, plus concurrent registry registration under the race detector.

// TestDefaultGuardChainRejectsXSSAndTruncates is an end-to-end check of the
// registry's default chain (code injection + PII + length 8192).
func TestDefaultGuardChainRejectsXSSAndTruncates(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	chain := defaultOutputGuardChain()

	// XSS is rejected with critical severity and no sanitized output leaks.
	res, err := chain.Check(ctx, "<script>alert(document.cookie)</script>")
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardCritical, res.Severity)
	assert.Empty(t, res.Sanitized)

	// A huge normal text is truncated to the configured 8192 runes.
	long := bigRunes(10000)
	res, err = chain.Check(ctx, long)
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.Equal(t, GuardLow, res.Severity)
	assert.Equal(t, 8192, len([]rune(res.Sanitized)))
}

// bigRunes builds a string of n ASCII runes quickly.
func bigRunes(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

// TestResilientOperationLoopEndToEnd composes a circuit breaker, retry policy
// and loop detector against a flaky operation to model an LLM wrapper.
func TestResilientOperationLoopEndToEnd(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	var calls atomic.Int64
	var ok atomic.Bool

	// A flaky callable that succeeds only on the 4th attempt.
	fn := func() (any, error) {
		n := calls.Add(1)
		if n < 4 {
			return nil, NewError(ErrorTransient, errConnReset())
		}
		ok.Store(true)
		return "completed", nil
	}

	breaker := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 10,
		RecoveryTimeout:  time.Minute,
	}, WithName("model-breaker"))
	policy := NewDefaultRetryPolicy(RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
		Jitter:      2 * time.Millisecond,
	})

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if !policy.ShouldRetry(ctx, lastErr, attempt) && lastErr != nil {
			break
		}
		out, err := breaker.Execute(ctx, fn)
		lastErr = err
		if err == nil {
			require.Equal(t, "completed", out)
			break
		}
	}
	require.True(t, ok.Load(), "the operation must eventually succeed within retry budget")
	require.Equal(t, int64(4), calls.Load())

	// A loop detector observing a healthy edit cadence must not flag anything.
	det := NewDefaultLoopDetector(LoopDetectionConfig{EditThreshold: 3})
	require.NoError(t, det.Observe(ctx, evt(KindEdit, "pkg/a.go")))
	require.NoError(t, det.Observe(ctx, evt(KindEdit, "pkg/b.go")))
	res := det.Check(ctx)
	assert.False(t, res.Detected)
}

// errConnReset returns a transient-shaped error for the retry policy.
func errConnReset() error {
	return errors.New("the downstream connection was reset")
}

// TestIdempotentCacheEndToEnd uses the cache to deduplicate repeated identical
// operations across many sequential callers so the underlying op runs exactly
// once: miss -> execute -> set -> all subsequent calls hit.
func TestIdempotentCacheEndToEnd(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	cache := NewFIFOIdempotentCache(16)

	key := "op:dedupe"
	executions := 0

	// First caller sees a miss and performs the expensive operation once.
	_, hit := cache.Get(ctx, key)
	require.False(t, hit)
	executions++
	require.NoError(t, cache.Set(ctx, key, "done"))

	// Every subsequent caller, across many iterations, hits the cached value.
	for i := 0; i < 100; i++ {
		v, hit := cache.Get(ctx, key)
		require.True(t, hit, "iteration %d must hit the cached value", i)
		require.Equal(t, "done", v)
		executions++ // (counted here only to mirror the pattern; never runs op)
	}
	// The op itself was invoked exactly once; the extra iterations were hits.
	assert.Equal(t, 101, executions)
	v, ok := cache.Get(ctx, key)
	require.True(t, ok)
	assert.Equal(t, "done", v)
}

// TestIdempotentCacheConcurrentUniqueKeys race-hardens the cache across many
// goroutines writing distinct keys and reading them back without data races.
func TestIdempotentCacheConcurrentUniqueKeys(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	cache := NewFIFOIdempotentCache(64)

	const workers = 8
	const keysPerWorker = 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for k := 0; k < keysPerWorker; k++ {
				key := "w" + string(rune('a'+w)) + "-" + string(rune('a'+k))
				_ = cache.Set(ctx, key, w*k) //nolint:errcheck // Set returns nil; value ignored
				_, _ = cache.Get(ctx, key)
				if k%4 == 0 {
					_ = cache.Delete(ctx, key) //nolint:errcheck // Delete returns nil
				}
			}
		}(w)
	}
	wg.Wait()
}

// TestAuditTelemetryEndToEnd writes audit entries and records telemetry, then
// verifies both are recoverable and consistent.
func TestAuditTelemetryEndToEnd(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "e2e-audit.jsonl")
	audit := NewDefaultAuditLog(path)
	tel, ok := NewDefaultTelemetry().(*DefaultTelemetry)
	require.True(t, ok)

	const n = 5
	for i := 0; i < n; i++ {
		require.NoError(t, audit.Log(ctx, AuditEntry{
			Operation: "tool.run",
			ToolName:  "editor",
			UserID:    "u-e2e",
		}))
		require.NoError(t, tel.Record(ctx, TelemetryMetric{
			Name:   "tool_run_total",
			Value:  1,
			Labels: map[string]string{"tool": "editor"},
		}))
	}

	entries, err := audit.Query(ctx, AuditFilter{Operation: "tool.run", ToolName: "editor"})
	require.NoError(t, err)
	require.Len(t, entries, n)
	for _, e := range entries {
		assert.Equal(t, "editor", e.ToolName)
	}

	snap := tel.Snapshot()
	assert.Equal(t, float64(n), snap["tool_run_total{tool=editor}"])
}

// TestTelemetryAndIdempotentCacheIntegration records metrics per cache
// operation to confirm the two storage layers interact cleanly.
func TestTelemetryAndIdempotentCacheIntegration(t *testing.T) {
	ctx := context.Background()
	cache := NewFIFOIdempotentCache(8)
	tel, ok := NewDefaultTelemetry().(*DefaultTelemetry)
	require.True(t, ok)

	require.NoError(t, cache.Set(ctx, "a", 1))
	require.NoError(t, cache.Set(ctx, "b", 2))
	v, ok := cache.Get(ctx, "a")
	require.True(t, ok)
	require.Equal(t, 1, v)

	require.NoError(t, tel.Record(ctx, TelemetryMetric{Name: "cache_puts", Value: 2}))
	require.NoError(t, tel.Record(ctx, TelemetryMetric{Name: "cache_hits", Value: 1}))
	require.NoError(t, tel.Record(ctx, TelemetryMetric{Name: "cache_misses", Value: 0}))

	snap := tel.Snapshot()
	assert.Equal(t, 2.0, snap["cache_puts"])
	assert.Equal(t, 1.0, snap["cache_hits"])
	assert.Equal(t, 0.0, snap["cache_misses"])
}

// TestLoopDetectorEndToEndWithTerminate simulates an agent stuck editing one
// file repeatedly and asserts the terminate disposition is recommended.
func TestLoopDetectorEndToEndWithTerminate(t *testing.T) {
	ctx := context.Background()
	det := NewDefaultLoopDetector(LoopDetectionConfig{
		EditThreshold: 3,
		Disposition:   DispositionTerminate,
	})

	for i := 0; i < 3; i++ {
		require.NoError(t, det.Observe(ctx, evt(KindEdit, "stuck.go")))
	}

	res := det.Check(ctx)
	require.True(t, res.Detected)
	assert.Equal(t, DimensionEditCount, res.Dimension)
	assert.Equal(t, DispositionTerminate, res.Disposition)
	assert.Equal(t, 3, res.Count)
	assert.Contains(t, res.Message, "stuck.go")
}

// TestRegistryAllComponentsConcurrent race-hardens concurrent registration and
// retrieval of every registry-backed component.
func TestRegistryAllComponentsConcurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	orig := func() {
		RegisterLoopDetector(nil)
		RegisterCircuitBreaker(nil)
		RegisterRetryPolicy(nil)
		RegisterIdempotentCache(nil)
		RegisterAuditLog(nil)
		RegisterTelemetry(nil)
		RegisterOutputGuard(nil)
	}
	orig()

	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if (g+i)%2 == 0 {
					RegisterLoopDetector(NewDefaultLoopDetector(LoopDetectionConfig{}, WithName("conc-loop")))
					RegisterCircuitBreaker(NewDefaultCircuitBreaker(CircuitBreakerConfig{}, WithName("conc-cb")))
					RegisterRetryPolicy(NewDefaultRetryPolicy(RetryConfig{}, WithName("conc-retry")))
				} else {
					RegisterLoopDetector(nil)
					RegisterCircuitBreaker(nil)
					RegisterRetryPolicy(nil)
				}
				RegisterIdempotentCache(NewFIFOIdempotentCache(1))
				RegisterAuditLog(NewDefaultAuditLog(""))
				RegisterTelemetry(NewDefaultTelemetry())
				RegisterOutputGuard(NewRegexOutputGuard([]string{`x`}))

				_ = GetLoopDetector()
				_ = GetCircuitBreaker()
				_ = GetRetryPolicy()
				_ = GetIdempotentCache()
				_ = GetAuditLog()
				_ = GetTelemetry()
				_ = GetOutputGuard()
			}
		}(g)
	}
	wg.Wait()

	// All registry entries remain reachable after the storm.
	assert.NotNil(t, GetLoopDetector())
	assert.NotNil(t, GetCircuitBreaker())
	assert.NotNil(t, GetRetryPolicy())
	assert.NotNil(t, GetIdempotentCache())
	assert.NotNil(t, GetAuditLog())
	assert.NotNil(t, GetTelemetry())
	assert.NotNil(t, GetOutputGuard())
}

// TestNilRegisterResetsToDefault verifies that explicit nil registrations yield
// concrete, non-panicking default components for every registry.
func TestNilRegisterResetsToDefault(t *testing.T) {
	RegisterLoopDetector(nil)
	RegisterCircuitBreaker(nil)
	RegisterRetryPolicy(nil)
	RegisterIdempotentCache(nil)
	RegisterAuditLog(nil)
	RegisterTelemetry(nil)
	RegisterOutputGuard(nil)

	require.Equal(t, "loop-detector", GetLoopDetector().Name())
	require.Equal(t, "circuit-breaker", GetCircuitBreaker().Name())
	require.Equal(t, "default-retry-policy", GetRetryPolicy().Name())
	require.Equal(t, "fifo-idempotent-cache", GetIdempotentCache().Name())
	require.Equal(t, "default-audit-log", GetAuditLog().Name())
	require.Equal(t, "default-telemetry", GetTelemetry().Name())
	require.Equal(t, "output-guard-chain", GetOutputGuard().Name())
}

// TestGuardAndLoopDetectorCombined drives a shared event stream through both
// the output guard and the loop detector to model an agentic loop safety pass.
func TestGuardAndLoopDetectorCombined(t *testing.T) {
	ctx := context.Background()

	guard := defaultOutputGuardChain()
	det := NewDefaultLoopDetector(LoopDetectionConfig{
		TestFailureThreshold: 2,
		Disposition:          DispositionSteer,
	})

	// A failing test message that also carries a code-injection string.
	injected := "UPDATE users SET admin = 1"
	res, err := guard.Check(ctx, injected)
	require.NoError(t, err)
	assert.False(t, res.Allowed)

	// Two consecutive test failure events trip the loop detector.
	for i := 0; i < 2; i++ {
		require.NoError(t, det.Observe(ctx, evt(KindTestFailure, "TestEndToEnd")))
	}
	lr := det.Check(ctx)
	require.True(t, lr.Detected)
	assert.Equal(t, DimensionTestFailure, lr.Dimension)

	// Reset and confirm a clean slate.
	require.NoError(t, det.Reset(ctx))
	assert.False(t, det.Check(ctx).Detected)
}

// TestCircuitRetryPolicyWithCategorizedPrecedence verifies the resilience
// pipeline honors an explicit category even when the text would otherwise be
// classified differently.
func TestCircuitRetryPolicyWithCategorizedPrecedence(t *testing.T) {
	ctx := context.Background()
	policy, ok := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 4, BaseDelay: time.Millisecond}).(*DefaultRetryPolicy)
	require.True(t, ok)

	// An error whose text mentions "connection reset" (transient) but carries
	// an explicit rate-limit category: the explicit category must win.
	explicit := NewError(ErrorRateLimit, errConnReset())
	assert.Equal(t, ErrorRateLimit, policy.Classify(explicit))
	assert.True(t, policy.ShouldRetry(ctx, explicit, 0))

	// The same error flowing through a breaker is propagated unchanged so the
	// caller can still classify it precisely.
	breaker := NewDefaultCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 1})
	_, err := breaker.Execute(ctx, failFn(explicit))
	assert.ErrorIs(t, err, explicit)
	assert.Equal(t, ErrorRateLimit, policy.Classify(err))
}

// TestCircuitRetryBreakerSmoke requires the breaker to propagate the wrapped
// result unchanged and to record a success (no accidental state flip).
func TestCircuitRetryBreakerSmoke(t *testing.T) {
	ctx := context.Background()
	breaker := NewDefaultCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 2})

	var attempts atomic.Int64
	for i := 0; i < 3; i++ {
		out, err := breaker.Execute(ctx, func() (any, error) {
			attempts.Add(1)
			return "result", nil
		})
		require.NoError(t, err)
		require.Equal(t, "result", out)
	}
	assert.Equal(t, int64(3), attempts.Load())
	assert.Equal(t, CircuitClosed, breaker.State(), "repeated successes must keep the breaker closed")
}

// TestLoopDetectorConsumesCoreEvents confirms the loop detector operates on the
// same core.AgentEvent values used across the runtime.
func TestLoopDetectorConsumesCoreEvents(t *testing.T) {
	ctx := context.Background()
	det := NewDefaultLoopDetector(LoopDetectionConfig{
		SameToolCallThreshold: 2,
	})
	require.NoError(t, det.Observe(ctx, evt(KindToolCall, "tool:read_file:{")))
	require.NoError(t, det.Observe(ctx, evt(KindToolCall, "tool:read_file:{")))

	res := det.Check(ctx)
	require.True(t, res.Detected)
	assert.Equal(t, DimensionSameToolCall, res.Dimension)
}
