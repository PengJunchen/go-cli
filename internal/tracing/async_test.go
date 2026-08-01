package tracing

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// countingExporter is a test helper that counts exported spans.
type countingExporter struct {
	onExport func()
}

func (e *countingExporter) ExportSpan(_ context.Context, _ TraceSpan) error {
	if e.onExport != nil {
		e.onExport()
	}
	return nil
}

func (e *countingExporter) Shutdown(context.Context) error { return nil }

func TestAsyncExporterFlushesOnBatchSize(t *testing.T) {
	var exported atomic.Int32
	inner := &countingExporter{onExport: func() { exported.Add(1) }}

	async := NewAsyncExporter(inner, 100, 3)
	defer func() { _ = async.Shutdown(context.Background()) }() //nolint:errcheck // test cleanup is best-effort

	tr := NewTracer("trace-async-batch", async)
	for i := 0; i < 3; i++ {
		span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
		span.SetStatus(SpanStatusOK, "")
		require.NoError(t, async.ExportSpan(context.Background(), span))
	}

	require.Eventually(t, func() bool {
		return exported.Load() == 3
	}, 2*time.Second, 10*time.Millisecond, "expected 3 exports after batch fills")
}

func TestAsyncExporterFlushesOnTimer(t *testing.T) {
	var exported atomic.Int32
	inner := &countingExporter{onExport: func() { exported.Add(1) }}

	async := newAsyncExporter(inner, 100, 10, 50*time.Millisecond)
	defer func() { _ = async.Shutdown(context.Background()) }() //nolint:errcheck // test cleanup is best-effort

	tr := NewTracer("trace-async-timer", async)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	span.SetStatus(SpanStatusOK, "")
	require.NoError(t, async.ExportSpan(context.Background(), span))

	require.Eventually(t, func() bool {
		return exported.Load() == 1
	}, 2*time.Second, 10*time.Millisecond, "expected 1 export after flush interval")
}

func TestAsyncExporterShutdownDrains(t *testing.T) {
	var exported atomic.Int32
	inner := &countingExporter{onExport: func() { exported.Add(1) }}

	async := NewAsyncExporter(inner, 100, 50)
	tr := NewTracer("trace-async-drain", async)

	for i := 0; i < 5; i++ {
		span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
		span.SetStatus(SpanStatusOK, "")
		require.NoError(t, async.ExportSpan(context.Background(), span))
	}

	require.NoError(t, async.Shutdown(context.Background()))
	assert.Equal(t, int32(5), exported.Load(), "shutdown should drain all queued spans")
}

func TestAsyncExporterDropsWhenFull(t *testing.T) {
	// Use a very small queue and a slow consumer to force drops.
	var exported atomic.Int32
	blockCh := make(chan struct{})
	inner := &countingExporter{onExport: func() {
		<-blockCh // block until test releases
		exported.Add(1)
	}}

	async := NewAsyncExporter(inner, 2, 50)
	defer func() {
		close(blockCh)
		_ = async.Shutdown(context.Background()) //nolint:errcheck // test cleanup is best-effort
	}()

	tr := NewTracer("trace-async-drop", async)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	span.SetStatus(SpanStatusOK, "")

	dropped := 0
	for i := 0; i < 10; i++ {
		s, _ := tr.Start(context.Background(), "op", SpanKindInternal)
		s.SetStatus(SpanStatusOK, "")
		if err := async.ExportSpan(context.Background(), s); err != nil {
			dropped++
		}
	}
	assert.Equal(t, 0, dropped, "ExportSpan should never return an error; drops are silent")
}

func TestAsyncExporterBatchSizeMinimum(t *testing.T) {
	var exported atomic.Int32
	inner := &countingExporter{onExport: func() { exported.Add(1) }}

	async := NewAsyncExporter(inner, 100, 0)                    // batchSize < 1 should be clamped to 1
	defer func() { _ = async.Shutdown(context.Background()) }() //nolint:errcheck // test cleanup is best-effort

	tr := NewTracer("trace-async-min", async)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	span.SetStatus(SpanStatusOK, "")
	require.NoError(t, async.ExportSpan(context.Background(), span))

	require.Eventually(t, func() bool {
		return exported.Load() == 1
	}, 2*time.Second, 10*time.Millisecond)
}

// gatedExporter blocks each ExportSpan until release() is called, and records
// the spans in FIFO order. It is used to deterministically force a full queue.
type gatedExporter struct {
	mu    sync.Mutex
	spans []TraceSpan
	first chan struct{}
	gate  chan struct{}
}

func newGatedExporter() *gatedExporter {
	return &gatedExporter{
		first: make(chan struct{}),
		gate:  make(chan struct{}),
	}
}

func (g *gatedExporter) ExportSpan(_ context.Context, span TraceSpan) error {
	select {
	case <-g.first:
	default:
		close(g.first)
	}
	<-g.gate
	g.mu.Lock()
	g.spans = append(g.spans, span)
	g.mu.Unlock()
	return nil
}

func (g *gatedExporter) Shutdown(context.Context) error { return nil }

// waitStarted blocks until the worker has begun the first export.
func (g *gatedExporter) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-g.first:
	case <-time.After(time.Second):
		t.Fatal("worker did not begin exporting in time")
	}
}

func (g *gatedExporter) release() { g.gate <- struct{}{} }

func (g *gatedExporter) ids() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ids := make([]string, 0, len(g.spans))
	for _, s := range g.spans {
		ids = append(ids, s.SpanID())
	}
	return ids
}

func (g *gatedExporter) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.spans)
}

func TestAsyncExporterDropsWhenQueueFull(t *testing.T) {
	g := newGatedExporter()
	async := NewAsyncExporter(g, 1, 1) // queue holds 1, batch flushes immediately

	tr := NewTracer("trace", nil)
	spanA, _ := tr.Start(context.Background(), "a", SpanKindInternal)
	spanB, _ := tr.Start(context.Background(), "b", SpanKindInternal)
	spanC, _ := tr.Start(context.Background(), "c", SpanKindInternal)

	require.NoError(t, async.ExportSpan(context.Background(), spanA))
	g.waitStarted(t) // worker is now blocked exporting A (spanCh is empty)

	require.NoError(t, async.ExportSpan(context.Background(), spanB)) // fills the single slot

	start := time.Now()
	require.NoError(t, async.ExportSpan(context.Background(), spanC)) // queue full -> dropped
	require.Less(t, time.Since(start), 200*time.Millisecond, "drop must not block the caller")

	// Let A and B through; C must never be delivered.
	g.release()
	g.release()
	require.Eventually(t, func() bool { return g.count() == 2 }, time.Second, 5*time.Millisecond)

	require.NoError(t, async.Shutdown(context.Background()))

	ids := g.ids()
	require.Len(t, ids, 2, "only two spans should be exported; the third was dropped")
	require.Contains(t, ids, spanA.SpanID())
	require.Contains(t, ids, spanB.SpanID())
	require.NotContains(t, ids, spanC.SpanID())
}

func TestAsyncExporterShutdownFlushesBufferedSpans(t *testing.T) {
	rec := &recordingExporter{}
	// Batch size larger than the number of spans, so final delivery must come
	// from the Shutdown drain (not from the batch-size trigger).
	async := NewAsyncExporter(rec, 64, 100)

	tr := NewTracer("trace", nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		span, c := tr.Start(ctx, "op", SpanKindInternal)
		ctx = c
		require.NoError(t, async.ExportSpan(context.Background(), span))
	}

	require.NoError(t, async.Shutdown(context.Background()))
	require.Len(t, rec.got, 5, "Shutdown must flush all buffered spans")
}

func TestAsyncExporterNoGoroutineLeak(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	rec := &recordingExporter{}
	for i := 0; i < 10; i++ {
		async := NewAsyncExporter(rec, 8, 4)
		require.NoError(t, async.ExportSpan(context.Background(), &localSpan{spanID: "sig"}))
		require.NoError(t, async.Shutdown(context.Background()))
	}
}
