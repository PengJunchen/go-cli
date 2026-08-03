package verify

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- AssertNoPanic edge cases ---

func TestAssertNoPanic_WithPanic(t *testing.T) {
	ft := &fakeT{}
	AssertNoPanic(ft, func() {
		panic("unexpected panic")
	})
	if !ft.failed {
		t.Error("expected AssertNoPanic to fail when panic occurs")
	}
}

// --- AssertPanic edge cases ---

func TestAssertPanic_NoPanic(t *testing.T) {
	ft := &fakeT{}
	AssertPanic(ft, func() {
		// does nothing - should trigger failure
	})
	if !ft.failed {
		t.Error("expected AssertPanic to fail when no panic occurs")
	}
}

// --- AssertErrorIs edge cases ---

func TestAssertErrorIs_NilError(t *testing.T) {
	ft := &fakeT{}
	AssertErrorIs(ft, nil, context.Canceled)
	if !ft.failed {
		t.Error("expected AssertErrorIs to fail with nil error")
	}
}

func TestAssertErrorIs_NonMatchingError(t *testing.T) {
	ft := &fakeT{}
	AssertErrorIs(ft, errors.New("some error"), context.Canceled)
	if !ft.failed {
		t.Error("expected AssertErrorIs to fail with non-matching error")
	}
}

// --- AssertContextCanceled edge cases ---

func TestAssertContextCanceled_NilError(t *testing.T) {
	ft := &fakeT{}
	AssertContextCanceled(ft, func(ctx context.Context) error {
		return nil
	})
	if !ft.failed {
		t.Error("expected AssertContextCanceled to fail with nil error")
	}
}

func TestAssertContextCanceled_NonCanceledError(t *testing.T) {
	ft := &fakeT{}
	AssertContextCanceled(ft, func(ctx context.Context) error {
		return errors.New("not canceled")
	})
	if !ft.failed {
		t.Error("expected AssertContextCanceled to fail with non-Canceled error")
	}
}

// --- AssertContextTimeout edge cases ---

func TestAssertContextTimeout_NilError(t *testing.T) {
	ft := &fakeT{}
	AssertContextTimeout(ft, func(ctx context.Context) error {
		return nil
	})
	if !ft.failed {
		t.Error("expected AssertContextTimeout to fail with nil error")
	}
}

func TestAssertContextTimeout_NonDeadlineError(t *testing.T) {
	ft := &fakeT{}
	AssertContextTimeout(ft, func(ctx context.Context) error {
		return errors.New("not deadline")
	})
	if !ft.failed {
		t.Error("expected AssertContextTimeout to fail with non-DeadlineExceeded error")
	}
}

// --- matchLogEntry edge cases ---

func TestMatchLogEntry_HasLevelMismatch(t *testing.T) {
	entry := LogEntry{
		Level:   slog.LevelDebug,
		Message: "test",
		Fields:  map[string]any{"op": "test"},
	}
	match := LogMatch{
		Op:       "test",
		HasLevel: true,
		Level:    slog.LevelInfo,
	}
	if matchLogEntry(entry, match) {
		t.Error("expected matchLogEntry to return false for level mismatch")
	}
}

func TestMatchLogEntry_FieldMismatch(t *testing.T) {
	entry := LogEntry{
		Level:   slog.LevelInfo,
		Message: "test",
		Fields:  map[string]any{"op": "test", "count": 5},
	}
	match := LogMatch{
		Fields: map[string]any{"op": "test", "count": 10},
	}
	if matchLogEntry(entry, match) {
		t.Error("expected matchLogEntry to return false for field value mismatch")
	}
}

func TestMatchLogEntry_FieldMissing(t *testing.T) {
	entry := LogEntry{
		Level:   slog.LevelInfo,
		Message: "test",
		Fields:  map[string]any{"op": "test"},
	}
	match := LogMatch{
		Fields: map[string]any{"op": "test", "missing": "value"},
	}
	if matchLogEntry(entry, match) {
		t.Error("expected matchLogEntry to return false for missing field")
	}
}

func TestMatchLogEntry_OpNotString(t *testing.T) {
	entry := LogEntry{
		Level:   slog.LevelInfo,
		Message: "test",
		Fields:  map[string]any{"op": 123},
	}
	match := LogMatch{
		Op: "test",
	}
	if matchLogEntry(entry, match) {
		t.Error("expected matchLogEntry to return false when op field is not string")
	}
}

func TestMatchLogEntry_EmptyMatch(t *testing.T) {
	entry := LogEntry{
		Level:   slog.LevelInfo,
		Message: "test",
		Fields:  map[string]any{"op": "test"},
	}
	if !matchLogEntry(entry, LogMatch{}) {
		t.Error("expected matchLogEntry to return true for empty match")
	}
}

// --- AssertNoLogEntry edge cases ---

func TestAssertNoLogEntry_MatchingEntry(t *testing.T) {
	entries := []LogEntry{
		{Level: slog.LevelInfo, Message: "test", Fields: map[string]any{"op": "test"}},
	}
	ft := &fakeT{}
	AssertNoLogEntry(ft, entries, LogMatch{Op: "test"})
	if !ft.failed {
		t.Error("expected AssertNoLogEntry to fail when matching entry exists")
	}
}

func TestAssertNoLogEntry_NoMatch(t *testing.T) {
	entries := []LogEntry{
		{Level: slog.LevelInfo, Message: "test", Fields: map[string]any{"op": "other"}},
	}
	ft := &fakeT{}
	AssertNoLogEntry(ft, entries, LogMatch{Op: "test"})
	if ft.failed {
		t.Error("expected AssertNoLogEntry to pass when no matching entry")
	}
}

// --- SaveReport edge cases ---

func TestSaveReport_WriteFailure(t *testing.T) {
	report := &VerifyReport{
		Timestamp: "2026-01-01",
		Dir:       ".",
		Summary: VerifySummary{
			Total:   1,
			Passed:  1,
			Failed:  0,
			Skipped: 0,
		},
	}
	// Write to a path inside a non-existent directory.
	err := SaveReport(report, filepath.Join(os.TempDir(), "nonexistent_dir_12345", "report.json"))
	if err == nil {
		t.Error("expected SaveReport to fail when directory does not exist")
	}
}

func TestSaveReport_Success(t *testing.T) {
	report := &VerifyReport{
		Timestamp: "2026-01-01",
		Dir:       ".",
		Summary: VerifySummary{
			Total:   1,
			Passed:  1,
			Failed:  0,
			Skipped: 0,
		},
		Results: []VerifyResult{
			{ID: "TEST-001", Name: "test rule", Status: "PASS", Message: "ok"},
		},
	}
	path := filepath.Join(t.TempDir(), "report.json")
	err := SaveReport(report, path)
	if err != nil {
		t.Fatalf("SaveReport failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected report file to exist: %v", err)
	}
}

// --- GoLeakChecker Assert with leak ---

func TestGoLeakChecker_ActualLeak(t *testing.T) {
	// This test intentionally leaks a goroutine to verify the Assert path
	// that reports a leak. We use a fakeT so the test doesn't fail.
	checker := NewGoLeakChecker()
	checker.Checkpoint("start")

	// Start a goroutine that blocks forever.
	leakCtx, cancel := context.WithCancel(context.Background())
	go func() {
		<-leakCtx.Done()
	}()

	// Give the goroutine time to start.
	time.Sleep(50 * time.Millisecond)

	ft := &fakeT{}
	checker.Assert(ft)
	// Cancel to clean up.
	cancel()

	// The fakeT may or may not have failed depending on timing,
	// but either way the test itself should not hang.
}

// --- formatCheckpoints covers via Assert ---

func TestGoLeakChecker_FormatCheckpoints(t *testing.T) {
	checker := NewGoLeakChecker()
	checker.Checkpoint("a")
	checker.Checkpoint("b")
	checker.Checkpoint("c")

	s := checker.formatCheckpoints()
	if s == "" {
		t.Error("expected non-empty checkpoints string")
	}
}

// --- countFilteredGoroutines edge ---

func TestCountFilteredGoroutines_ReturnsPositive(t *testing.T) {
	count := countFilteredGoroutines()
	if count <= 0 {
		t.Error("expected at least 1 goroutine (current test goroutine)")
	}
}

// --- AssertLogEntry with empty entries ---

func TestAssertLogEntry_EmptyEntries(t *testing.T) {
	ft := &fakeT{}
	AssertLogEntry(ft, nil, LogMatch{Op: "test"})
	if !ft.failed {
		t.Error("expected AssertLogEntry to fail with empty entries")
	}
}

// --- AssertLogSequence edge ---

func TestAssertLogSequence_EmptyMatches(t *testing.T) {
	entries := []LogEntry{
		{Level: slog.LevelInfo, Message: "test", Fields: map[string]any{"op": "test"}},
	}
	ft := &fakeT{}
	AssertLogSequence(ft, entries, nil)
	// Empty matches should not fail.
	if ft.failed {
		t.Error("expected AssertLogSequence to pass with empty matches")
	}
}

func TestAssertLogSequence_MatchNotFound(t *testing.T) {
	entries := []LogEntry{
		{Level: slog.LevelInfo, Message: "test", Fields: map[string]any{"op": "first"}},
	}
	ft := &fakeT{}
	AssertLogSequence(ft, entries, []LogMatch{{Op: "nonexistent"}})
	if !ft.failed {
		t.Error("expected AssertLogSequence to fail when match not found")
	}
}

func TestAssertLogSequence_OutOfOrder(t *testing.T) {
	entries := []LogEntry{
		{Level: slog.LevelInfo, Message: "a", Fields: map[string]any{"op": "a"}},
		{Level: slog.LevelInfo, Message: "b", Fields: map[string]any{"op": "b"}},
	}
	ft := &fakeT{}
	// Search for "b" first, then "a" - should fail since "a" is before "b".
	AssertLogSequence(ft, entries, []LogMatch{
		{Op: "b"},
		{Op: "a"},
	})
	if !ft.failed {
		t.Error("expected AssertLogSequence to fail for out-of-order matches")
	}
}

// --- Attach / Detach edge ---

func TestLogCapturer_AttachMultiple(t *testing.T) {
	capturer := NewLogCapturer()
	capturer.Attach(t.Context())
	defer capturer.Detach()

	// Attach again should not panic.
	capturer.Attach(t.Context())

	slog.Info("test", "op", "multi_attach")
	entries := capturer.Entries()
	if len(entries) == 0 {
		t.Error("expected at least one entry after double attach")
	}
}

// --- FormatReportText ---

func TestFormatReportText_WithFailures(t *testing.T) {
	report := &VerifyReport{
		Timestamp: "2026-01-01",
		Dir:       ".",
		Summary: VerifySummary{
			Total:   3,
			Passed:  1,
			Failed:  1,
			Skipped: 1,
		},
		Results: []VerifyResult{
			{ID: "PASS-001", Name: "pass rule", Status: "PASS", Message: "ok"},
			{ID: "FAIL-001", Name: "fail rule", Status: "FAIL", Message: "bad"},
			{ID: "SKIP-001", Name: "skip rule", Status: "SKIP", Message: "skipped"},
		},
	}
	text := FormatReportText(report)
	if text == "" {
		t.Error("expected non-empty report text")
	}
	if !strings.Contains(text, "FAIL") {
		t.Error("expected report text to contain FAIL")
	}
	if !strings.Contains(text, "SKIP") {
		t.Error("expected report text to contain SKIP")
	}
}

// --- RunVerify coverage ---

func TestRunVerify_WithResults(t *testing.T) {
	// RunVerify on a temp directory with a simple Go file.
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "test.go")
	err := os.WriteFile(goFile, []byte("package test\n\n// TestFunc does something.\nfunc TestFunc() {}\n"), 0o644)
	if err != nil {
		t.Fatalf("write file: %v", err)
	}

	report := RunVerify(tmpDir)
	if report == nil {
		t.Fatal("expected non-nil report")
	}
	if report.Summary.Total == 0 {
		t.Error("expected at least some rules in report")
	}
}

// --- checkGoFiles edge ---

func TestCheckGoFiles_NoGoFiles(t *testing.T) {
	tmpDir := t.TempDir()
	// No .go files in directory.
	results := checkGoFiles(tmpDir)
	// Should return results (even if empty or with a SKIP).
	_ = results // just verify no panic
}

// --- scanNolint edge ---

func TestScanNolint_NoNolint(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "test.go")
	_ = os.WriteFile(goFile, []byte("package main\n\nfunc main() {}\n"), 0o644)

	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, goFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse file: %v", err)
	}

	findings := scanNolint(goFile, astFile, fset, 3)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestScanNolint_WithNolint(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "test.go")
	_ = os.WriteFile(goFile, []byte("package main //nolint:errcheck\n\nfunc main() {} //nolint\n"), 0o644)

	fset := token.NewFileSet()
	astFile, err := parser.ParseFile(fset, goFile, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse file: %v", err)
	}

	findings := scanNolint(goFile, astFile, fset, 1)
	// Should detect nolint count exceeding threshold.
	if len(findings) == 0 {
		t.Error("expected at least 1 finding for nolint exceeding threshold")
	}
}

// --- isExcludedDir edge ---

func TestIsExcludedDir_HiddenDir(t *testing.T) {
	excludeDirs := []string{".git", ".github", "vendor", "node_modules"}
	if !isExcludedDir(".git", excludeDirs) {
		t.Error("expected .git to be excluded")
	}
	if !isExcludedDir(".github", excludeDirs) {
		t.Error("expected .github to be excluded")
	}
}

func TestIsExcludedDir_NormalDir(t *testing.T) {
	excludeDirs := []string{".git", ".github", "vendor", "node_modules"}
	if isExcludedDir("src", excludeDirs) {
		t.Error("expected src to not be excluded")
	}
	if isExcludedDir("internal", excludeDirs) {
		t.Error("expected internal to not be excluded")
	}
}

// --- scanHardcodedSecrets edge ---

func TestScanHardcodedSecrets_NoSecrets(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "test.go")
	_ = os.WriteFile(goFile, []byte("package main\n\nfunc main() {}\n"), 0o644)

	findings := scanHardcodedSecrets(goFile)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(findings))
	}
}

func TestScanHardcodedSecrets_WithSecret(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "test.go")
	_ = os.WriteFile(goFile, []byte(`package main

var apiKey = "sk-proj-1234567890abcdefghijklmnop"

func main() {}
`), 0o644)

	findings := scanHardcodedSecrets(goFile)
	if len(findings) == 0 {
		t.Error("expected at least 1 finding for hardcoded API key")
	}
}

// --- VerifyResult formatting ---

func TestVerifyResult_String(t *testing.T) {
	r := VerifyResult{ID: "TEST", Name: "test", Status: "PASS", Message: "ok"}
	s := fmt.Sprintf("%+v", r)
	if s == "" {
		t.Error("expected non-empty string")
	}
}
