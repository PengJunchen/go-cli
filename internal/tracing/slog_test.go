package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTraceLoggerInjectsTraceFields(t *testing.T) {
	// Build a tracer + span so we have real trace/span IDs.
	tr := NewTracer("trace-slog-1", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	base := slog.New(handler)

	logger := NewTraceLogger(span, base)
	logger.Info("hello", "key", "value")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))

	assert.Equal(t, "trace-slog-1", record["trace_id"], "trace_id must be injected")
	assert.NotEmpty(t, record["span_id"], "span_id must be injected")
	// Root span has no parent, so parent_span_id should be absent.
	_, hasParent := record["parent_span_id"]
	assert.False(t, hasParent, "root span should not have parent_span_id")
	assert.Equal(t, "hello", record["msg"])
	assert.Equal(t, "value", record["key"])
}

func TestNewTraceLoggerNilBaseUsesDefault(t *testing.T) {
	tr := NewTracer("trace-slog-default", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	// Should not panic when base is nil.
	logger := NewTraceLogger(span, nil)
	require.NotNil(t, logger)

	// Logging should work without panic.
	logger.Info("test message")
}

func TestTraceHandlerParentSpanIDOmittedForRoot(t *testing.T) {
	tr := NewTracer("trace-slog-root", nil)
	span, _ := tr.Start(context.Background(), "root-op", SpanKindInternal)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	base := slog.New(handler)

	logger := NewTraceLogger(span, base)
	logger.Info("root log")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))

	// Root span has no parent, so parent_span_id should be absent.
	_, hasParent := record["parent_span_id"]
	assert.False(t, hasParent, "root span should not have parent_span_id in log")
}

func TestTraceHandlerChildSpanIncludesParentSpanID(t *testing.T) {
	tr := NewTracer("trace-slog-child", nil)
	root, ctx := tr.Start(context.Background(), "root", SpanKindInternal)
	child, _ := tr.Start(ctx, "child", SpanKindInternal)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	base := slog.New(handler)

	logger := NewTraceLogger(child, base)
	logger.Info("child log")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))

	assert.Equal(t, root.SpanID(), record["parent_span_id"], "child span must include parent_span_id")
	assert.Equal(t, child.SpanID(), record["span_id"])
}

func TestTraceHandlerEnabledForwards(t *testing.T) {
	tr := NewTracer("trace-slog-enabled", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	base := slog.New(handler)

	logger := NewTraceLogger(span, base)

	// Debug should be filtered out by the Warn-level handler.
	logger.Debug("should be filtered")
	assert.Empty(t, buf.String(), "debug should be filtered by Warn-level handler")

	// Warn should pass through.
	logger.Warn("should pass")
	assert.Contains(t, buf.String(), "should pass")
}

func TestTraceHandlerWithAttrsPreservesTraceFields(t *testing.T) {
	tr := NewTracer("trace-slog-attrs", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	base := slog.New(handler)

	logger := NewTraceLogger(span, base).With("preset", "abc")
	logger.Info("with attrs")

	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))

	assert.Equal(t, "abc", record["preset"])
	assert.Equal(t, "trace-slog-attrs", record["trace_id"], "trace_id must survive WithAttrs")
}

func TestTraceHandlerWithGroupPreservesTraceFields(t *testing.T) {
	tr := NewTracer("trace-slog-group", nil)
	span, _ := tr.Start(context.Background(), "op", SpanKindInternal)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	base := slog.New(handler)

	logger := NewTraceLogger(span, base).WithGroup("g1")
	logger.Info("with group", "k", "v")

	out := buf.String()
	assert.True(t, strings.Contains(out, "trace-slog-group"), "trace_id must survive WithGroup: %s", out)
}
