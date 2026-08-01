package mock

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// MockTraceExporter is an in-memory tracing.TraceExporter that collects every
// exported span for assertions. It is used by the conversation runner and
// tests to verify trace completeness and chain integrity.
type MockTraceExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

// Compile-time assertion that the mock exporter satisfies the trace contract.
var _ tracing.TraceExporter = (*MockTraceExporter)(nil)

// NewMockTraceExporter creates an empty mock trace exporter.
func NewMockTraceExporter() *MockTraceExporter {
	return &MockTraceExporter{}
}

// ExportSpan collects the span into the in-memory store.
func (e *MockTraceExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanData{
		TraceID:      span.TraceID(),
		SpanID:       span.SpanID(),
		ParentSpanID: span.ParentSpanID(),
		Name:         span.Name(),
		StartTime:    span.StartTime().Format(time.RFC3339Nano),
		EndTime:      span.EndTime().Format(time.RFC3339Nano),
	})
	return nil
}

// Shutdown is a no-op for the in-memory exporter.
func (e *MockTraceExporter) Shutdown(_ context.Context) error { return nil }

// Spans returns a copy of all collected spans.
func (e *MockTraceExporter) Spans() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]tracing.SpanData, len(e.spans))
	copy(result, e.spans)
	return result
}

// SpanCount returns the number of collected spans.
func (e *MockTraceExporter) SpanCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.spans)
}

// Reset clears all collected spans.
func (e *MockTraceExporter) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = nil
}

// AssertSpanExists fails the test unless a span with the given name was
// collected.
func (e *MockTraceExporter) AssertSpanExists(t verify.TestingT, name string) {
	t.Helper()
	for _, span := range e.Spans() {
		if span.Name == name {
			return
		}
	}
	t.Fatalf("expected span with name %q, not found in %d spans", name, e.SpanCount())
}

// AssertSpanChain fails the test unless every span's parent_span_id resolves
// to a collected span and at least one root span (empty parent_span_id) is
// present.
func (e *MockTraceExporter) AssertSpanChain(t verify.TestingT) {
	t.Helper()
	spans := e.Spans()
	if len(spans) == 0 {
		t.Fatal("no spans exported")
	}

	spanMap := make(map[string]tracing.SpanData, len(spans))
	for _, span := range spans {
		spanMap[span.SpanID] = span
	}

	var problems []string
	hasRoot := false
	for _, span := range spans {
		if span.ParentSpanID == "" {
			hasRoot = true
			continue
		}
		if _, ok := spanMap[span.ParentSpanID]; !ok {
			problems = append(problems, fmt.Sprintf("span %s (%s) references missing parent %s",
				span.SpanID, span.Name, span.ParentSpanID))
		}
	}
	if !hasRoot {
		problems = append(problems, "no root span found (all spans have parent_span_id)")
	}
	if len(problems) > 0 {
		t.Fatalf("span chain integrity violated: %s", strings.Join(problems, "; "))
	}
}
