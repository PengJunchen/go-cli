package tracing

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingExporter collects spans synchronously for deterministic tests.
// It is concurrency-safe because localSpan.End() may call ExportSpan from a
// background goroutine while the test reads the collected spans.
type recordingExporter struct {
	mu              sync.Mutex
	got             []TraceSpan
	shutdownCalled  bool
}

func (r *recordingExporter) ExportSpan(_ context.Context, span TraceSpan) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, span)
	return nil
}

func (r *recordingExporter) Shutdown(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shutdownCalled = true
	return nil
}

// Len returns the number of collected spans in a concurrency-safe manner.
func (r *recordingExporter) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func newTestTracer() *Tracer {
	return NewTracer("test-trace-id", nil)
}

func TestTracerStartNodeInheritance(t *testing.T) {
	tr := newTestTracer()

	root, ctx := tr.Start(context.Background(), "cli.invocation", SpanKindInternal)
	require.Equal(t, "test-trace-id", root.TraceID())
	require.Equal(t, "test-trace-id", tr.TraceID())
	require.Empty(t, root.ParentSpanID(), "root span has no parent")

	child, _ := tr.Start(ctx, "config.load", SpanKindInternal)
	require.Equal(t, root.SpanID(), child.ParentSpanID(), "child inherits parent span id")
	require.Equal(t, root.TraceID(), child.TraceID(), "child shares the same trace id")

	grandchild, _ := tr.Start(child.Context(), "config.validate", SpanKindInternal)
	require.Equal(t, child.SpanID(), grandchild.ParentSpanID())
}

func TestTracerStartReturnsDistinctIDs(t *testing.T) {
	tr := newTestTracer()
	ctx := context.Background()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		span, ctx2 := tr.Start(ctx, "op", SpanKindInternal)
		ctx = ctx2
		require.False(t, seen[span.SpanID()], "span ids must be unique")
		seen[span.SpanID()] = true
	}
}

func TestSpanFromContextCreatesChain(t *testing.T) {
	tr := newTestTracer()
	ctx := context.Background()

	span, ctx := SpanFromContext(ctx, "root", SpanKindInternal)
	// No tracer in a bare context -> noop span.
	assert.IsType(t, (*noopSpan)(nil), span)

	// Start a real root so the tracer is injected into ctx.
	root, ctx := tr.Start(ctx, "cli.invocation", SpanKindInternal)

	child, _ := SpanFromContext(ctx, "config.load", SpanKindInternal)
	require.Equal(t, root.SpanID(), child.ParentSpanID())
	_, isNoop := child.(*noopSpan)
	assert.False(t, isNoop, "expected a real span, got noopSpan")
}

func TestTracerDisabledReturnsNoopSpan(t *testing.T) {
	rec := &recordingExporter{}
	tr := NewTracer("", rec)
	tr.SetEnabled(false)

	span, ctx := tr.Start(context.Background(), "op", SpanKindInternal)
	require.IsType(t, (*noopSpan)(nil), span)
	// Should not export anything.
	span.End()
	span.SetAttributes(Attribute{Key: "k", Value: "v"})
	span.AddEvent("event")
	span.SetStatus(SpanStatusError, "boom")
	require.Eventually(t, func() bool {
		// Wait a short while to let the async goroutine finish (if it were
		// going to export). The recordingExporter is concurrency-safe.
		time.Sleep(10 * time.Millisecond)
		return rec.Len() == 0
	}, 200*time.Millisecond, 10*time.Millisecond, "disabled tracer must not export")

	child, _ := SpanFromContext(ctx, "child", SpanKindInternal)
	require.IsType(t, (*noopSpan)(nil), child, "disabled tracer does not start real spans")
}

func TestNoopSpanAllMethodsAreNoOps(t *testing.T) {
	s := &noopSpan{}
	assert.Equal(t, "", s.TraceID())
	assert.Equal(t, "", s.SpanID())
	assert.Equal(t, "", s.ParentSpanID())
	assert.Equal(t, "", s.Name())
	assert.Equal(t, time.Time{}, s.StartTime())
	assert.Equal(t, time.Time{}, s.EndTime())
	s.SetAttributes(Attribute{Key: "a", Value: "b"})
	s.AddEvent("e")
	s.SetStatus(SpanStatusError, "m")
	s.End()
	assert.Equal(t, context.Background(), s.Context())
}

func TestLocalSpanSetStatusAndData(t *testing.T) {
	tr := newTestTracer()
	span, ctx := tr.Start(context.Background(), "task", SpanKindClient)
	defer span.End()

	span.SetAttributes(
		Attribute{Key: "model", Value: "gpt-4"},
		Attribute{Key: "tokens", Value: 100},
	)
	span.AddEvent("retry", Attribute{Key: "attempt", Value: 1})
	span.SetStatus(SpanStatusError, "timeout")
	span.End()

	ls, ok := span.(*localSpan)
	require.True(t, ok)
	data := ls.ToSpanData()
	require.Equal(t, "test-trace-id", data.TraceID)
	assert.Equal(t, SpanStatusError, data.Status)
	assert.Equal(t, "timeout", data.StatusMessage)
	assert.Equal(t, SpanKindClient, data.SpanKind)
	assert.Len(t, data.Attributes, 2)
	assert.Len(t, data.Events, 1)
	assert.False(t, data.StartTime == "")
	assert.False(t, data.EndTime == "")
	// ctx is the one returned by Start.
	assert.Equal(t, ctx, ls.ctx)
}

func TestLocalSpanDefaultStatusOK(t *testing.T) {
	tr := newTestTracer()
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	span.End()
	ls, ok := span.(*localSpan)
	require.True(t, ok)
	data := ls.ToSpanData()
	assert.Equal(t, SpanStatusOK, data.Status)
}

func TestEndIsIdempotentWithSyncExporter(t *testing.T) {
	rec := &recordingExporter{}
	tr := NewTracer("", rec)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	span.End()
	span.End() // second call is a no-op
	// localSpan.End exports asynchronously; wait for the goroutine.
	require.Eventually(t, func() bool {
		return rec.Len() == 1
	}, time.Second, 5*time.Millisecond, "expected exactly one export")
}

func TestContextCancellationStillEndsSpan(t *testing.T) {
	rec := &recordingExporter{}
	tr := NewTracer("", rec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	span, _ := tr.Start(ctx, "op", SpanKindInternal)
	require.NotNil(t, span)
	span.SetStatus(SpanStatusOK, "")
	span.End()

	require.Eventually(t, func() bool {
		return rec.Len() == 1
	}, time.Second, 5*time.Millisecond)
}

func TestAttributeJSONTags(t *testing.T) {
	b, err := json.Marshal(Attribute{Key: "k", Value: "v"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"key":"k","value":"v"}`, string(b))
}

func TestInternalSpanEndsConcurrent(t *testing.T) {
	tr := newTestTracer()
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				span.SetAttributes(Attribute{Key: "x", Value: 1})
				span.AddEvent("e")
				span.SetStatus(SpanStatusError, "m")
			}
			_ = span.SpanID()
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	span.End()
	ls, ok := span.(*localSpan)
	require.True(t, ok)
	_ = ls.ToSpanData()
}
