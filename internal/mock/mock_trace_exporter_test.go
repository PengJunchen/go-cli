package mock

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// recordingTestingT is a verify.TestingT that records a failure instead of
// terminating the test. It is used to exercise the failure paths of the mock
// assertion helpers (which call Fatal/Fatalf) without aborting the test.
type recordingTestingT struct {
	failed bool
}

func (r *recordingTestingT) Helper()               {}
func (r *recordingTestingT) Fatal(...any)          { r.failed = true }
func (r *recordingTestingT) Fatalf(string, ...any) { r.failed = true }
func (r *recordingTestingT) Logf(string, ...any)   {}

// stubSpan is a minimal tracing.TraceSpan used to inject spans with controlled
// parent/sibling relationships directly into the exporter.
type stubSpan struct {
	traceID      string
	spanID       string
	parentSpanID string
	name         string
	start        time.Time
	end          time.Time
}

func (s *stubSpan) TraceID() string                       { return s.traceID }
func (s *stubSpan) SpanID() string                        { return s.spanID }
func (s *stubSpan) ParentSpanID() string                  { return s.parentSpanID }
func (s *stubSpan) Name() string                          { return s.name }
func (s *stubSpan) StartTime() time.Time                  { return s.start }
func (s *stubSpan) EndTime() time.Time                    { return s.end }
func (s *stubSpan) SetAttributes(...tracing.Attribute)    {}
func (s *stubSpan) AddEvent(string, ...tracing.Attribute) {}
func (s *stubSpan) SetStatus(tracing.SpanStatus, string)  {}
func (s *stubSpan) End()                                  {}
func (s *stubSpan) Context() context.Context              { return context.Background() }

// exportSpan injects a stub span into the exporter.
func exportSpan(t *testing.T, e *MockTraceExporter, s *stubSpan) {
	t.Helper()
	require.NoError(t, e.ExportSpan(context.Background(), s))
}

func TestMockTraceExporterCollectsSpans(t *testing.T) {
	e := NewMockTraceExporter()
	exportSpan(t, e, &stubSpan{spanID: "a", name: "cli.invocation"})
	exportSpan(t, e, &stubSpan{spanID: "b", name: "llm.request"})

	assert.Equal(t, 2, e.SpanCount())
	spans := e.Spans()
	assert.Equal(t, "cli.invocation", spans[0].Name)
	assert.Equal(t, "llm.request", spans[1].Name)
}

func TestMockTraceExporterAssertSpanExists(t *testing.T) {
	e := NewMockTraceExporter()
	exportSpan(t, e, &stubSpan{spanID: "x", name: "tool.call"})

	ok := &recordingTestingT{}
	e.AssertSpanExists(ok, "tool.call")
	assert.False(t, ok.failed, "existing span should not fail")

	bad := &recordingTestingT{}
	e.AssertSpanExists(bad, "missing")
	assert.True(t, bad.failed, "missing span should fail the assertion")
}

func TestMockTraceExporterAssertSpanChainValid(t *testing.T) {
	e := NewMockTraceExporter()
	exportSpan(t, e, &stubSpan{spanID: "root", name: "cli.invocation"})
	exportSpan(t, e, &stubSpan{spanID: "a", parentSpanID: "root", name: "llm.request"})
	exportSpan(t, e, &stubSpan{spanID: "b", parentSpanID: "a", name: "tool.call"})

	e.AssertSpanChain(t) // must not fail
}

func TestMockTraceExporterAssertSpanChainMissingParent(t *testing.T) {
	e := NewMockTraceExporter()
	exportSpan(t, e, &stubSpan{spanID: "root", name: "cli.invocation"})
	// This span references an undeclared parent.
	exportSpan(t, e, &stubSpan{spanID: "c", parentSpanID: "ghost", name: "llm.request"})

	rec := &recordingTestingT{}
	e.AssertSpanChain(rec)
	assert.True(t, rec.failed, "dangling parent should fail the assertion")
}

func TestMockTraceExporterAssertSpanChainNoRoot(t *testing.T) {
	e := NewMockTraceExporter()
	exportSpan(t, e, &stubSpan{spanID: "a", parentSpanID: "b", name: "x"})
	exportSpan(t, e, &stubSpan{spanID: "b", parentSpanID: "c", name: "y"})

	rec := &recordingTestingT{}
	e.AssertSpanChain(rec)
	assert.True(t, rec.failed, "no root span should fail the assertion")
}

func TestMockTraceExporterReset(t *testing.T) {
	e := NewMockTraceExporter()
	exportSpan(t, e, &stubSpan{spanID: "s", name: "n"})
	assert.Equal(t, 1, e.SpanCount())

	e.Reset()
	assert.Equal(t, 0, e.SpanCount())
}

// TestMockTraceExporterIntegrationViaTracer verifies the real Tracer drives the
// exporter (async export; spans become visible via polling).
func TestMockTraceExporterIntegrationViaTracer(t *testing.T) {
	e := NewMockTraceExporter()
	tracer := tracing.NewTracer("trace-1", e)

	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)
	child, _ := tracer.Start(ctx, "llm.request", tracing.SpanKindClient)
	child.SetStatus(tracing.SpanStatusOK, "")
	child.SetAttributes(tracing.Attribute{Key: "model", Value: "mock"})
	child.End()
	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.SpanCount() >= 2 }, 2*time.Second, 10*time.Millisecond)

	e.AssertSpanExists(t, "cli.invocation")
	e.AssertSpanExists(t, "llm.request")
	e.AssertSpanChain(t)
}
