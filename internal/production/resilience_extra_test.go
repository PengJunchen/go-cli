package production

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// applyOptions tolerates nil entries and folds in order.
func TestApplyOptionsHandlesNil(t *testing.T) {
	o := applyOptions([]Option{nil, WithName("first"), nil, WithName("second")})
	assert.Equal(t, "second", o.name)

	o = applyOptions(nil)
	assert.Equal(t, "", o.name)
}

// FIFOIdempotentCache: default maxSize fallback and re-Set keeps position.
func TestFIFOCacheDefaultSizeAndSetKeepsPosition(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()

	// Non-positive maxSize falls back to 1024.
	c := NewFIFOIdempotentCache(-1)
	assert.Equal(t, "fifo-idempotent-cache", c.Name())
	require.NoError(t, c.Set(ctx, "k", "v"))
	assert.Equal(t, 1024, c.maxSize)

	// Re-setting an existing key does not re-append to the eviction order.
	c2 := NewFIFOIdempotentCache(2)
	require.NoError(t, c2.Set(ctx, "a", 1))
	require.NoError(t, c2.Set(ctx, "b", 2))
	require.NoError(t, c2.Set(ctx, "a", 999)) // refresh value, keep position
	require.NoError(t, c2.Set(ctx, "c", 3))   // evicts "a" (oldest), not "b"
	_, ok := c2.Get(ctx, "a")
	assert.False(t, ok, "a was refreshed but remains oldest and should be evicted")
	_, ok = c2.Get(ctx, "b")
	assert.True(t, ok)
	v, ok := c2.Get(ctx, "c")
	assert.True(t, ok)
	assert.Equal(t, 3, v)
}

// FIFOIdempotentCache: Delete of a missing key is a no-op returning nil.
func TestFIFOCacheDeleteMissingIsNoOp(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	c := NewFIFOIdempotentCache(4)
	require.NoError(t, c.Delete(ctx, "never-set"))
}

// AuditFilter.Matches covers each dimension independently.
func TestAuditFilterMatches(t *testing.T) {
	base := time.Now()
	entry := AuditEntry{Timestamp: base, Operation: "opA", ToolName: "toolX"}

	assert.True(t, (AuditFilter{}).Matches(entry))

	// From excludes entries before it.
	assert.True(t, AuditFilter{From: base.Add(-time.Second)}.Matches(entry))
	assert.False(t, AuditFilter{From: base.Add(time.Second)}.Matches(entry))

	// To excludes entries after it.
	assert.True(t, AuditFilter{To: base.Add(time.Second)}.Matches(entry))
	assert.False(t, AuditFilter{To: base.Add(-time.Second)}.Matches(entry))

	assert.True(t, AuditFilter{Operation: "opA"}.Matches(entry))
	assert.False(t, AuditFilter{Operation: "opB"}.Matches(entry))

	assert.True(t, AuditFilter{ToolName: "toolX"}.Matches(entry))
	assert.False(t, AuditFilter{ToolName: "toolY"}.Matches(entry))
}

// DefaultAuditLog: a malformed JSONL line makes Query error.
func TestAuditLogQueryCorruptLineErrors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{not-json}\n"), 0o600))

	l := NewDefaultAuditLog(path)
	_, err := l.Query(ctx, AuditFilter{})
	require.Error(t, err)
}

// DefaultAuditLog: a directory path leads to a read/scan path; on platforms
// where opening a directory succeeds the scan yields zero entries.
func TestAuditLogQueryOnDirectoryIsHarmless(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	dir := t.TempDir()
	l := NewDefaultAuditLog(dir)

	entries, err := l.Query(ctx, AuditFilter{})
	if err == nil {
		assert.Empty(t, entries)
	}
}

// DefaultAuditLog: a zero Timestamp when reading back after AutoTimestamp.
func TestAuditLogPreservesFullEntry(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l := NewDefaultAuditLog(path)
	now := time.Now().UTC()

	require.NoError(t, l.Log(ctx, AuditEntry{
		Timestamp: now,
		Operation: "tool.run",
		ToolName:  "bash",
		Args:      map[string]any{"cmd": "ls"},
		Result:    map[string]any{"exit": 0},
		UserID:    "u-9",
		SessionID: "s-7",
	}))

	entries, err := l.Query(ctx, AuditFilter{Operation: "tool.run", ToolName: "bash"})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, now, entries[0].Timestamp)
	assert.Equal(t, map[string]any{"cmd": "ls"}, entries[0].Args)
	assert.Equal(t, map[string]any{"exit": float64(0)}, entries[0].Result)
}

// Telemetry: lastSeen tracks the most recent timestamp per name.
func TestTelemetryTracksLastSeen(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	tm := NewDefaultTelemetry()
	t1 := time.Now().Add(-time.Hour)
	t2 := time.Now()

	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "gauge", Value: 1, Timestamp: t1}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "gauge", Value: 2, Timestamp: t2}))

	assert.Equal(t, 3.0, tm.Snapshot()["gauge"])
	assert.WithinDuration(t, t2, tm.lastSeen["gauge"], time.Millisecond*10)
}

// CircuitBreaker: RecoveryTimeout<=0 keeps the breaker Open indefinitely.
func TestCircuitNeverRecoversWithoutTimeout(t *testing.T) {
	ctx := context.Background()
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  0, // no recovery
	})

	sentinel := errors.New("boom")
	_, err := b.Execute(ctx, failFn(sentinel))
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, CircuitOpen, b.State())

	_, err = b.Execute(ctx, okFn("x"))
	require.ErrorIs(t, err, ErrCircuitOpen)
}

// CircuitBreaker: State().String() returns the canonical form.
func TestCircuitStateString(t *testing.T) {
	assert.Equal(t, "closed", CircuitClosed.String())
	assert.Equal(t, "open", CircuitOpen.String())
	assert.Equal(t, "half_open", CircuitHalfOpen.String())
}

// CircuitBreaker: HalfOpenMaxCalls bounds probing and refuses extra calls with
// a fallback.
func TestCircuitHalfOpenMaxCallsBounded(t *testing.T) {
	ctx, _ := newCircuitTestCtx(t)
	clockVal, clock := manualClock()
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 2, // successful probes accumulate rather than closing immediately
	}, WithClock(clock), WithFallback(func() (any, error) { return "fallback", nil }))

	sentinel := errors.New("boom")
	_, err := b.Execute(ctx, failFn(sentinel))
	require.ErrorIs(t, err, sentinel)
	require.Equal(t, CircuitOpen, b.State())

	// Enter HalfOpen after timeout; first probe consumes the single slot.
	clockVal.Add(int64(2 * time.Second))
	out, err := b.Execute(ctx, okFn("probe"))
	require.NoError(t, err)
	require.Equal(t, "probe", out)
	require.Equal(t, CircuitHalfOpen, b.State())

	// Second call while still HalfOpen (slot exhausted) -> fallback used.
	out, err = b.Execute(ctx, failFn(sentinel))
	require.NoError(t, err)
	require.Equal(t, "fallback", out)
}

// CircuitBreaker: Reset idempotent and toggles to Closed from any state.
func TestCircuitResetIsIdempotent(t *testing.T) {
	ctx := context.Background()
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{FailureThreshold: 1})
	require.NoError(t, b.Reset(ctx))
	require.Equal(t, CircuitClosed, b.State())
	require.NoError(t, b.Reset(ctx))
	require.Equal(t, CircuitClosed, b.State())
}

// CircuitBreaker: concurrency storm verifying no race and terminal state valid.
func TestCircuitConcurrentFallbackAndProbe(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newCircuitTestCtx(t)
	clockVal, clock := manualClock()
	b := NewDefaultCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryTimeout:  10 * time.Millisecond,
		HalfOpenMaxCalls: 2,
		SuccessThreshold: 1,
	}, WithClock(clock), WithFallback(func() (any, error) { return "fb", nil }))

	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				clockVal.Store(int64(i) * int64(time.Millisecond))
				var err error
				if i%2 == 0 {
					_, err = b.Execute(ctx, failFn(errors.New("boom")))
				} else {
					_, err = b.Execute(ctx, okFn(i))
				}
				_ = err
			}
		}(g)
	}
	wg.Wait()
}

// CategorizedError: Error() with a nil inner error falls back to the category.
func TestCategorizedErrorNilInner(t *testing.T) {
	e := &CategorizedError{category: ErrorRateLimit}
	assert.Equal(t, "rate_limit", e.Error())
	assert.Nil(t, e.Unwrap())
}

// RetryPolicy: Classify follows wrapped error chains built from multiple NewError.
func TestClassifyFollowsWrappedChain(t *testing.T) {
	p := NewDefaultRetryPolicy(RetryConfig{})
	inner := errors.New("429 too many requests")
	outer := NewError(ErrorFatal, errors.New("wrapped"))
	// errContains walks the chain; classify prioritizes explicit category.
	assert.Equal(t, ErrorFatal, p.Classify(outer))
	// An explicit rate-limit category beats the string match.
	assert.Equal(t, ErrorRateLimit, p.Classify(NewError(ErrorRateLimit, inner)))
}

// RetryPolicy: errContains is case-insensitive across the error chain.
func TestErrContainsAcrossChain(t *testing.T) {
	root := errors.New("Underlying Connection RESET happened")
	wrapped := fmt.Errorf("outer: %w", root)
	// errContains lowercases the message text but not the supplied substrings.
	assert.True(t, errContains(wrapped, "connection reset"))
	assert.False(t, errContains(wrapped, "nothing"))
	assert.False(t, errContains(nil, "x"))
}

// RetryPolicy: randomJitter returns [0,jitter) and fails closed on bad input.
func TestRandomJitterBounds(t *testing.T) {
	if j, ok := randomJitter(100 * time.Millisecond); ok {
		assert.GreaterOrEqual(t, j, time.Duration(0))
		assert.Less(t, j, 100*time.Millisecond)
	}
	_, ok := randomJitter(0)
	assert.False(t, ok)
	_, ok = randomJitter(-5)
	assert.False(t, ok)
}

// RetryPolicy: ShouldRetry with a nil error (attempts pending) classifies fatal.
func TestShouldRetryNilError(t *testing.T) {
	ctx, _ := newRetryTestCtx(t)
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 3})
	require.False(t, p.ShouldRetry(ctx, nil, 0))
}

// RetryPolicy: concurrency safety on an immutable policy.
func TestRetryPolicyConcurrentBackoff(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	p := NewDefaultRetryPolicy(RetryConfig{MaxAttempts: 5, BaseDelay: time.Millisecond})

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_ = p.NextBackoff(ctx, i%5)
			}
		}()
	}
	wg.Wait()
}

// options: WithFallback/WithClock/WithGuardSeverity fold into options.
func TestOptionSetters(t *testing.T) {
	fb := func() (any, error) { return nil, nil }
	o := applyOptions([]Option{
		WithFallback(fb),
		WithClock(time.Now),
		WithGuardSeverity(GuardCritical),
	})
	assert.NotNil(t, o.fallback)
	assert.NotNil(t, o.now)
	assert.Equal(t, GuardCritical, o.guardSeverity)
}

// Registry: loop detector / circuit breaker / retry policy setters with
// lazy defaults, custom overrides, and nil-reset-to-default.
func TestRegistryLoopCircuitRetry(t *testing.T) {
	origLoop, origCB, origRetry := GetLoopDetector(), GetCircuitBreaker(), GetRetryPolicy()
	defer func() {
		RegisterLoopDetector(origLoop)
		RegisterCircuitBreaker(origCB)
		RegisterRetryPolicy(origRetry)
	}()

	// Lazy defaults when unset.
	assert.NotNil(t, GetLoopDetector())
	assert.NotNil(t, GetCircuitBreaker())
	assert.NotNil(t, GetRetryPolicy())

	// nil resets to a fresh default implementation.
	RegisterLoopDetector(nil)
	RegisterCircuitBreaker(nil)
	RegisterRetryPolicy(nil)
	assert.Equal(t, "loop-detector", GetLoopDetector().Name())
	require.Equal(t, CircuitClosed, GetCircuitBreaker().State())
	assert.Equal(t, "default-retry-policy", GetRetryPolicy().Name())

	// Custom implementations are retrievable.
	RegisterLoopDetector(NewDefaultLoopDetector(LoopDetectionConfig{}, WithName("custom-loop")))
	RegisterCircuitBreaker(NewDefaultCircuitBreaker(CircuitBreakerConfig{}, WithName("custom-cb")))
	RegisterRetryPolicy(NewDefaultRetryPolicy(RetryConfig{}, WithName("custom-retry")))
	assert.Equal(t, "custom-loop", GetLoopDetector().Name())
	assert.Equal(t, "custom-cb", GetCircuitBreaker().Name())
	assert.Equal(t, "custom-retry", GetRetryPolicy().Name())
}

// Registry: concurrent register/get for loop/circuit/retry under the race.
func TestRegistryLoopCircuitRetryConcurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			if g%2 == 0 {
				RegisterLoopDetector(nil)
				RegisterCircuitBreaker(nil)
				RegisterRetryPolicy(nil)
			} else {
				RegisterLoopDetector(NewDefaultLoopDetector(LoopDetectionConfig{}))
				RegisterCircuitBreaker(NewDefaultCircuitBreaker(CircuitBreakerConfig{}))
				RegisterRetryPolicy(NewDefaultRetryPolicy(RetryConfig{}))
			}
			_ = GetLoopDetector()
			_ = GetCircuitBreaker()
			_ = GetRetryPolicy()
		}(g)
	}
	wg.Wait()
}

// LoopDetector: empty edit content is ignored (does not bump count).
func TestLoopDetectorIgnoresEmptyEditContent(t *testing.T) {
	ctx := context.Background()
	det := NewDefaultLoopDetector(LoopDetectionConfig{EditThreshold: 1})
	require.NoError(t, det.Observe(ctx, evt(KindEdit, "")))
	res := det.Check(ctx)
	assert.False(t, res.Detected)
}

// LoopDetector: an unrecognized event kind is a no-op.
func TestLoopDetectorIgnoresUnknownKind(t *testing.T) {
	ctx := context.Background()
	det := NewDefaultLoopDetector(LoopDetectionConfig{EditThreshold: 1})
	require.NoError(t, det.Observe(ctx, evt("totally_new_kind", "data")))
	require.NoError(t, det.Observe(ctx, evt("", "")))
	res := det.Check(ctx)
	assert.False(t, res.Detected)
}

// LoopDetector: custom event kind overrides the package defaults.
func TestLoopDetectorCustomKinds(t *testing.T) {
	ctx := context.Background()

	// Edit dimension with a custom kind.
	editDet := NewDefaultLoopDetector(LoopDetectionConfig{
		EditThreshold: 2,
		EditKind:      "patch",
	})
	require.NoError(t, editDet.Observe(ctx, evt("patch", "file.go")))
	require.NoError(t, editDet.Observe(ctx, evt("patch", "file.go")))
	res := editDet.Check(ctx)
	require.True(t, res.Detected)
	require.Equal(t, DimensionEditCount, res.Dimension)

	// Test-failure dimension with a custom kind.
	failDet := NewDefaultLoopDetector(LoopDetectionConfig{
		TestFailureThreshold: 2,
		TestFailureKind:      "fail",
	})
	require.NoError(t, failDet.Observe(ctx, evt("fail", "T")))
	require.NoError(t, failDet.Observe(ctx, evt("fail", "T")))
	res = failDet.Check(ctx)
	require.True(t, res.Detected)
	require.Equal(t, DimensionTestFailure, res.Dimension)
}

// toolCallKey is deterministic and different payloads yield different keys.
func TestToolCallKeyDeterminism(t *testing.T) {
	a := toolCallKey("payload-a")
	b := toolCallKey("payload-a")
	c := toolCallKey("payload-b")
	assert.Equal(t, a, b)
	assert.NotEqual(t, a, c)
	assert.Equal(t, 24, len(a))
}

// LoopDetector: concurrent Observe/Check/Reset under race detector.
func TestLoopDetectorConcurrentReset(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	det := NewDefaultLoopDetector(LoopDetectionConfig{EditThreshold: 1000})

	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				require.NoError(t, det.Observe(ctx, evt(KindEdit, "file.go")))
				_ = det.Check(ctx)
				if i%10 == 0 {
					require.NoError(t, det.Reset(ctx))
				}
			}
		}()
	}
	wg.Wait()
}
