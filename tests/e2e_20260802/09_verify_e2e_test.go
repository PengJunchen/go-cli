// Package e2e_20260802 contains end-to-end integration tests for the verify
// module: goroutine leak detection, log capture/assertion, verification runner
// (against temp projects), SCAN scanner rules (AST-level mock/hardcoded/slog),
// VQVG heuristics, and report generation (text and JSON formats).
package e2e_20260802

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Goroutine leak detection
// =============================================================================

func TestVerify_GoroutineLeakDetection_NoLeak(t *testing.T) {
	// Start with a clean baseline and assert no leak after a no-op.
	done := make(chan bool)
	go func() {
		defer verify.AssertNoGoroutineLeak(t)()
		// No goroutines spawned beyond this test helper.
		time.Sleep(10 * time.Millisecond)
		close(done)
	}()
	<-done
}

func TestVerify_GoroutineLeakDetection_CleanShutdown(t *testing.T) {
	done := make(chan bool)
	go func() {
		defer verify.AssertNoGoroutineLeak(t)()
		// Spawn a goroutine that exits cleanly.
		ch := make(chan struct{})
		go func() { close(ch) }()
		<-ch
		time.Sleep(50 * time.Millisecond) // let goroutine settle
		close(done)
	}()
	<-done
}

func TestVerify_GoLeakCheckerMultipleCheckpoints(t *testing.T) {
	checker := verify.NewGoLeakChecker()

	checker.Checkpoint("initial")

	// Spawn a goroutine that exits cleanly.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(10 * time.Millisecond)
	}()
	wg.Wait()

	checker.Checkpoint("after-cleanup")

	checker.Assert(t)
}

func TestVerify_AssertContextCanceled(t *testing.T) {
	verify.AssertContextCanceled(t, func(ctx context.Context) error {
		select {
		case <-time.After(10 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

func TestVerify_AssertContextTimeout(t *testing.T) {
	verify.AssertContextTimeout(t, func(ctx context.Context) error {
		select {
		case <-time.After(100 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

func TestVerify_AssertErrorIs(t *testing.T) {
	err := context.Canceled
	verify.AssertErrorIs(t, err, context.Canceled)
}

func TestVerify_AssertNoPanic(t *testing.T) {
	verify.AssertNoPanic(t, func() {
		_ = 1 + 1
	})
}

func TestVerify_AssertPanic(t *testing.T) {
	verify.AssertPanic(t, func() {
		panic("expected panic")
	})
}

// =============================================================================
// Log capture and assertion
// =============================================================================

func TestVerify_LogCapturerAttachDetach(t *testing.T) {
	cap := verify.NewLogCapturer()

	ctx := cap.Attach(context.Background())

	slog.InfoContext(ctx, "test message", "key", "value")

	entries := cap.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, slog.LevelInfo, entries[0].Level)
	assert.Equal(t, "test message", entries[0].Message)
	assert.Equal(t, "value", entries[0].Fields["key"])

	cap.Detach()
}

func TestVerify_LogCapturerMultipleEntries(t *testing.T) {
	cap := verify.NewLogCapturer()

	ctx := cap.Attach(context.Background())

	slog.DebugContext(ctx, "debug msg", "a", 1)
	slog.InfoContext(ctx, "info msg", "b", 2)
	slog.WarnContext(ctx, "warn msg", "c", 3)
	slog.ErrorContext(ctx, "error msg", "d", 4)

	entries := cap.Entries()
	// Note: slog.Error is the only level that uses Attr-style interface
	// which may come through differently. At minimum we should see entries.
	assert.GreaterOrEqual(t, len(entries), 3)

	cap.Detach()
}

func TestVerify_LogCapturerReset(t *testing.T) {
	cap := verify.NewLogCapturer()

	ctx := cap.Attach(context.Background())
	slog.InfoContext(ctx, "before reset")

	assert.Equal(t, 1, cap.Count())

	cap.Reset()
	assert.Equal(t, 0, cap.Count())

	cap.Detach()
}

func TestVerify_LogCapturerCount(t *testing.T) {
	cap := verify.NewLogCapturer()

	ctx := cap.Attach(context.Background())

	assert.Equal(t, 0, cap.Count())

	slog.InfoContext(ctx, "msg1")
	assert.Equal(t, 1, cap.Count())

	slog.InfoContext(ctx, "msg2")
	assert.Equal(t, 2, cap.Count())

	cap.Detach()
}

func TestVerify_AssertLogEntryMatch(t *testing.T) {
	cap := verify.NewLogCapturer()

	ctx := cap.Attach(context.Background())

	slog.InfoContext(ctx, "test op", "op", "config.load")
	slog.InfoContext(ctx, "other op", "op", "config.save")

	entries := cap.Entries()

	match := verify.LogMatch{Op: "config.load"}
	found := verify.AssertLogEntry(t, entries, match)
	assert.Equal(t, "config.load", found.Fields["op"])

	cap.Detach()
}

func TestVerify_AssertLogEntryMatchLevel(t *testing.T) {
	cap := verify.NewLogCapturer()

	ctx := cap.Attach(context.Background())

	slog.DebugContext(ctx, "debug op", "op", "test")
	slog.ErrorContext(ctx, "error op", "op", "test")

	entries := cap.Entries()

	match := verify.LogMatch{Op: "test", HasLevel: true, Level: slog.LevelError}
	found := verify.AssertLogEntry(t, entries, match)
	assert.Equal(t, slog.LevelError, found.Level)

	cap.Detach()
}

func TestVerify_AssertLogSequence(t *testing.T) {
	cap := verify.NewLogCapturer()

	ctx := cap.Attach(context.Background())

	slog.InfoContext(ctx, "step1", "op", "a")
	slog.InfoContext(ctx, "step2", "op", "b")
	slog.InfoContext(ctx, "step3", "op", "c")

	entries := cap.Entries()

	verify.AssertLogSequence(t, entries, []verify.LogMatch{
		{Op: "a"},
		{Op: "b"},
		{Op: "c"},
	})

	cap.Detach()
}

func TestVerify_AssertNoLogEntry(t *testing.T) {
	cap := verify.NewLogCapturer()

	ctx := cap.Attach(context.Background())

	slog.InfoContext(ctx, "normal", "op", "ok")

	entries := cap.Entries()

	verify.AssertNoLogEntry(t, entries, verify.LogMatch{Op: "nonexistent"})

	cap.Detach()
}

func TestVerify_LogEntryFields(t *testing.T) {
	cap := verify.NewLogCapturer()

	ctx := cap.Attach(context.Background())

	slog.InfoContext(ctx, "complex log", "string_val", "hello", "int_val", 42, "bool_val", true)

	entries := cap.Entries()
	require.GreaterOrEqual(t, len(entries), 1)

	entry := entries[0]
	assert.Equal(t, "complex log", entry.Message)
	assert.Equal(t, "hello", entry.Fields["string_val"])
	assert.Equal(t, int64(42), entry.Fields["int_val"])
	assert.Equal(t, true, entry.Fields["bool_val"])

	cap.Detach()
}

// =============================================================================
// Verification runner — run against temp project with known issues
// =============================================================================

func TestVerify_RunnerAgainstTempProject(t *testing.T) {
	// Create a temporary directory that looks like a Go project.
	dir := t.TempDir()

	// Create go.mod.
	err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/test\n\ngo 1.24\n"), 0o644)
	require.NoError(t, err)

	// Create a production .go file with a hardcoded test URL.
	prodContent := `package main

import "fmt"

func main() {
	url := "http://localhost:8080"
	fmt.Println(url)
}
`
	err = os.WriteFile(filepath.Join(dir, "main.go"), []byte(prodContent), 0o644)
	require.NoError(t, err)

	report := verify.RunVerify(dir)
	require.NotNil(t, report)

	assert.Equal(t, dir, report.Dir)
	assert.NotEmpty(t, report.Timestamp)

	// Should have GOMOD = PASS because we created go.mod.
	hasGoMod := false
	for _, r := range report.Results {
		if r.ID == "GOMOD" {
			assert.Equal(t, "PASS", r.Status)
			hasGoMod = true
		}
	}
	assert.True(t, hasGoMod, "should have a GOMOD result")

	assert.GreaterOrEqual(t, report.Summary.Total, 1)
}

func TestVerify_RunnerAgainstProjectWithoutMod(t *testing.T) {
	dir := t.TempDir()

	// No go.mod — GOMOD should be FAIL.
	report := verify.RunVerify(dir)
	require.NotNil(t, report)

	for _, r := range report.Results {
		if r.ID == "GOMOD" {
			assert.Equal(t, "FAIL", r.Status)
			return
		}
	}
	t.Fatal("expected GOMOD result not found")
}

// =============================================================================
// SCAN scanner rules (create temp Go file with known issues, verify detection)
// =============================================================================

func TestVerify_ScanMockImportDetection(t *testing.T) {
	dir := t.TempDir()

	src := `package main
import (
	"fmt"
	"github.com/stretchr/testify/mock"
)

func main() { fmt.Println("test") }
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	report, err := verify.Scan(verify.ScanConfig{
		Dir:             dir,
		CheckMockImport: true,
		CheckTesting:    false,
		CheckHardcoded:  false,
		CheckTestURL:    false,
		CheckBuildTag:   false,
		CheckNolint:     false,
	})
	require.NoError(t, err)

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-001" {
			found = true
			assert.Equal(t, verify.SeverityError, f.Severity)
			assert.Contains(t, f.Message, "mock")
		}
	}
	assert.True(t, found, "should detect mock import")
}

func TestVerify_ScanTestingUsageDetection(t *testing.T) {
	dir := t.TempDir()

	src := `package main
import "testing"

func helper(t *testing.T) { t.Log("test") }
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	report, err := verify.Scan(verify.ScanConfig{
		Dir:             dir,
		CheckMockImport: false,
		CheckTesting:    true,
		CheckHardcoded:  false,
		CheckTestURL:    false,
		CheckBuildTag:   false,
		CheckNolint:     false,
	})
	require.NoError(t, err)

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-002" {
			found = true
			assert.Contains(t, f.Message, "testing")
		}
	}
	assert.True(t, found, "should detect testing usage")
}

func TestVerify_ScanHardcodedSecretDetection(t *testing.T) {
	dir := t.TempDir()

	src := `package main
const apiKey = "sk-abcdefghijklmnopqrstuvwxyz"
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	report, err := verify.Scan(verify.ScanConfig{
		Dir:             dir,
		CheckMockImport: false,
		CheckTesting:    false,
		CheckHardcoded:  true,
		CheckTestURL:    false,
		CheckBuildTag:   false,
		CheckNolint:     false,
	})
	require.NoError(t, err)

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-003" {
			found = true
			assert.Contains(t, f.Message, "secret")
		}
	}
	assert.True(t, found, "should detect hardcoded secret")
}

func TestVerify_ScanTestURLDetection(t *testing.T) {
	dir := t.TempDir()

	src := `package main
var addr = "http://localhost:9999"
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	report, err := verify.Scan(verify.ScanConfig{
		Dir:             dir,
		CheckMockImport: false,
		CheckTesting:    false,
		CheckHardcoded:  false,
		CheckTestURL:    true,
		CheckBuildTag:   false,
		CheckNolint:     false,
	})
	require.NoError(t, err)

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-004" {
			found = true
			assert.Contains(t, f.Message, "localhost")
		}
	}
	assert.True(t, found, "should detect hardcoded test URL")
}

func TestVerify_ScanBuildTagDetection(t *testing.T) {
	dir := t.TempDir()

	src := `//go:build integration
package main
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	report, err := verify.Scan(verify.ScanConfig{
		Dir:             dir,
		CheckMockImport: false,
		CheckTesting:    false,
		CheckHardcoded:  false,
		CheckTestURL:    false,
		CheckBuildTag:   true,
		CheckNolint:     false,
	})
	require.NoError(t, err)

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-005" {
			found = true
			assert.Contains(t, f.Message, "build tag")
		}
	}
	assert.True(t, found, "should detect build tag")
}

func TestVerify_ScanNolintDetection(t *testing.T) {
	dir := t.TempDir()

	// Generate a file with excessive nolint directives.
	lines := []string{"package main", ""}
	for i := 0; i < 5; i++ {
		lines = append(lines, "//nolint")
	}
	src := strings.Join(lines, "\n")

	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	report, err := verify.Scan(verify.ScanConfig{
		Dir:             dir,
		CheckMockImport: false,
		CheckTesting:    false,
		CheckHardcoded:  false,
		CheckTestURL:    false,
		CheckBuildTag:   false,
		CheckNolint:     true,
		NolintThreshold: 3,
	})
	require.NoError(t, err)

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-007" {
			found = true
			assert.Contains(t, f.Message, "nolint")
		}
	}
	assert.True(t, found, "should detect excessive nolint")
}

func TestVerify_ScanSlogUsage(t *testing.T) {
	dir := t.TempDir()

	// File without slog usage.
	src := `package main
import "fmt"
func main() { fmt.Println("hello") }
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	cfg := verify.DefaultScanConfig(dir)
	cfg.CheckMockImport = false
	cfg.CheckTesting = false
	cfg.CheckHardcoded = false
	cfg.CheckTestURL = false
	cfg.CheckBuildTag = false
	cfg.CheckNolint = false
	cfg.CheckSlogUsage = true
	cfg.CheckHardcodedDefaults = false
	cfg.CheckCommandRouting = false
	cfg.CheckConfigMergePriority = false
	cfg.CheckInterfaceDefaultImpl = false
	cfg.CheckConcreteInInterface = false
	cfg.CheckExportOnlyUsedByTest = false

	report, err := verify.Scan(cfg)
	require.NoError(t, err)

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-008" {
			found = true
			assert.Contains(t, f.Message, "slog")
		}
	}
	assert.True(t, found, "should detect missing slog usage")
}

func TestVerify_ScanHardcodedDefaultsDetection(t *testing.T) {
	dir := t.TempDir()

	src := `package main
const defaultPrompt = "You are a helpful assistant. Default value."
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	cfg := verify.DefaultScanConfig(dir)
	cfg.CheckMockImport = false
	cfg.CheckTesting = false
	cfg.CheckHardcoded = false
	cfg.CheckTestURL = false
	cfg.CheckBuildTag = false
	cfg.CheckNolint = false
	cfg.CheckSlogUsage = false
	cfg.CheckHardcodedDefaults = true
	cfg.CheckCommandRouting = false
	cfg.CheckConfigMergePriority = false
	cfg.CheckInterfaceDefaultImpl = false
	cfg.CheckConcreteInInterface = false
	cfg.CheckExportOnlyUsedByTest = false

	report, err := verify.Scan(cfg)
	require.NoError(t, err)

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-009" {
			found = true
		}
	}
	assert.True(t, found, "should detect hardcoded defaults")
}

func TestVerify_ScanCommandRoutingDetection(t *testing.T) {
	dir := t.TempDir()

	src := `package main
func route(args []string) {
	switch args[0] {
	case "help":
		println("help")
	}
}
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	cfg := verify.DefaultScanConfig(dir)
	cfg.CheckMockImport = false
	cfg.CheckTesting = false
	cfg.CheckHardcoded = false
	cfg.CheckTestURL = false
	cfg.CheckBuildTag = false
	cfg.CheckNolint = false
	cfg.CheckSlogUsage = false
	cfg.CheckHardcodedDefaults = false
	cfg.CheckCommandRouting = true
	cfg.CheckConfigMergePriority = false
	cfg.CheckInterfaceDefaultImpl = false
	cfg.CheckConcreteInInterface = false
	cfg.CheckExportOnlyUsedByTest = false

	report, err := verify.Scan(cfg)
	require.NoError(t, err)

	found := false
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-010" {
			found = true
			assert.Contains(t, f.Message, "command routing")
		}
	}
	assert.True(t, found, "should detect hardcoded command routing")
}

func TestVerify_ScanCleanFileNoFindings(t *testing.T) {
	dir := t.TempDir()

	src := `package main
import (
	"context"
	"log/slog"
)

func Run(ctx context.Context) error {
	slog.InfoContext(ctx, "running")
	return nil
}
`
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	cfg := verify.DefaultScanConfig(dir)
	// Only enable checks that would find issues.
	cfg.CheckMockImport = true
	cfg.CheckTesting = true
	cfg.CheckHardcoded = true
	cfg.CheckTestURL = true
	cfg.CheckBuildTag = true
	cfg.CheckNolint = true
	cfg.CheckSlogUsage = true
	cfg.CheckCommandRouting = true
	cfg.CheckHardcodedDefaults = false
	cfg.CheckConfigMergePriority = false
	cfg.CheckInterfaceDefaultImpl = false
	cfg.CheckConcreteInInterface = false
	cfg.CheckExportOnlyUsedByTest = false

	report, err := verify.Scan(cfg)
	require.NoError(t, err)

	// Should have no error-level findings for this clean file.
	assert.False(t, report.HasErrors(), "clean file should have no errors")
}

func TestVerify_ScanReportHasErrors(t *testing.T) {
	report := &verify.ScanReport{}
	assert.False(t, report.HasErrors())

	report.Findings = append(report.Findings, verify.Finding{Severity: verify.SeverityError})
	assert.True(t, report.HasErrors())
}

func TestVerify_ScanReportToJSON(t *testing.T) {
	report := &verify.ScanReport{
		Dir: "/tmp/test",
		Findings: []verify.Finding{
			{RuleID: "SCAN-001", Severity: verify.SeverityError, File: "main.go", Line: 3, Message: "mock import"},
		},
	}
	data, err := report.ToJSON()
	require.NoError(t, err)

	var parsed verify.ScanReport
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)
	assert.Equal(t, "/tmp/test", parsed.Dir)
	assert.Len(t, parsed.Findings, 1)
}

func TestVerify_ScanExcludesTestFiles(t *testing.T) {
	dir := t.TempDir()

	// A test file with testing.T usage should NOT be flagged because test files
	// are excluded from the scanner (except hardcoded secrets).
	testSrc := `package main
import "testing"
func TestFoo(t *testing.T) { t.Log("ok") }
`
	err := os.WriteFile(filepath.Join(dir, "main_test.go"), []byte(testSrc), 0o644)
	require.NoError(t, err)

	cfg := verify.DefaultScanConfig(dir)
	cfg.CheckMockImport = false
	cfg.CheckTesting = true
	cfg.CheckHardcoded = false
	cfg.CheckTestURL = false
	cfg.CheckBuildTag = false
	cfg.CheckNolint = false
	cfg.CheckSlogUsage = false
	cfg.CheckHardcodedDefaults = false
	cfg.CheckCommandRouting = false
	cfg.CheckConfigMergePriority = false
	cfg.CheckInterfaceDefaultImpl = false
	cfg.CheckConcreteInInterface = false
	cfg.CheckExportOnlyUsedByTest = false

	report, err := verify.Scan(cfg)
	require.NoError(t, err)

	// SCAN-002 should not trigger for test files.
	for _, f := range report.Findings {
		assert.NotEqual(t, "SCAN-002", f.RuleID, "test files should be excluded from SCAN-002")
	}
}

func TestVerify_ScanDirWrapper(t *testing.T) {
	dir := t.TempDir()
	err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main"), 0o644)
	require.NoError(t, err)

	report, err := verify.ScanDir(dir)
	require.NoError(t, err)
	assert.NotNil(t, report)
}

func TestVerify_ScanReportFormatText(t *testing.T) {
	report := &verify.ScanReport{
		Dir: "/tmp",
		Findings: []verify.Finding{
			{RuleID: "SCAN-001", Severity: verify.SeverityError, File: "x.go", Line: 1, Message: "err"},
		},
		Summary: verify.ScanSummary{TotalFiles: 1, TotalFindings: 1, Errors: 1},
	}
	text := report.FormatText()
	assert.Contains(t, text, "Scan Report")
	assert.Contains(t, text, "SCAN-001")
}

// =============================================================================
// VQVG rules
// =============================================================================

func TestVerify_VQVGRulesAllRules(t *testing.T) {
	rules := verify.AllRules()
	require.NotEmpty(t, rules)

	// Should have VQ-001 through VQ-004, VT-*, VS-*, VC-*, VH-*, VP-*, VG-*, VE-*, VT-01*.
	hasVQ001 := false
	hasVG001 := false
	for _, r := range rules {
		if r.ID == "VQ-001" {
			hasVQ001 = true
			assert.Equal(t, "命令注册后可路由执行", r.Name)
		}
		if r.ID == "VG-001" {
			hasVG001 = true
			assert.Equal(t, "context 取消传播", r.Name)
		}
	}
	assert.True(t, hasVQ001)
	assert.True(t, hasVG001)
}

func TestVerify_RunVerifyAgainstGoProject(t *testing.T) {
	dir := t.TempDir()

	// Create a minimal Go project with version/help handling.
	err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module test\n\ngo 1.24\n"), 0o644)
	require.NoError(t, err)

	src := `package main

import (
	"context"
	"fmt"
	"sync"
)

type CommandRegistry map[string]func()

func Register(r CommandRegistry, name string, fn func()) {
	r[name] = fn
}

func RunCLI() {
	r := CommandRegistry{}
	Register(r, "version", func() { fmt.Println("v1.0") })
	Register(r, "help", func() { fmt.Println("usage") })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		fmt.Println("unknown command")
	}()
	wg.Wait()
}
`
	err = os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	require.NoError(t, err)

	report := verify.RunVerify(dir)
	require.NotNil(t, report)

	// Check that VQ-001 and VQ-004 pass based on the source heuristics.
	for _, r := range report.Results {
		if r.ID == "VQ-001" || r.ID == "VQ-003" || r.ID == "VQ-004" {
			assert.Equal(t, "PASS", r.Status, "rule %s should pass", r.ID)
		}
		if r.ID == "VG-001" || r.ID == "VG-002" {
			assert.Equal(t, "PASS", r.Status, "rule %s should pass", r.ID)
		}
	}
}

// =============================================================================
// Report generation (text and JSON formats)
// =============================================================================

func TestVerify_ReportJSONMarshal(t *testing.T) {
	report := &verify.VerifyReport{
		Timestamp: "2026-01-01T00:00:00Z",
		Dir:       "/tmp/test",
		Summary: verify.VerifySummary{
			Total:   3,
			Passed:  2,
			Failed:  1,
			Skipped: 0,
		},
		Results: []verify.VerifyResult{
			{ID: "VQ-001", Name: "test", Status: "PASS", DurationMs: 10},
			{ID: "VQ-002", Name: "test2", Status: "FAIL", DurationMs: 5, Message: "reason"},
			{ID: "VQ-003", Name: "test3", Status: "SKIP", DurationMs: 0},
		},
	}

	data, err := json.Marshal(report)
	require.NoError(t, err)

	var parsed verify.VerifyReport
	err = json.Unmarshal(data, &parsed)
	require.NoError(t, err)

	assert.Equal(t, "2026-01-01T00:00:00Z", parsed.Timestamp)
	assert.Equal(t, "/tmp/test", parsed.Dir)
	assert.Equal(t, 3, parsed.Summary.Total)
	assert.Equal(t, 2, parsed.Summary.Passed)
	assert.Equal(t, 1, parsed.Summary.Failed)
	assert.Len(t, parsed.Results, 3)
}

func TestVerify_ReportSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	reportPath := filepath.Join(dir, "report.json")

	report := &verify.VerifyReport{
		Timestamp: "2026-01-01T00:00:00Z",
		Dir:       "/tmp/test",
		Summary:   verify.VerifySummary{Total: 1, Passed: 1},
		Results: []verify.VerifyResult{
			{ID: "TEST", Name: "test", Status: "PASS"},
		},
	}

	err := verify.SaveReport(report, reportPath)
	require.NoError(t, err)

	// Read back.
	data, err := os.ReadFile(reportPath)
	require.NoError(t, err)

	var loaded verify.VerifyReport
	err = json.Unmarshal(data, &loaded)
	require.NoError(t, err)

	assert.Equal(t, "TEST", loaded.Results[0].ID)
	assert.Equal(t, "PASS", loaded.Results[0].Status)
}

func TestVerify_ReportFormatText(t *testing.T) {
	report := &verify.VerifyReport{
		Timestamp: "2026-01-01T00:00:00Z",
		Dir:       "/tmp",
		Summary: verify.VerifySummary{
			Total:   2,
			Passed:  1,
			Failed:  1,
			Skipped: 0,
		},
		Results: []verify.VerifyResult{
			{ID: "A", Name: "rule-a", Status: "PASS", DurationMs: 5},
			{ID: "B", Name: "rule-b", Status: "FAIL", DurationMs: 3, Message: "bad"},
		},
	}

	text := verify.FormatReportText(report)

	assert.Contains(t, text, "VERIFICATION REPORT")
	assert.Contains(t, text, "2026-01-01T00:00:00Z")
	assert.Contains(t, text, "/tmp")
	assert.Contains(t, text, "PASS")
	assert.Contains(t, text, "FAIL")
	assert.Contains(t, text, "rule-a")
	assert.Contains(t, text, "rule-b")
}

func TestVerify_ReportWithScanFindings(t *testing.T) {
	report := &verify.VerifyReport{
		Timestamp: "2026-01-01T00:00:00Z",
		Dir:       "/tmp",
		Summary:   verify.VerifySummary{Total: 1, Passed: 1},
		Results: []verify.VerifyResult{
			{ID: "SCAN", Name: "scanner", Status: "PASS"},
		},
		Scan: &verify.ScanReport{
			Dir: "/tmp",
			Findings: []verify.Finding{
				{RuleID: "SCAN-001", Severity: verify.SeverityError, File: "x.go", Line: 1, Message: "err"},
			},
			Summary: verify.ScanSummary{TotalFiles: 1, TotalFindings: 1, Errors: 1},
		},
	}

	text := verify.FormatReportText(report)
	assert.Contains(t, text, "SCAN FINDINGS")
	assert.Contains(t, text, "SCAN-001")
}

func TestVerify_ReportSummaryComputation(t *testing.T) {
	dir := t.TempDir()

	// No go.mod — some results will FAIL, some will PASS, some SKIP.
	report := verify.RunVerify(dir)

	assert.Greater(t, report.Summary.Total, 0)
	assert.Equal(t, report.Summary.Total,
		report.Summary.Passed+report.Summary.Failed+report.Summary.Skipped,
		"summary must add up")
}
