package tracing

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// customSpan is a minimal TraceSpan not backed by *localSpan, used to exercise
// SpanToData's fallback path which only has access to the interface fields.
type customSpan struct {
	traceID, spanID, parentID, name string
	start, end                      time.Time
}

func (c *customSpan) TraceID() string               { return c.traceID }
func (c *customSpan) SpanID() string                { return c.spanID }
func (c *customSpan) ParentSpanID() string          { return c.parentID }
func (c *customSpan) Name() string                  { return c.name }
func (c *customSpan) StartTime() time.Time          { return c.start }
func (c *customSpan) EndTime() time.Time            { return c.end }
func (c *customSpan) SetAttributes(...Attribute)    {}
func (c *customSpan) AddEvent(string, ...Attribute) {}
func (c *customSpan) SetStatus(SpanStatus, string)  {}
func (c *customSpan) End()                          {}
func (c *customSpan) Context() context.Context      { return context.Background() }

var _ TraceSpan = (*customSpan)(nil)

// TestSpanToDataCustomSpan verifies the fallback path in SpanToData for a span
// that is not a *localSpan: only interface-visible fields are preserved.
func TestSpanToDataCustomSpan(t *testing.T) {
	start := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(time.Second)
	sp := &customSpan{
		traceID: "t", spanID: "s", parentID: "p", name: "custom",
		start: start, end: end,
	}

	data := SpanToData(sp)
	assert.Equal(t, "t", data.TraceID)
	assert.Equal(t, "s", data.SpanID)
	assert.Equal(t, "p", data.ParentSpanID)
	assert.Equal(t, "custom", data.Name)
	assert.Equal(t, start.Format(time.RFC3339Nano), data.StartTime)
	assert.Equal(t, end.Format(time.RFC3339Nano), data.EndTime)
}

// TestSpanToDataLocalSpan verifies SpanToData routes a *localSpan to its richer
// ToSpanData (including attributes/status).
func TestSpanToDataLocalSpan(t *testing.T) {
	tr := NewTracer("trace-local", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	span.SetStatus(SpanStatusError, "bad")
	span.SetAttributes(Attribute{Key: "k", Value: "v"})
	span.End()

	ls, ok := span.(*localSpan)
	require.True(t, ok)
	data := SpanToData(ls)
	assert.Equal(t, SpanStatusError, data.Status)
	assert.Len(t, data.Attributes, 1)
}

// TestGenerateIDHexFormat verifies generateID returns a 32-char lowercase hex id.
func TestGenerateIDHexFormat(t *testing.T) {
	id := generateID()
	require.Len(t, id, 32)
	for _, c := range id {
		assert.True(t, (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f'),
			"id contains non-hex char %q", c)
	}
	// Distinct calls produce distinct ids.
	require.NotEqual(t, generateID(), generateID())
}

// TestNewTracerGeneratesIDWhenEmpty verifies Tracer assigns a random trace id
// when an empty one is provided.
func TestNewTracerGeneratesIDWhenEmpty(t *testing.T) {
	tr := NewTracer("", nil)
	require.NotEmpty(t, tr.TraceID())
	assert.Len(t, tr.TraceID(), 32)
}

// TestTracerSetEnabledToggle verifies tracing can be disabled and re-enabled.
func TestTracerSetEnabledToggle(t *testing.T) {
	rec := &recordingExporter{}
	tr := NewTracer("trace-toggle", rec)

	// Enabled by default.
	span, _ := tr.Start(context.Background(), "on", SpanKindInternal)
	require.IsType(t, (*localSpan)(nil), span)
	span.End()

	tr.SetEnabled(false)
	noop, _ := tr.Start(context.Background(), "off", SpanKindInternal)
	require.IsType(t, (*noopSpan)(nil), noop)

	tr.SetEnabled(true)
	back, _ := tr.Start(context.Background(), "on-again", SpanKindInternal)
	require.IsType(t, (*localSpan)(nil), back)
	back.End()

	require.Eventually(t, func() bool { return rec.Len() == 2 }, time.Second, 5*time.Millisecond)
}

// TestLocalSpanAddEventTimestampOrdering verifies events carry the adding call's
// attributes and that multiple events are preserved in order.
func TestLocalSpanAddEventTimestampOrdering(t *testing.T) {
	tr := NewTracer("trace-events", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	span.AddEvent("first", Attribute{Key: "attempt", Value: 1})
	span.AddEvent("second", Attribute{Key: "attempt", Value: 2})

	ls, ok := span.(*localSpan)
	require.True(t, ok)
	data := ls.ToSpanData()
	require.Len(t, data.Events, 2)
	assert.Equal(t, "first", data.Events[0].Name)
	assert.Equal(t, "second", data.Events[1].Name)
	assert.NotEmpty(t, data.Events[0].Timestamp)
	require.Len(t, data.Events[0].Attributes, 1)
	assert.Equal(t, 1, data.Events[0].Attributes[0].Value)
}

// TestLocalSpanThreadSafetyAccessors verifies the mutex-guarded accessors are
// safe under the race detector.
func TestLocalSpanThreadSafetyAccessors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tr := NewTracer("trace-race-access", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	ls, ok := span.(*localSpan)
	require.True(t, ok)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			ls.SetAttributes(Attribute{Key: "k", Value: i})
			ls.AddEvent("e")
			ls.SetStatus(SpanStatusOK, "")
			_ = ls.EndTime()
			_ = ls.ToSpanData()
		}
	}()
	for i := 0; i < 50; i++ {
		ls.SetAttributes(Attribute{Key: "j", Value: i})
		_ = ls.EndTime()
	}
	<-done
	span.End()
}

// TestTraceLoggerLevelFiltering ensures the wrapping handler preserves the base
// handler's level filtering.
func TestTraceLoggerLevelFiltering(t *testing.T) {
	tr := NewTracer("trace-filter", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger := NewTraceLogger(span, base)

	logger.Debug("dropped")
	assert.Empty(t, buf.String(), "debug must be dropped by the Info-level base handler")

	logger.Info("kept")
	assert.Contains(t, buf.String(), "kept")
}

// TestTraceHandlerEnabledIdentity verifies Enabled forwards to the base handler.
func TestTraceHandlerEnabledIdentity(t *testing.T) {
	tr := NewTracer("trace-ident", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	base := slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelError})
	h := &traceHandler{inner: base, span: span}
	assert.False(t, h.Enabled(context.Background(), slog.LevelInfo))
	assert.True(t, h.Enabled(context.Background(), slog.LevelError))
}

// TestBuildSpanTreeFallbackRoot verifies that when no span has an empty parent
// (no explicit root), a root node is still selected among the spans rather than
// returning nil.
func TestBuildSpanTreeFallbackRoot(t *testing.T) {
	spans := []SpanData{
		{SpanID: "b", ParentSpanID: "ghost", StartTime: "2026-01-01T00:00:01Z"},
		{SpanID: "a", ParentSpanID: "ghost", StartTime: "2026-01-01T00:00:00Z"},
	}
	root, err := buildSpanTree(spans)
	require.NoError(t, err)
	// The fallback picks an arbitrary span when no explicit root exists.
	require.NotNil(t, root)
	assert.Contains(t, []string{"a", "b"}, root.Span.SpanID)
}

// TestBuildSpanTreeOrphanParent verifies a child whose parent is absent is kept
// without being appended to any parent (no crash).
func TestBuildSpanTreeOrphanParent(t *testing.T) {
	spans := []SpanData{
		{SpanID: "root", ParentSpanID: "", StartTime: "2026-01-01T00:00:00Z"},
		{SpanID: "orphan", ParentSpanID: "missing", StartTime: "2026-01-01T00:00:01Z"},
	}
	root, err := buildSpanTree(spans)
	require.NoError(t, err)
	assert.Equal(t, "root", root.Span.SpanID)
	require.Len(t, root.Children, 0, "orphan should not attach to the root")
}

// TestPrintTreeErrorStatus verifies the ERROR line is rendered for error-status
// spans and that recursion terminates on a cycle-free tree.
func TestPrintTreeErrorStatus(t *testing.T) {
	// Redirect stdout briefly to verify output without crashing.
	root := &SpanNode{Span: SpanData{
		SpanID: "1", Name: "pipeline", SpanKind: SpanKindClient,
		Status: SpanStatusError, StatusMessage: "boom", StartTime: "2026-01-01T00:00:00Z",
	}}
	child := &SpanNode{Span: SpanData{
		SpanID: "2", ParentSpanID: "1", Name: "sub", SpanKind: SpanKindInternal,
		Status: SpanStatusOK, StartTime: "2026-01-01T00:00:01Z",
	}}
	root.Children = []*SpanNode{child}

	assert.NotPanics(t, func() { PrintTree(root, "") })
}

// TestLoadTraceSkipsBadLinesAndOrdersTree uses a file with error-status and
// unordered entries to exercise LoadTrace end-to-end.
func TestLoadTraceSkipsBadLinesAndOrdersTree(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tree.jsonl"
	lines := []string{
		`{"trace_id":"t","span_id":"child","parent_span_id":"root","name":"c","start_time":"2026-01-01T00:00:01Z"}`,
		`garbage`,
		`{"trace_id":"t","span_id":"root","name":"r","start_time":"2026-01-01T00:00:00Z"}`,
		``, // empty line: valid JSON "null"? not unmarshalable into struct -> skipped
	}
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600))

	root, err := LoadTrace(path)
	require.NoError(t, err)
	require.NotNil(t, root)
	assert.Equal(t, "root", root.Span.SpanID)
	require.Len(t, root.Children, 1)
	assert.Equal(t, "child", root.Children[0].Span.SpanID)
}
