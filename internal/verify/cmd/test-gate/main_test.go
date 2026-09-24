package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunExecutesRealBehaviorTests(t *testing.T) {
	module := writeTestModule(t, "package fixture\n\nimport \"testing\"\n\nfunc TestBehavior(t *testing.T) {}\n")
	goCommand := mustGoCommand(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", module, "-go-command", goCommand}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr=%q, stdout=%q", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "go test gate passed: 1 test function(s) started, 1 passed") {
		t.Fatalf("passing test evidence missing from stdout: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "=== RUN   TestBehavior") {
		t.Fatalf("real go test output missing: %q", stdout.String())
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
	fakeGo := filepath.Join(module, "fake-go")
	if runtime.GOOS == "windows" {
		fakeGo += ".exe"
	}
	fakeSource := `package main
import (
	"fmt"
	"os"
)
func main() {
	fmt.Println("{\"Action\":\"run\",\"Package\":\"fixture\",\"Test\":\"TestInjected\"}")
	fmt.Println("{\"Action\":\"pass\",\"Package\":\"fixture\",\"Test\":\"TestInjected\"}")
	os.Exit(23)
}
`
	if err := os.WriteFile(filepath.Join(module, "fake.go"), []byte(fakeSource), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.Command(goCommand, "build", "-o", fakeGo, filepath.Join(module, "fake.go"))
	build.Dir = module
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build fake go process: %v: %s", err, output)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", module, "-go-command", fakeGo, "-package=fixture"}, &stdout, &stderr)
	if code != 23 {
		t.Fatalf("run() = %d, want original child exit code 23; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "go test observed 1 test function(s): 1 passed") {
		t.Fatalf("JSON test event summary missing: %q", stderr.String())
	}
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
