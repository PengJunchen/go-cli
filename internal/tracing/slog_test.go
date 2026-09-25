package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestRedactingSlogHandlerMasksPromptAndToolArgs(t *testing.T) {
	const (
		promptCanary  = "PROMPT-LOG-CANARY"
		toolCanary    = "TOOL-ARGS-LOG-CANARY"
		secretCanary  = "API-KEY-LOG-CANARY"
		reasonCanary  = "PLAN-REASON-LOG-CANARY"
		hookCanary    = "HOOK-OUTPUT-LOG-CANARY"
		commandCanary = "HOOK-COMMAND-LOG-CANARY"
		titleCanary   = "PR-TITLE-LOG-CANARY"
	)
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewRedactingSlogHandler(base))
	logger.Info("tool.completed",
		"trace_id", "trace-123",
		"tool_name", "todo_write",
		"status", "ok",
		"duration_ms", 17,
		"prompt_length", len(promptCanary),
		"query", promptCanary,
		"cache_key", "todo_write:{\"content\":\""+toolCanary+"\"}",
		"args", map[string]any{"content": toolCanary},
		"api_key", secretCanary,
		"reason", reasonCanary,
		"output", hookCanary,
		"command", commandCanary,
		"title", titleCanary,
	)

	line := buf.String()
	assert.False(t, strings.Contains(line, promptCanary), "slog leaked prompt content")
	assert.False(t, strings.Contains(line, toolCanary), "slog leaked tool arguments")
	assert.False(t, strings.Contains(line, secretCanary), "slog leaked API key")
	assert.False(t, strings.Contains(line, reasonCanary), "slog leaked plan-mode reason")
	assert.False(t, strings.Contains(line, hookCanary), "slog leaked hook output")
	assert.False(t, strings.Contains(line, commandCanary), "slog leaked hook command")
	assert.False(t, strings.Contains(line, titleCanary), "slog leaked PR title")
	assert.True(t, strings.Contains(line, "***REDACTED***"), "slog must retain redaction markers")
	assert.True(t, strings.Contains(line, "bytes="), "slog must retain redacted value lengths")
	assert.True(t, strings.Contains(line, "tool.completed"), "slog must retain operation identity")
	assert.True(t, strings.Contains(line, "trace-123"), "slog must retain trace identity")
	assert.True(t, strings.Contains(line, "todo_write"), "slog must retain tool identity")
	assert.True(t, strings.Contains(line, "duration_ms"), "slog must retain timing metadata")
	assert.True(t, strings.Contains(line, "prompt_length"), "slog must retain prompt length metadata")
}

func TestRedactingSlogHandlerMasksSensitiveGroups(t *testing.T) {
	const canary = "GROUPED-TOOL-ARGS-CANARY"
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewRedactingSlogHandler(base))

	logger.Info("nested.args", slog.Group("args", slog.String("arbitrary", canary)))
	logger.WithGroup("args").Info("scoped.args", "arbitrary_field", canary)

	line := buf.String()
	assert.False(t, strings.Contains(line, canary), "slog leaked nested or scoped group content")
	assert.True(t, strings.Contains(line, "nested.args"), "slog must retain nested operation identity")
	assert.True(t, strings.Contains(line, "scoped.args"), "slog must retain scoped operation identity")
	assert.True(t, strings.Contains(line, "bytes="), "slog must retain redacted value lengths")
}

func TestRedactingSlogHandlerMasksErrorFieldsAndKeepsDiagnostics(t *testing.T) {
	const (
		errCanary      = "ERR-FIELD-RAW-CANARY"
		errorCanary    = "ERROR-FIELD-RAW-CANARY"
		causeCanary    = "CAUSE-FIELD-RAW-CANARY"
		originalCanary = "ORIGINAL-ERROR-RAW-CANARY"
	)
	var buf bytes.Buffer
	base := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(NewRedactingSlogHandler(base))
	logger.Error("provider.request.failed",
		"err", errors.New(errCanary),
		"error", errorCanary,
		"cause", causeCanary,
		"original_error", originalCanary,
		"error_type", "server",
		"http_status_code", 400,
	)

	line := buf.String()
	for _, canary := range []string{errCanary, errorCanary, causeCanary, originalCanary} {
		if strings.Contains(line, canary) {
			t.Fatal("slog redaction leaked an error field")
		}
	}
	assert.Contains(t, line, "provider.request.failed")
	assert.Contains(t, line, "error_type")
	assert.Contains(t, line, "server")
	assert.Contains(t, line, "http_status_code")
	assert.Contains(t, line, "400")
	assert.Contains(t, line, "bytes=")
}

func TestRedactingSlogHandlerDoesNotMutateOriginalRecord(t *testing.T) {
	const canary = "ORIGINAL-GROUP-CANARY"
	children := []slog.Attr{slog.String("query", canary)}
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "nested", 0)
	record.AddAttrs(slog.Attr{Key: "request", Value: slog.GroupValue(children...)})

	var buf bytes.Buffer
	handler := NewRedactingSlogHandler(slog.NewJSONHandler(&buf, nil))
	require.NoError(t, handler.Handle(context.Background(), record))
	assert.False(t, strings.Contains(buf.String(), canary), "slog leaked group content")
	assert.True(t, children[0].Value.String() == canary, "redaction must not mutate the caller's group slice")

	var retained string
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "request" {
			retained = attr.Value.Group()[0].Value.String()
		}
		return true
	})
	assert.True(t, retained == canary, "redaction must not mutate the original slog.Record")
}

func TestRedactingSlogHandlerConcurrentGroupDoesNotMutateRecord(t *testing.T) {
	const canary = "CONCURRENT-ORIGINAL-GROUP-CANARY"
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "nested", 0)
	record.AddAttrs(slog.Attr{Key: "request", Value: slog.GroupValue(slog.String("query", canary))})
	handler := NewRedactingSlogHandler(slog.NewJSONHandler(io.Discard, nil))

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				if err := handler.Handle(context.Background(), record); err != nil {
					t.Errorf("Handle: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == "request" {
			assert.True(t, attr.Value.Group()[0].Value.String() == canary, "concurrent redaction must not mutate the original slog.Record")
		}
		return true
	})
}
