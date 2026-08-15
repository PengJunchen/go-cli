package tracing

import (
	"bufio"
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// readLines reads a file and returns each non-empty line as a byte slice.
// It is shared with other test files in this package.
func readLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var lines [][]byte
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Copy because scanner.Bytes() reuses its buffer.
		cp := make([]byte, len(line))
		copy(cp, line)
		lines = append(lines, cp)
	}
	return lines, scanner.Err()
}

// mockExporter captures exported spans for inspection in tests.
type mockExporter struct {
	mu    sync.Mutex
	spans []SpanData
}

func (m *mockExporter) ExportSpan(_ context.Context, span TraceSpan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spans = append(m.spans, SpanToData(span))
	return nil
}

func (m *mockExporter) Shutdown(_ context.Context) error { return nil }

func (m *mockExporter) getSpans() []SpanData {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]SpanData, len(m.spans))
	copy(cp, m.spans)
	return cp
}

// findAttr returns a pointer to the attribute with the given key, or nil.
func findAttr(attrs []Attribute, key string) *Attribute {
	for i := range attrs {
		if attrs[i].Key == key {
			return &attrs[i]
		}
	}
	return nil
}

// exportOne creates a tracer that exports through a RedactingExporter wrapping
// mock, starts a span, runs setup, ends the span, and returns the captured data.
func exportOne(t *testing.T, mock *mockExporter, level RedactionLevel, setup func(TraceSpan)) SpanData {
	t.Helper()
	re := NewRedactingExporter(mock, level)
	tracer := NewTracer("test-trace", re)
	ctx := tracer.ContextWithTracer(context.Background())
	span, _ := tracer.Start(ctx, "test-op", SpanKindInternal)
	setup(span)
	span.End()
	tracer.Flush()

	spans := mock.getSpans()
	require.Len(t, spans, 1, "expected exactly one exported span")
	return spans[0]
}

func TestRedaction_MasksSensitiveAttributes(t *testing.T) {
	mock := &mockExporter{}
	data := exportOne(t, mock, RedactionLevelRedact, func(span TraceSpan) {
		span.SetAttributes(SensitiveAttribute("secret", "my-api-key"))
		span.SetAttributes(Attribute{Key: "public", Value: "visible"})
		span.AddEvent("checkpoint", SensitiveAttribute("token", "abc123"))
	})

	// Sensitive attribute is masked.
	secret := findAttr(data.Attributes, "secret")
	require.NotNil(t, secret)
	assert.Equal(t, "***REDACTED***", secret.Value)
	assert.True(t, secret.Sensitive)

	// Non-sensitive attribute is not masked.
	public := findAttr(data.Attributes, "public")
	require.NotNil(t, public)
	assert.Equal(t, "visible", public.Value)

	// Sensitive event attribute is also masked.
	require.Len(t, data.Events, 1)
	evToken := findAttr(data.Events[0].Attributes, "token")
	require.NotNil(t, evToken)
	assert.Equal(t, "***REDACTED***", evToken.Value)
}

func TestRedactingExporter_NonSensitiveNotMasked(t *testing.T) {
	mock := &mockExporter{}
	data := exportOne(t, mock, RedactionLevelRedact, func(span TraceSpan) {
		span.SetAttributes(Attribute{Key: "user", Value: "alice"})
		span.SetAttributes(Attribute{Key: "count", Value: 42})
	})

	require.Len(t, data.Attributes, 2)
	for _, attr := range data.Attributes {
		assert.NotEqual(t, redactedValue, attr.Value, "non-sensitive attribute %q should not be masked", attr.Key)
	}
}

func TestRedactionLevelOff_StripsAllAttributes(t *testing.T) {
	mock := &mockExporter{}
	data := exportOne(t, mock, RedactionLevelOff, func(span TraceSpan) {
		span.SetAttributes(SensitiveAttribute("secret", "key"))
		span.SetAttributes(Attribute{Key: "public", Value: "visible"})
		span.AddEvent("evt", Attribute{Key: "data", Value: "x"})
	})

	assert.Nil(t, data.Attributes, "all span attributes should be stripped")
	require.Len(t, data.Events, 1)
	assert.Nil(t, data.Events[0].Attributes, "all event attributes should be stripped")
}

func TestRedactionLevelFull_NoMasking(t *testing.T) {
	mock := &mockExporter{}
	data := exportOne(t, mock, RedactionLevelFull, func(span TraceSpan) {
		span.SetAttributes(SensitiveAttribute("secret", "my-api-key"))
		span.SetAttributes(Attribute{Key: "public", Value: "visible"})
	})

	secret := findAttr(data.Attributes, "secret")
	require.NotNil(t, secret)
	assert.Equal(t, "my-api-key", secret.Value, "sensitive attribute should not be masked at full level")

	public := findAttr(data.Attributes, "public")
	require.NotNil(t, public)
	assert.Equal(t, "visible", public.Value)
}

// stringErrorExporter fails every ExportSpan with a caller-supplied message so
// tests can distinguish failures from different exporters.
type stringErrorExporter struct {
	msg string
}

func (e *stringErrorExporter) ExportSpan(context.Context, TraceSpan) error {
	return errors.New(e.msg)
}

func (e *stringErrorExporter) Shutdown(context.Context) error { return nil }

// TestMultiExporterAggregatesAllErrors verifies that when multiple exporters
// fail, ExportSpan returns an error aggregating every failure (via errors.Join)
// rather than only the last one, while still attempting every exporter.
func TestMultiExporterAggregatesAllErrors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	failA := &stringErrorExporter{msg: "exporter-A-failed"}
	ok := &recordingExporter{}
	failB := &stringErrorExporter{msg: "exporter-B-failed"}

	m := NewMultiExporter(failA, ok, failB)
	tr := NewTracer("trace-agg", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	err := m.ExportSpan(context.Background(), span)
	require.Error(t, err)

	// Every exporter must have been attempted despite failures.
	require.Len(t, ok.got, 1, "pass-through exporter must still receive the span")

	// The aggregated error must contain both distinct failures; errors.Join
	// concatenates the wrapped errors' messages.
	msg := err.Error()
	assert.Contains(t, msg, "exporter-A-failed", "first failure must be present")
	assert.Contains(t, msg, "exporter-B-failed", "second failure must be present")
}

// TestMultiExporterNoErrorsReturnsNil verifies that when every exporter
// succeeds, ExportSpan returns nil.
func TestMultiExporterNoErrorsReturnsNil(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ok1 := &recordingExporter{}
	ok2 := &recordingExporter{}
	m := NewMultiExporter(ok1, ok2)

	tr := NewTracer("trace-noerr", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	err := m.ExportSpan(context.Background(), span)
	require.NoError(t, err, "no errors when all exporters succeed")
	require.Len(t, ok1.got, 1)
	require.Len(t, ok2.got, 1)
}
