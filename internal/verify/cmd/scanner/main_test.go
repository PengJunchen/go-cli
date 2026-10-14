package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// writeCleanProject creates a temp dir with a clean Go source file that
// will not trigger any scanner findings.
func writeCleanProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	code := `package example

import "log/slog"

func Hello() string {
	slog.Info("hello")
	return "hello"
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(code), 0o600))
	return dir
}

// writeBypassProject creates a temp dir with a production file that imports
// a mock package, which will trigger an ERROR finding.
func writeBypassProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	code := `package example

import "github.com/stretchr/testify/mock"

type S struct{ m mock.Mock }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "service.go"), []byte(code), 0o600))
	return dir
}

func TestRun_TextFormatClean(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	dir := writeCleanProject(t)
	var stdout, stderr bytes.Buffer

	code := run([]string{"-dir", dir, "-format", "text"}, &stdout, &stderr)
	assert.Equal(t, 0, code, "clean project should exit 0")
	assert.Empty(t, stderr.String())
	assert.Contains(t, stdout.String(), "Scan Report:")
	assert.Contains(t, stdout.String(), "Files scanned:")
	assert.Contains(t, stdout.String(), "Findings: 0")
}

func TestRun_JSONFormatClean(t *testing.T) {
	dir := writeCleanProject(t)
	var stdout, stderr bytes.Buffer

	code := run([]string{"-dir", dir, "-format", "json"}, &stdout, &stderr)
	assert.Equal(t, 0, code, "clean project should exit 0")
	assert.Empty(t, stderr.String())

	var report verify.ScanReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	assert.Equal(t, dir, report.Dir)
	assert.Equal(t, 0, report.Summary.TotalFindings)
}

func TestRun_ErrorFinding_NonZeroExit(t *testing.T) {
	dir := writeBypassProject(t)
	var stdout, stderr bytes.Buffer

	code := run([]string{"-dir", dir, "-format", "text"}, &stdout, &stderr)
	assert.Equal(t, 1, code, "ERROR finding should produce non-zero exit")
	assert.Empty(t, stderr.String())
	assert.Contains(t, stdout.String(), "SCAN-001")
}

func TestRun_JSONErrorFinding_NonZeroExit(t *testing.T) {
	dir := writeBypassProject(t)
	var stdout, stderr bytes.Buffer

	code := run([]string{"-dir", dir, "-format", "json"}, &stdout, &stderr)
	assert.Equal(t, 1, code, "ERROR finding should produce non-zero exit")

	var report verify.ScanReport
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.NotEmpty(t, report.Findings)
	var foundErrorFinding bool
	for _, f := range report.Findings {
		if f.RuleID == "SCAN-001" && f.Severity == verify.SeverityError {
			foundErrorFinding = true
		}
	}
	assert.True(t, foundErrorFinding, "expected SCAN-001 ERROR finding in JSON output")
}

func TestRun_InvalidPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", "/nonexistent/path/xxx", "-format", "text"}, &stdout, &stderr)
	assert.Equal(t, 1, code, "scan error should produce non-zero exit")
	assert.Empty(t, stdout.String())
	assert.Contains(t, stderr.String(), "scan error:")
}

func TestRun_UnknownFormat(t *testing.T) {
	dir := writeCleanProject(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", dir, "-format", "yaml"}, &stdout, &stderr)
	assert.Equal(t, 1, code, "unknown format should produce non-zero exit")
	assert.Contains(t, stderr.String(), "unknown format: yaml")
}

func TestRun_InvalidFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-bogus-flag"}, &stdout, &stderr)
	assert.Equal(t, 2, code, "flag parse error should produce exit 2")
}

func TestRun_IncludeTests(t *testing.T) {
	dir := writeCleanProject(t)
	// Add a test file that would be skipped by default.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main_test.go"),
		[]byte("package example\n"), 0o600))

	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", dir, "-include-tests", "-format", "text"}, &stdout, &stderr)
	assert.Equal(t, 0, code)
	// With include-tests, the _test.go file is scanned too (main.go + main_test.go).
	assert.Contains(t, stdout.String(), "Files scanned: 2")
}

func TestRun_DefaultDir(t *testing.T) {
	// With default -dir ".", we scan the scanner package itself (main.go).
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	assert.Equal(t, 0, code)
	assert.Contains(t, stdout.String(), "Scan Report:")
}

func TestRun_EmptyOutputNoStderrOnSuccess(t *testing.T) {
	dir := writeCleanProject(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", dir}, &stdout, &stderr)
	assert.Equal(t, 0, code)
	assert.Empty(t, stderr.String())
	assert.NotEmpty(t, stdout.String())
	// Ensure the text output is not the literal JSON braces.
	assert.False(t, strings.HasPrefix(stdout.String(), "{"))
}

func TestScannerProcessFailureFailsGate(t *testing.T) {
	output, err := runScannerGateFixture(t, "process-failure")
	assert.Error(t, err, "a non-zero scanner process must fail the gate")
	assert.Contains(t, output, "scanner command failed")
	assert.Contains(t, output, "exit status 23")
}

func TestScannerInvalidJSONFailsGate(t *testing.T) {
	output, err := runScannerGateFixture(t, "invalid-json")
	assert.Error(t, err, "malformed scanner JSON must fail the gate")
	assert.Contains(t, output, "invalid JSON")
}

func TestScannerErrorFindingFailsGate(t *testing.T) {
	output, err := runScannerGateFixture(t, "error-json")
	assert.Error(t, err, "an ERROR in valid scanner JSON must fail the gate")
	assert.Contains(t, output, "SCAN-012")
	assert.Contains(t, output, "scanner reported 1 ERROR finding")
}

func runScannerGateFixture(t *testing.T, mode string) (string, error) {
	t.Helper()

	testBinary, err := os.Executable()
	require.NoError(t, err)
	repoRoot := filepath.Clean(filepath.Join("..", "..", "..", ".."))
	cmd := exec.Command("go", "run", "./internal/verify/cmd/scanner-gate",
		"-dir", "./internal",
		"-scanner-command", testBinary,
		"-scanner-arg=-test.run=^TestScannerGateSubprocessFixture$",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "GO_CLI_SCANNER_GATE_FIXTURE_MODE="+mode)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestMain(m *testing.M) {
	if mode := os.Getenv("GO_CLI_SCANNER_GATE_FIXTURE_MODE"); mode != "" {
		switch mode {
		case "process-failure":
			_, _ = fmt.Fprint(os.Stdout, `{"dir":".","findings":[],"summary":{"total_files":1,"total_findings":0,"errors":0,"warnings":0,"infos":0}}`)
			os.Exit(23)
		case "invalid-json":
			_, _ = fmt.Fprint(os.Stdout, "{broken-json")
			os.Exit(0)
		case "error-json":
			_, _ = fmt.Fprint(os.Stdout, `{"dir":".","findings":[{"rule_id":"SCAN-012","file":"core.go","line":9,"severity":"ERROR","message":"unresolved interface"}],"summary":{"total_files":1,"total_findings":1,"errors":1,"warnings":0,"infos":0}}`)
			os.Exit(0)
		default:
			os.Exit(24)
		}
	}
	os.Exit(m.Run())
}
