package verify

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"
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
