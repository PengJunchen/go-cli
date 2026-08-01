package tracing

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONLTraceExporterWritesOneLinePerSpan(t *testing.T) {
	dir := t.TempDir()
	exp, err := NewJSONLTraceExporter(dir, "session-1")
	require.NoError(t, err)

	tr := NewTracer("trace-abc", exp)

	spans := []TraceSpan{}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		span, ctx2 := tr.Start(ctx, "op", SpanKindInternal)
		ctx = ctx2
		span.SetAttributes(Attribute{Key: "idx", Value: i})
		span.SetStatus(SpanStatusOK, "")
		spans = append(spans, span)
	}

	for _, sp := range spans {
		require.NoError(t, exp.ExportSpan(context.Background(), sp))
	}
	require.NoError(t, exp.Shutdown(context.Background()))

	// Each line must be valid JSON with expected fields.
	lines, err := readLines(exp.FilePath())
	require.NoError(t, err)
	require.Len(t, lines, 3, "expected one JSON line per span")

	for i, line := range lines {
		var span SpanData
		require.NoError(t, json.Unmarshal(line, &span))
		assert.Equal(t, "trace-abc", span.TraceID)
		assert.Equal(t, SpanKindInternal, span.SpanKind)
		assert.Equal(t, "op", span.Name)
		assert.Equal(t, SpanStatusOK, span.Status)
		if i > 0 {
			// children inherit parent from the previous span
			assert.Equal(t, spans[i-1].SpanID(), span.ParentSpanID)
		} else {
			assert.Empty(t, span.ParentSpanID)
		}
	}
}

func TestJSONLTraceExporterEndExportsAsync(t *testing.T) {
	dir := t.TempDir()
	exp, err := NewJSONLTraceExporter(dir, "session-async")
	require.NoError(t, err)
	tr := NewTracer("trace-x", exp)

	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	span.End()
	span.End() // idempotent

	require.Eventually(t, func() bool {
		lines, err := readLines(exp.FilePath())
		return err == nil && len(lines) == 1
	}, time.Second, 5*time.Millisecond)
	require.NoError(t, exp.Shutdown(context.Background()))
}

func TestStdoutTraceExporterPrefixedOutput(t *testing.T) {
	var sb strings.Builder
	exp := NewStdoutTraceExporterWithWriter(false, &sb)

	tr := NewTracer("trace-stdout", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindClient)
	span.SetStatus(SpanStatusOK, "")
	require.NoError(t, exp.ExportSpan(context.Background(), span))

	out := sb.String()
	require.True(t, strings.HasPrefix(out, "[TRACE] "), "output %q must be prefixed", out)
	line := strings.TrimPrefix(out, "[TRACE] ")
	var data SpanData
	require.NoError(t, json.Unmarshal([]byte(line), &data))
	assert.Equal(t, "trace-stdout", data.TraceID)
	assert.Equal(t, SpanKindClient, data.SpanKind)
}

func TestMultiExporterFansOut(t *testing.T) {
	r1 := &recordingExporter{}
	r2 := &recordingExporter{}
	m := NewMultiExporter(r1, r2)

	tr := NewTracer("trace-multi", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)
	require.NoError(t, m.ExportSpan(context.Background(), span))
	require.Len(t, r1.got, 1)
	require.Len(t, r2.got, 1)
}

func readLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() //nolint:errcheck // test read helper is best-effort

	scanner := bufio.NewScanner(f)
	var lines [][]byte
	for scanner.Scan() {
		lines = append(lines, append([]byte(nil), scanner.Bytes()...))
	}
	return lines, scanner.Err()
}
