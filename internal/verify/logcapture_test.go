package verify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogCapturer_CapturesEntries(t *testing.T) {
	defer AssertNoGoroutineLeak(t)()

	capturer := NewLogCapturer()
	capturer.Attach(t.Context())
	defer capturer.Detach()

	slog.Info("task submitted", "op", "submit", "task_id", "t1")
	slog.Debug("debug msg", "op", "debug_op")
	slog.Error("error msg", "op", "error_op", "error_type", "timeout")

	entries := capturer.Entries()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	if entries[0].Message != "task submitted" {
		t.Errorf("entry[0] message = %q, want %q", entries[0].Message, "task submitted")
	}
	if entries[0].Fields["op"] != "submit" {
		t.Errorf("entry[0] op = %v, want %q", entries[0].Fields["op"], "submit")
	}
	if entries[0].Level != slog.LevelInfo {
		t.Errorf("entry[0] level = %v, want %v", entries[0].Level, slog.LevelInfo)
	}
}

func TestLogCapturer_AssertLogEntry_NoMatch(t *testing.T) {
	// AssertLogEntry calls Fatalf when no match found.
	// We use a fake TestingT to verify the failure message.
	capturer := NewLogCapturer()

	ft := &fakeT{}
	AssertLogEntry(ft, capturer.Entries(), LogMatch{
		Op:       "submit",
		HasLevel: true,
		Level:    slog.LevelInfo,
	})

	if !ft.failed {
		t.Error("expected AssertLogEntry to fail when no entries match")
	}
}

// fakeT implements TestingT for testing assertion failures.
type fakeT struct {
	failed bool
	msg    string
}

func (f *fakeT) Helper() {}
func (f *fakeT) Fatal(args ...any) {
	f.failed = true
	f.msg = fmt.Sprint(args...)
}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.failed = true
	f.msg = fmt.Sprintf(format, args...)
}
func (f *fakeT) Logf(format string, args ...any) {}

func TestLogCapturer_AssertLogEntry_Pass(t *testing.T) {
	capturer := NewLogCapturer()
	capturer.Attach(t.Context())
	defer capturer.Detach()

	slog.Info("task submitted", "op", "submit", "task_id", "t1")

	entry := AssertLogEntry(t, capturer.Entries(), LogMatch{
		Op:       "submit",
		HasLevel: true,
		Level:    slog.LevelInfo,
	})

	if entry.Message != "task submitted" {
		t.Errorf("message = %q, want %q", entry.Message, "task submitted")
	}
	if entry.Fields["task_id"] != "t1" {
		t.Errorf("task_id = %v, want %q", entry.Fields["task_id"], "t1")
	}
}

func TestLogCapturer_AssertLogSequence(t *testing.T) {
	capturer := NewLogCapturer()
	capturer.Attach(t.Context())
	defer capturer.Detach()

	slog.Info("start", "op", "submit")
	slog.Info("running", "op", "execute")
	slog.Info("done", "op", "complete")

	AssertLogSequence(t, capturer.Entries(), []LogMatch{
		{Op: "submit"},
		{Op: "execute"},
		{Op: "complete"},
	})
}

func TestLogCapturer_AssertNoLogEntry(t *testing.T) {
	capturer := NewLogCapturer()
	capturer.Attach(t.Context())
	defer capturer.Detach()

	slog.Info("normal op", "op", "submit")

	// Should not find any retry entry.
	AssertNoLogEntry(t, capturer.Entries(), LogMatch{Op: "retry"})
}

func TestLogCapturer_ConcurrentSafety(t *testing.T) {
	defer AssertNoGoroutineLeak(t)()

	capturer := NewLogCapturer()
	capturer.Attach(t.Context())
	defer capturer.Detach()

	var wg sync.WaitGroup
	n := 100
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(id int) {
			defer wg.Done()
			slog.Info("concurrent log", "op", "concurrent", "id", id)
		}(i)
	}
	wg.Wait()

	if capturer.Count() != n {
		t.Errorf("expected %d entries, got %d", n, capturer.Count())
	}
}

func TestLogCapturer_Reset(t *testing.T) {
	capturer := NewLogCapturer()
	capturer.Attach(t.Context())
	defer capturer.Detach()

	slog.Info("msg1", "op", "op1")
	slog.Info("msg2", "op", "op2")

	if capturer.Count() != 2 {
		t.Fatalf("expected 2 entries, got %d", capturer.Count())
	}

	capturer.Reset()

	if capturer.Count() != 0 {
		t.Errorf("after reset, expected 0 entries, got %d", capturer.Count())
	}
}

func TestLogCapturer_DetachRestores(t *testing.T) {
	original := slog.Default()

	capturer := NewLogCapturer()
	capturer.Attach(t.Context())

	slog.Info("captured", "op", "test")
	capturer.Detach()

	// After detach, default logger should be the original.
	if slog.Default().Handler() != original.Handler() {
		t.Error("Detach did not restore original logger")
	}
}

func TestLogCapturer_TimeField(t *testing.T) {
	capturer := NewLogCapturer()
	capturer.Attach(t.Context())
	defer capturer.Detach()

	before := time.Now()
	slog.Info("timed", "op", "timed_op")
	after := time.Now()

	entries := capturer.Entries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Time.Before(before) || entries[0].Time.After(after.Add(time.Millisecond)) {
		t.Errorf("entry time %v not in expected range [%v, %v]", entries[0].Time, before, after)
	}
}

func TestCaptureHandler_WithAttrs(t *testing.T) {
	capturer := NewLogCapturer()
	ch := &captureHandler{
		capturer: capturer,
		level:    slog.LevelDebug,
	}

	h2 := ch.WithAttrs([]slog.Attr{slog.String("preset_key", "preset_val")})
	require.NotNil(t, h2)

	ch2, ok := h2.(*captureHandler)
	require.True(t, ok, "returned handler should be *captureHandler")
	assert.Len(t, ch2.attrs, 1)
	assert.Equal(t, "preset_key", ch2.attrs[0].Key)

	// Log through the new handler and verify both preset and record attrs appear.
	logger := slog.New(h2)
	logger.Info("test message", "op", "with_attrs_test")

	entries := capturer.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, "test message", entries[0].Message)
	assert.Equal(t, "preset_val", entries[0].Fields["preset_key"], "pre-set attr should appear in captured entry")
	assert.Equal(t, "with_attrs_test", entries[0].Fields["op"], "record attr should appear in captured entry")
}

func TestCaptureHandler_WithAttrs_Chained(t *testing.T) {
	capturer := NewLogCapturer()
	ch := &captureHandler{
		capturer: capturer,
		level:    slog.LevelDebug,
	}

	h2 := ch.WithAttrs([]slog.Attr{slog.String("k1", "v1")})
	h3 := h2.WithAttrs([]slog.Attr{slog.Int("k2", 42)})

	ch3, ok := h3.(*captureHandler)
	require.True(t, ok)
	assert.Len(t, ch3.attrs, 2, "chained WithAttrs should accumulate attrs")
	assert.Equal(t, "k1", ch3.attrs[0].Key)
	assert.Equal(t, "k2", ch3.attrs[1].Key)
}

func TestCaptureHandler_WithGroup(t *testing.T) {
	capturer := NewLogCapturer()
	ch := &captureHandler{
		capturer: capturer,
		level:    slog.LevelDebug,
	}

	h2 := ch.WithGroup("testgroup")
	require.NotNil(t, h2)

	logger := slog.New(h2)
	logger.Info("grouped message", "op", "with_group_test")

	entries := capturer.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, "grouped message", entries[0].Message)
	assert.Equal(t, "with_group_test", entries[0].Fields["testgroup.op"], "attr key should be prefixed with group")
}

func TestCaptureHandler_WithGroup_EmptyName(t *testing.T) {
	capturer := NewLogCapturer()
	ch := &captureHandler{
		capturer: capturer,
		level:    slog.LevelDebug,
	}

	h2 := ch.WithGroup("")
	assert.Same(t, ch, h2, "WithGroup with empty name should return the same handler")
}

func TestCaptureHandler_WithGroup_Nested(t *testing.T) {
	capturer := NewLogCapturer()
	ch := &captureHandler{
		capturer: capturer,
		level:    slog.LevelDebug,
		group:    "outer",
	}

	h2 := ch.WithGroup("inner")
	ch2, ok := h2.(*captureHandler)
	require.True(t, ok)
	assert.Equal(t, "outer.inner", ch2.group, "nested groups should be dot-separated")
}

func TestCaptureHandler_WithGroup_WithAttrs(t *testing.T) {
	capturer := NewLogCapturer()
	ch := &captureHandler{
		capturer: capturer,
		level:    slog.LevelDebug,
	}

	h2 := ch.WithGroup("mygroup").WithAttrs([]slog.Attr{slog.String("preset", "val")})
	logger := slog.New(h2)
	logger.Info("combined message", "op", "combo")

	entries := capturer.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, "val", entries[0].Fields["mygroup.preset"], "preset attr should be prefixed with group")
	assert.Equal(t, "combo", entries[0].Fields["mygroup.op"], "record attr should be prefixed with group")
}

func TestCaptureHandler_Enabled(t *testing.T) {
	capturer := NewLogCapturer()
	ch := &captureHandler{
		capturer: capturer,
		level:    slog.LevelWarn,
	}

	assert.True(t, ch.Enabled(context.Background(), slog.LevelError), "Error >= Warn should be enabled")
	assert.True(t, ch.Enabled(context.Background(), slog.LevelWarn), "Warn >= Warn should be enabled")
	assert.False(t, ch.Enabled(context.Background(), slog.LevelInfo), "Info < Warn should not be enabled")
	assert.False(t, ch.Enabled(context.Background(), slog.LevelDebug), "Debug < Warn should not be enabled")
}

func TestFormatEntry(t *testing.T) {
	entry := LogEntry{
		Level:   slog.LevelInfo,
		Message: "task completed",
		Fields: map[string]any{
			"op":      "complete",
			"task_id": "t1",
		},
		Time: time.Now(),
	}

	text := formatEntry(entry)
	assert.Contains(t, text, "INFO")
	assert.Contains(t, text, "task completed")
	assert.Contains(t, text, "op=complete")
	assert.Contains(t, text, "task_id=t1")
}

func TestFormatEntry_NoFields(t *testing.T) {
	entry := LogEntry{
		Level:   slog.LevelError,
		Message: "critical failure",
		Fields:  map[string]any{},
		Time:    time.Now(),
	}

	text := formatEntry(entry)
	assert.Contains(t, text, "ERROR")
	assert.Contains(t, text, "critical failure")
	// Should not panic or produce unexpected output with empty fields.
	assert.NotContains(t, text, "=")
}

func TestFormatEntries_Empty(t *testing.T) {
	text := formatEntries(nil)
	assert.Equal(t, "", text, "empty entries should produce empty string")
}

func TestFormatEntries_Single(t *testing.T) {
	entries := []LogEntry{
		{Level: slog.LevelInfo, Message: "single", Fields: map[string]any{}, Time: time.Now()},
	}
	text := formatEntries(entries)
	assert.Contains(t, text, "INFO")
	assert.Contains(t, text, "single")
}

func TestFormatEntries_Multiple(t *testing.T) {
	entries := []LogEntry{
		{Level: slog.LevelInfo, Message: "first", Fields: map[string]any{"k": "v"}, Time: time.Now()},
		{Level: slog.LevelError, Message: "second", Fields: map[string]any{}, Time: time.Now()},
	}
	text := formatEntries(entries)
	assert.Contains(t, text, "first")
	assert.Contains(t, text, "second")
	assert.Contains(t, text, "\n", "multiple entries should be newline-separated")
}
