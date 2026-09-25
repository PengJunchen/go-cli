package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/verify/cmd/gatecapture"
)

func TestRunExecutesRealBehaviorTests(t *testing.T) {
	module := writeTestModule(t, "package fixture\n\nimport \"testing\"\n\nfunc TestBehavior(t *testing.T) { t.Parallel() }\n")
	goCommand := mustGoCommand(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", module, "-go-command", goCommand, "-go-arg=-count=2"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr=%q, stdout=%q", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "go test gate passed: 2 test function(s) started, 2 passed") {
		t.Fatalf("passing test evidence missing from stdout: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "=== RUN   TestBehavior") {
		t.Fatalf("real go test output missing: %q", stdout.String())
	}
}

func TestRunRepeatsFreshTestsByDefault(t *testing.T) {
	module := writeTestModule(t, "package fixture\n\nimport \"testing\"\n\nfunc TestBehavior(t *testing.T) {}\n")
	goCommand := mustGoCommand(t)
	for attempt := 1; attempt <= 2; attempt++ {
		var stdout, stderr bytes.Buffer
		code := run([]string{"-dir", module, "-go-command", goCommand}, &stdout, &stderr)
		if code != 0 || !strings.Contains(stdout.String(), "1 test function(s) started, 1 passed") {
			t.Fatalf("run %d = %d, expected fresh behavior-test evidence; stdout=%q stderr=%q", attempt, code, stdout.String(), stderr.String())
		}
	}
}

func TestRunRejectsZeroMatchingTests(t *testing.T) {
	module := writeTestModule(t, "package fixture\n\nimport \"testing\"\n\nfunc TestBehavior(t *testing.T) {}\n")
	goCommand := mustGoCommand(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", module, "-go-command", goCommand, "-package=.", "-go-arg=-run", "-go-arg=NoMatchingTest"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run() accepted zero matching tests; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "matched zero test functions") || !strings.Contains(combined, "no tests to run") {
		t.Fatalf("zero-match diagnostic lacks evidence: %q", combined)
	}
}

func TestRunRejectsPackageWithoutTestFiles(t *testing.T) {
	module := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module test-gate-empty\n\ngo 1.24.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "fixture.go"), []byte("package fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	goCommand := mustGoCommand(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", module, "-go-command", goCommand, "-package=."}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run() accepted a package without test files; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "matched zero test functions") {
		t.Fatalf("no-test diagnostic missing: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "no test files") {
		t.Fatalf("Go's no-test package evidence missing: %q", stdout.String())
	}
}

func TestRunPreservesGoTestExitCodeAndFailureOutput(t *testing.T) {
	module := writeTestModule(t, "package fixture\n\nimport \"testing\"\n\nfunc TestBehaviorFailure(t *testing.T) { t.Fatal(\"expected failure evidence\") }\n")
	goCommand := mustGoCommand(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", module, "-go-command", goCommand}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() = %d, want original go test exit code 1; stderr=%q", code, stderr.String())
	}
	combined := stdout.String() + stderr.String()
	if !strings.Contains(combined, "TestBehaviorFailure") || !strings.Contains(combined, "expected failure evidence") {
		t.Fatalf("failed test evidence missing: %q", combined)
	}
}

func TestRunPreservesNonstandardChildExitCode(t *testing.T) {
	module := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module test-gate-helper\n\ngo 1.24.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	goCommand := mustGoCommand(t)
	fakeSource := fakeGoEventSource([]string{
		`{"Action":"start","Package":"fixture"}`,
		`{"Action":"run","Package":"fixture","Test":"TestInjected"}`,
		`{"Action":"pass","Package":"fixture","Test":"TestInjected"}`,
		`{"Action":"pass","Package":"fixture"}`,
	}, "os.Exit(23)")
	fakeGo := buildFakeGoProcess(t, module, goCommand, fakeSource)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", module, "-go-command", fakeGo, "-package=fixture"}, &stdout, &stderr)
	if code != 23 {
		t.Fatalf("run() = %d, want original child exit code 23; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "go test observed 1 test function(s): 1 passed") {
		t.Fatalf("JSON test event summary missing: %q", stderr.String())
	}
}

func TestRunRejectsFailureEventsWhenChildExitsZero(t *testing.T) {
	module := t.TempDir()
	goCommand := mustGoCommand(t)
	fakeGo := buildFakeGoProcess(t, module, goCommand, fakeGoEventSource([]string{
		`{"Action":"start","Package":"fixture"}`,
		`{"Action":"run","Package":"fixture","Test":"TestPass"}`,
		`{"Action":"pass","Package":"fixture","Test":"TestPass"}`,
		`{"Action":"run","Package":"fixture","Test":"TestFailure"}`,
		`{"Action":"fail","Package":"fixture","Test":"TestFailure"}`,
		`{"Action":"fail","Package":"fixture"}`,
	}, ""))
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", module, "-go-command", fakeGo, "-package=fixture"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run() accepted failed test/package events from a zero-exit child; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "TestFailure") || !strings.Contains(stderr.String(), "package fixture failed") {
		t.Fatalf("failure-event diagnostics missing: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunRejectsStartedTestWithoutTerminalEvent(t *testing.T) {
	module := t.TempDir()
	goCommand := mustGoCommand(t)
	fakeGo := buildFakeGoProcess(t, module, goCommand, fakeGoEventSource([]string{
		`{"Action":"start","Package":"fixture"}`,
		`{"Action":"run","Package":"fixture","Test":"TestComplete"}`,
		`{"Action":"pass","Package":"fixture","Test":"TestComplete"}`,
		`{"Action":"run","Package":"fixture","Test":"TestUnfinished"}`,
	}, ""))
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", module, "-go-command", fakeGo, "-package=fixture"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("run() accepted an unterminated test event; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "TestUnfinished") || !strings.Contains(stderr.String(), "no terminal event") {
		t.Fatalf("unterminated-test diagnostic missing: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunRejectsMalformedEventShapesAmongPassingEvents(t *testing.T) {
	tests := []struct {
		name   string
		event  string
		needle string
	}{
		{name: "unknown action", event: `{"Action":"mystery","Package":"fixture"}`, needle: "unknown action"},
		{name: "missing package", event: `{"Action":"output","Output":"orphan"}`, needle: "missing package"},
		{name: "test action without test name", event: `{"Action":"run","Package":"fixture"}`, needle: "requires a test name"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			module := t.TempDir()
			goCommand := mustGoCommand(t)
			fakeSource := fakeGoEventSource([]string{
				`{"Action":"start","Package":"fixture"}`,
				`{"Action":"run","Package":"fixture","Test":"TestPass"}`,
				`{"Action":"pass","Package":"fixture","Test":"TestPass"}`,
				test.event,
			}, "")
			fakeGo := buildFakeGoProcess(t, module, goCommand, fakeSource)
			var stdout, stderr bytes.Buffer
			code := run([]string{"-dir", module, "-go-command", fakeGo, "-package=fixture"}, &stdout, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), test.needle) {
				t.Fatalf("run() = %d, expected rejection containing %q; stdout=%q stderr=%q", code, test.needle, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunRejectsInvalidPackageEventOrder(t *testing.T) {
	tests := []struct {
		name   string
		events []string
		needle string
	}{
		{
			name: "test before package start",
			events: []string{
				`{"Action":"run","Package":"fixture","Test":"TestBehavior"}`,
				`{"Action":"pass","Package":"fixture","Test":"TestBehavior"}`,
				`{"Action":"pass","Package":"fixture"}`,
			},
			needle: "no preceding start",
		},
		{
			name: "package terminal before test",
			events: []string{
				`{"Action":"start","Package":"fixture"}`,
				`{"Action":"pass","Package":"fixture"}`,
				`{"Action":"run","Package":"fixture","Test":"TestBehavior"}`,
			},
			needle: "after package fixture terminal event",
		},
		{
			name: "event after package terminal",
			events: []string{
				`{"Action":"start","Package":"fixture"}`,
				`{"Action":"run","Package":"fixture","Test":"TestBehavior"}`,
				`{"Action":"pass","Package":"fixture","Test":"TestBehavior"}`,
				`{"Action":"pass","Package":"fixture"}`,
				`{"Action":"output","Package":"fixture","Output":"late"}`,
			},
			needle: "after package fixture terminal event",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			module := t.TempDir()
			fakeGo := buildFakeGoProcess(t, module, mustGoCommand(t), fakeGoEventSource(test.events, ""))
			var stdout, stderr bytes.Buffer
			code := run([]string{"-dir", module, "-go-command", fakeGo, "-package=fixture"}, &stdout, &stderr)
			if code == 0 || !strings.Contains(stderr.String(), test.needle) {
				t.Fatalf("run() = %d, expected package-lifecycle rejection containing %q; stdout=%q stderr=%q", code, test.needle, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunAcceptsBenchmarkTerminalAction(t *testing.T) {
	module := t.TempDir()
	fakeGo := buildFakeGoProcess(t, module, mustGoCommand(t), fakeGoEventSource([]string{
		`{"Action":"start","Package":"fixture"}`,
		`{"Action":"run","Package":"fixture","Test":"TestBehavior"}`,
		`{"Action":"pass","Package":"fixture","Test":"TestBehavior"}`,
		`{"Action":"run","Package":"fixture","Test":"BenchmarkBehavior"}`,
		`{"Action":"output","Package":"fixture","Test":"BenchmarkBehavior","Output":"benchmark output\n"}`,
		`{"Action":"bench","Package":"fixture","Test":"BenchmarkBehavior"}`,
		`{"Action":"pass","Package":"fixture"}`,
	}, ""))
	var stdout, stderr bytes.Buffer
	code := run([]string{"-go-command", fakeGo, "-package=fixture"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "1 passed, 1 benchmarked") {
		t.Fatalf("run() = %d, want test and legal benchmark evidence; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunDoesNotCountBenchmarkAsPassingTestEvidence(t *testing.T) {
	module := t.TempDir()
	fakeGo := buildFakeGoProcess(t, module, mustGoCommand(t), fakeGoEventSource([]string{
		`{"Action":"start","Package":"fixture"}`,
		`{"Action":"run","Package":"fixture","Test":"BenchmarkBehavior"}`,
		`{"Action":"bench","Package":"fixture","Test":"BenchmarkBehavior"}`,
		`{"Action":"pass","Package":"fixture"}`,
	}, ""))
	var stdout, stderr bytes.Buffer
	code := run([]string{"-go-command", fakeGo, "-package=fixture"}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), "no test or example passed") {
		t.Fatalf("run() = %d, want benchmark-only evidence rejected; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestRunRejectsOutputBeyondCaptureLimit(t *testing.T) {
	module := t.TempDir()
	goCommand := mustGoCommand(t)
	validEvents := []string{
		`{"Action":"start","Package":"fixture"}`,
		`{"Action":"run","Package":"fixture","Test":"TestBehavior"}`,
		`{"Action":"pass","Package":"fixture","Test":"TestBehavior"}`,
		`{"Action":"pass","Package":"fixture"}`,
	}
	tests := []struct {
		name   string
		limits gatecapture.Limits
		body   string
		needle string
	}{
		{name: "stdout", limits: gatecapture.Limits{StdoutBytes: 32, StderrBytes: 1024, WaitDelay: 100 * time.Millisecond}, needle: "stdout exceeded capture limit"},
		{name: "stderr", limits: gatecapture.Limits{StdoutBytes: 1024, StderrBytes: 32, WaitDelay: 100 * time.Millisecond}, body: `fmt.Fprint(os.Stderr, strings.Repeat("x", 128))`, needle: "stderr exceeded capture limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fakeGo := buildFakeGoProcess(t, module, goCommand, fakeGoEventSource(validEvents, test.body))
			var stdout, stderr bytes.Buffer
			code := runWithCaptureLimits([]string{"-dir", module, "-go-command", fakeGo, "-package=fixture"}, &stdout, &stderr, test.limits)
			if code == 0 || !strings.Contains(stderr.String(), test.needle) {
				t.Fatalf("runWithCaptureLimits() = %d, expected explicit cap failure %q; stdout=%q stderr=%q", code, test.needle, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunTimeoutReturnsWhenDescendantHoldsPipes(t *testing.T) {
	if !processGroupsSupported() {
		t.Skip("owned process-group cleanup is supported only on Darwin and Linux")
	}
	module := t.TempDir()
	goCommand := mustGoCommand(t)
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	t.Cleanup(func() { cleanupDescendantFromFile(pidFile) })
	t.Setenv("VERIFY_TEST_GATE_PID_FILE", pidFile)
	fakeSource := `package main
import (
	"os"
	"os/exec"
	"strconv"
	"time"
)
func main() {
	if len(os.Args) > 1 && os.Args[1] == "hold" {
		time.Sleep(10 * time.Second)
		return
	}
	child := exec.Command(os.Args[0], "hold")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil { os.Exit(21) }
	_ = os.WriteFile(os.Getenv("VERIFY_TEST_GATE_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
}
`
	fakeGo := buildFakeGoProcess(t, module, goCommand, fakeSource)
	var stdout, stderr bytes.Buffer
	started := time.Now()
	code := runWithCaptureLimits([]string{"-dir", module, "-go-command", fakeGo, "-timeout=100ms", "-package=fixture"}, &stdout, &stderr, gatecapture.Limits{StdoutBytes: 1024, StderrBytes: 1024, WaitDelay: 150 * time.Millisecond})
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("test-gate run took %s with a 100ms timeout and 150ms WaitDelay; stderr=%q", elapsed, stderr.String())
	}
	if code != 124 || !strings.Contains(stderr.String(), "timed out after 100ms") {
		t.Fatalf("runWithCaptureLimits() = %d, want bounded timeout 124; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertDescendantStoppedFromFile(t, pidFile)
}

func writeTestModule(t *testing.T, testSource string) string {
	t.Helper()
	module := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module test-gate-fixture\n\ngo 1.24.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "fixture_test.go"), []byte(testSource), 0o600); err != nil {
		t.Fatal(err)
	}
	return module
}

func mustGoCommand(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func buildFakeGoProcess(t *testing.T, module, goCommand, source string) string {
	t.Helper()
	fakeGo := filepath.Join(module, "fake-go")
	if runtime.GOOS == "windows" {
		fakeGo += ".exe"
	}
	sourcePath := filepath.Join(module, "fake.go")
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command(goCommand, "build", "-o", fakeGo, sourcePath)
	build.Dir = module
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake go process: %v: %s", err, output)
	}
	return fakeGo
}

func fakeGoEventSource(events []string, tail string) string {
	var source strings.Builder
	source.WriteString("package main\nimport (\"fmt\"; \"os\"; \"strings\")\nfunc main() {\n_ = os.Args; _ = strings.Repeat\n")
	for _, event := range events {
		_, _ = fmt.Fprintf(&source, "fmt.Println(%s)\n", strconv.Quote(event))
	}
	if tail != "" {
		source.WriteString(tail + "\n")
	}
	source.WriteString("}\n")
	return source.String()
}
