package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/verify/cmd/gatecapture"
)

const scannerFixtureModeEnv = "VERIFY_SCANNER_GATE_FIXTURE_MODE"

const scannerFixtureReport = `{"dir":"internal","findings":[],"summary":{"total_files":1,"total_findings":0,"errors":0,"warnings":0,"infos":0}}`

func TestScannerGateProcessFixture(t *testing.T) {
	mode := os.Getenv(scannerFixtureModeEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "valid":
		_, _ = fmt.Fprintln(os.Stdout, scannerFixtureReport)
		os.Exit(0)
	case "exit23":
		_, _ = fmt.Fprintln(os.Stdout, scannerFixtureReport)
		os.Exit(23)
	case "malformed":
		_, _ = fmt.Fprintln(os.Stdout, `{"dir":`)
		os.Exit(0)
	case "partial":
		_, _ = fmt.Fprintln(os.Stdout, `{"dir":"internal","summary":{}}`)
		os.Exit(0)
	case "summary-mismatch":
		_, _ = fmt.Fprintln(os.Stdout, `{"dir":"internal","findings":[],"summary":{"total_files":1,"total_findings":1,"errors":0,"warnings":1,"infos":0}}`)
		os.Exit(0)
	case "error-report":
		_, _ = fmt.Fprintln(os.Stdout, `{"dir":"internal","findings":[{"rule_id":"SCAN-999","file":"internal/example.go","line":1,"severity":"ERROR","message":"fixture error"}],"summary":{"total_files":1,"total_findings":1,"errors":1,"warnings":0,"infos":0}}`)
		os.Exit(0)
	case "stdout":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 128))
		os.Exit(0)
	case "stderr":
		_, _ = fmt.Fprintln(os.Stdout, scannerFixtureReport)
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("x", 128))
		os.Exit(0)
	case "sleep":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "spawn-holder":
		pidFile := os.Getenv("VERIFY_SCANNER_GATE_PID_FILE")
		child := exec.Command(os.Args[0], "-test.run=^TestScannerGateProcessFixture$")
		child.Env = envWith(scannerFixtureModeEnv, "hold")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "start pipe holder: %v\n", err)
			os.Exit(21)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "write pipe holder pid: %v\n", err)
			os.Exit(22)
		}
		os.Exit(0)
	case "hold":
		time.Sleep(10 * time.Second)
		os.Exit(0)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown scanner fixture mode %q\n", mode)
		os.Exit(23)
	}
}

func TestRunRejectsInvalidScannerResults(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		needle string
		code   int
	}{
		{name: "nonzero subprocess", mode: "exit23", needle: "scanner command failed", code: 1},
		{name: "malformed JSON", mode: "malformed", needle: "scanner produced invalid JSON", code: 1},
		{name: "partial report", mode: "partial", needle: "missing or null findings", code: 1},
		{name: "summary mismatch", mode: "summary-mismatch", needle: "summary total_findings=1 does not match 0 findings", code: 1},
		{name: "ERROR finding", mode: "error-report", needle: "scanner reported 1 ERROR finding(s)", code: 1},
		{name: "valid report", mode: "valid", code: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GORACE", "atexit_sleep_ms=0")
			t.Setenv(scannerFixtureModeEnv, test.mode)
			var stdout, stderr strings.Builder
			code := run(scannerFixtureArgs("1s"), &stdout, &stderr)
			if code != test.code || (test.needle != "" && !strings.Contains(stderr.String(), test.needle)) {
				t.Fatalf("run() = %d, want %d with diagnostic %q; stdout=%q stderr=%q", code, test.code, test.needle, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunRejectsOutputBeyondCaptureLimit(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		limits gatecapture.Limits
		needle string
	}{
		{name: "stdout", mode: "stdout", limits: gatecapture.Limits{StdoutBytes: 32, StderrBytes: 1024, WaitDelay: 100 * time.Millisecond}, needle: "stdout exceeded capture limit"},
		{name: "stderr", mode: "stderr", limits: gatecapture.Limits{StdoutBytes: 1024, StderrBytes: 32, WaitDelay: 100 * time.Millisecond}, needle: "stderr exceeded capture limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(scannerFixtureModeEnv, test.mode)
			var stdout, stderr strings.Builder
			code := runWithCaptureLimits(scannerFixtureArgs("1s"), &stdout, &stderr, test.limits)
			if code == 0 || !strings.Contains(stderr.String(), test.needle) {
				t.Fatalf("runWithCaptureLimits() = %d, expected explicit cap failure %q; stdout=%q stderr=%q", code, test.needle, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunTimeoutReturnsWhenScannerDescendantHoldsPipes(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Setenv(scannerFixtureModeEnv, "spawn-holder")
	t.Setenv("VERIFY_SCANNER_GATE_PID_FILE", pidFile)
	var stdout, stderr strings.Builder
	started := time.Now()
	code := runWithCaptureLimits(scannerFixtureArgs("5s"), &stdout, &stderr, gatecapture.Limits{StdoutBytes: 1024, StderrBytes: 1024, WaitDelay: 150 * time.Millisecond})
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("scanner-gate run took %s with 150ms WaitDelay; stderr=%q", elapsed, stderr.String())
	}
	if code == 0 || !strings.Contains(stderr.String(), "scanner command failed") {
		t.Fatalf("runWithCaptureLimits() = %d, want incomplete pipe failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	killProcessFromFile(t, pidFile)
}

func TestRunReturnsAfterScannerTimeout(t *testing.T) {
	t.Setenv(scannerFixtureModeEnv, "sleep")
	var stdout, stderr strings.Builder
	started := time.Now()
	code := runWithCaptureLimits(scannerFixtureArgs("100ms"), &stdout, &stderr, gatecapture.Limits{StdoutBytes: 1024, StderrBytes: 1024, WaitDelay: 150 * time.Millisecond})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("scanner-gate run took %s with a 100ms timeout and 150ms WaitDelay; stderr=%q", elapsed, stderr.String())
	}
	if code == 0 || !strings.Contains(stderr.String(), "timed out after 100ms") {
		t.Fatalf("runWithCaptureLimits() = %d, want bounded timeout failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func scannerFixtureArgs(timeout string) []string {
	return []string{
		"-dir", "./internal",
		"-scanner-command", os.Args[0],
		"-timeout", timeout,
		"-scanner-arg=-test.run=^TestScannerGateProcessFixture$",
	}
}

func killProcessFromFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pipe holder pid: %v", err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatalf("parse pipe holder pid %q: %v", data, err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find pipe holder process %d: %v", pid, err)
	}
	_ = process.Kill()
}

func envWith(name, value string) []string {
	prefix := name + "="
	env := os.Environ()
	filtered := env[:0]
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			filtered = append(filtered, item)
		}
	}
	return append(filtered, prefix+value)
}
