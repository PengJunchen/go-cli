package production

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// captureExporter is a minimal in-memory tracing.TraceExporter used to assert
// emitted spans. It mirrors MockTraceExporter's behavior without importing
// internal/mock (which imports internal/production for its mocks, causing an
// import cycle when referenced from this test package).
type captureExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

func (e *captureExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanToData(span))
	return nil
}

func (e *captureExporter) Shutdown(_ context.Context) error { return nil }

func (e *captureExporter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.spans)
}

func (e *captureExporter) allSpans() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]tracing.SpanData, len(e.spans))
	copy(out, e.spans)
	return out
}

func (e *captureExporter) hasSpan(name string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, s := range e.spans {
		if s.Name == name {
			return true
		}
	}
	return false
}

func newCaptureExporter() *captureExporter { return &captureExporter{} }

// discardErr consumes an error result so errcheck is satisfied inside
// goroutines where asserting on the error is not safe.
func discardErr(error) {}

// newIdemTestCtx wires a captureExporter + Tracer into a root context.
func newIdemTestCtx(t *testing.T) (context.Context, *captureExporter) {
	t.Helper()
	exporter := newCaptureExporter()
	tr := tracing.NewTracer("idem-trace", exporter)
	_, ctx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)
	return ctx, exporter
}

func TestFIFOCacheGetSetAndMiss(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newIdemTestCtx(t)
	c := NewFIFOIdempotentCache(4)

	_, ok := c.Get(ctx, "a")
	assert.False(t, ok, "unset key should be a miss")

	require.NoError(t, c.Set(ctx, "a", "v-a"))
	v, ok := c.Get(ctx, "a")
	assert.True(t, ok)
	assert.Equal(t, "v-a", v)
}

func TestFIFOCacheEvictsOldestAtCapacity(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newIdemTestCtx(t)
	c := NewFIFOIdempotentCache(2)

	require.NoError(t, c.Set(ctx, "k1", 1))
	require.NoError(t, c.Set(ctx, "k2", 2))
	require.NoError(t, c.Set(ctx, "k3", 3)) // evicts k1

	_, ok1 := c.Get(ctx, "k1")
	assert.False(t, ok1, "k1 should be evicted")
	_, ok2 := c.Get(ctx, "k2")
	assert.True(t, ok2, "k2 should remain")
	_, ok3 := c.Get(ctx, "k3")
	assert.True(t, ok3, "k3 should remain")
}

func TestFIFOCacheDeleteRemovesEntry(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newIdemTestCtx(t)
	c := NewFIFOIdempotentCache(4)

	require.NoError(t, c.Set(ctx, "a", 1))
	require.NoError(t, c.Delete(ctx, "a"))
	_, ok := c.Get(ctx, "a")
	assert.False(t, ok, "deleted key should be a miss")

	// Delete is idempotent.
	require.NoError(t, c.Delete(ctx, "a"))
}

func TestFIFOCacheMissThenHitNoReExecute(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newIdemTestCtx(t)
	c := NewFIFOIdempotentCache(4)

	// Simulate the idempotency pattern: miss -> execute -> set -> hit.
	executions := 0
	key := "op:build"
	_, ok := c.Get(ctx, key)
	if !ok {
		executions++
		require.NoError(t, c.Set(ctx, key, "done"))
	}

	v, ok := c.Get(ctx, key)
	require.True(t, ok)
	assert.Equal(t, "done", v)
	assert.Equal(t, 1, executions, "operation must execute exactly once")
}

func TestFIFOCacheConcurrentSafety(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newIdemTestCtx(t)
	c := NewFIFOIdempotentCache(100)

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				key := fmt.Sprintf("key-%d", i%20)
				discardErr(c.Set(ctx, key, g))
				_, _ = c.Get(ctx, key)
				if i%5 == 0 {
					discardErr(c.Delete(ctx, key))
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestFIFOCacheHitSpanAttributes(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, exporter := newIdemTestCtx(t)
	c := NewFIFOIdempotentCache(4)

	require.NoError(t, c.Set(ctx, "k", "v"))
	_, _ = c.Get(ctx, "k")       // hit
	_, _ = c.Get(ctx, "missing") // miss

	require.Eventually(t, func() bool {
		return exporter.count() >= 2
	}, time.Second, 5*time.Millisecond, "expected idempotent.hit spans")

	var foundHit, foundMiss bool
	var traceID string
	for _, s := range exporter.allSpans() {
		if s.Name != "idempotent.hit" {
			continue
		}
		if traceID == "" {
			traceID = s.TraceID
		} else {
			assert.Equal(t, traceID, s.TraceID, "trace_id must be consistent")
		}
		attrs := map[string]any{}
		for _, a := range s.Attributes {
			attrs[a.Key] = a.Value
		}
		switch attrs["cache_key"] {
		case "k":
			assert.Equal(t, true, attrs["hit"])
			foundHit = true
		case "missing":
			assert.Equal(t, false, attrs["hit"])
			foundMiss = true
		}
	}
	assert.True(t, foundHit, "expected hit span for existing key")
	assert.True(t, foundMiss, "expected miss span for missing key")
}

func TestFIFOCacheSpanChainAndParent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	exporter := newCaptureExporter()
	tr := tracing.NewTracer("idem-trace", exporter)
	root, ctx := tr.Start(context.Background(), "root", tracing.SpanKindInternal)

	c := NewFIFOIdempotentCache(4)
	require.NoError(t, c.Set(ctx, "a", 1))
	_, _ = c.Get(ctx, "a")

	require.Eventually(t, func() bool {
		return exporter.hasSpan("idempotent.hit")
	}, time.Second, 5*time.Millisecond, "expected idempotent.hit span")

	var hit tracing.SpanData
	for _, s := range exporter.allSpans() {
		if s.Name == "idempotent.hit" {
			hit = s
		}
	}
	assert.Equal(t, "idem-trace", hit.TraceID)
	assert.Equal(t, root.SpanID(), hit.ParentSpanID, "parent_span_id must link to the root span")
}

func TestFIFOCacheNameAndDefault(t *testing.T) {
	c := NewFIFOIdempotentCache(4)
	assert.Equal(t, "fifo-idempotent-cache", c.Name())
	assert.Equal(t, "custom-cache", NewFIFOIdempotentCache(4, WithName("custom-cache")).Name())
}

func TestFIFOCacheContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := NewFIFOIdempotentCache(4)

	// A canceled context must not panic and calls still operate.
	require.NoError(t, c.Set(ctx, "a", 1))
	_, ok := c.Get(ctx, "a")
	assert.True(t, ok)
	require.NoError(t, c.Delete(ctx, "a"))
}
