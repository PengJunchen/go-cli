// verify-test-gate runs Go tests through the JSON event stream and rejects
// successful commands that did not actually execute passing test functions.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/verify/cmd/gatecapture"
)

const defaultTestTimeout = 15 * time.Minute

var defaultCaptureLimits = gatecapture.Limits{
	StdoutBytes: 128 << 20,
	StderrBytes: 16 << 20,
	WaitDelay:   gatecapture.DefaultWaitDelay,
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, " ") }

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type testEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

type testCounts struct {
	Packages           int
	Started            int
	Passed             int
	Benchmarked        int
	Failed             int
	PackageFailed      int
	Skipped            int
	FailedDetails      []string
	Unfinished         []string
	UnfinishedPackages []string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithCaptureLimits(args, stdout, stderr, defaultCaptureLimits)
}

func runWithCaptureLimits(args []string, stdout, stderr io.Writer, limits gatecapture.Limits) int {
	fs := flag.NewFlagSet("verify-test-gate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "working directory for go test")
	goCommand := fs.String("go-command", "go", "Go executable")
	timeout := fs.Duration("timeout", defaultTestTimeout, "maximum go test runtime")
	var goArgs stringList
	var packages stringList
	fs.Var(&goArgs, "go-arg", "one go test argument; may be repeated")
	fs.Var(&packages, "package", "package pattern to test; may be repeated (defaults to ./...)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected arguments: %s\n", strings.Join(fs.Args(), " ")) //nolint:errcheck // CLI diagnostic is best-effort
		return 2
	}
	if *goCommand == "" {
		_, _ = fmt.Fprintln(stderr, "go-command is required") //nolint:errcheck // CLI diagnostic is best-effort
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "timeout must be positive") //nolint:errcheck // CLI diagnostic is best-effort
		return 2
	}
	for _, arg := range goArgs {
		if arg == "-json" || strings.HasPrefix(arg, "-json=") {
			_, _ = fmt.Fprintln(stderr, "go test JSON output is controlled by test-gate; do not pass -json through -go-arg") //nolint:errcheck // CLI diagnostic is best-effort
			return 2
		}
	}
	for _, pattern := range packages {
		if strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "-") {
			_, _ = fmt.Fprintf(stderr, "invalid package pattern %q\n", pattern) //nolint:errcheck // CLI diagnostic is best-effort
			return 2
		}
	}
	if len(packages) == 0 {
		packages = append(packages, "./...")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	// Disable the Go test cache by default so a passing gate carries run events
	// from this invocation. A caller's explicit -count option remains later and
	// can request multiple executions such as -count=2.
	commandArgs := append([]string{"test", "-json", "-count=1"}, goArgs...)
	commandArgs = append(commandArgs, packages...)
	cmd := exec.CommandContext(ctx, *goCommand, commandArgs...)
	cmd.Dir = *dir
	capture, err := gatecapture.Run(cmd, limits)

	events, counts, decodeErr := parseTestEvents(capture.Stdout)
	if decodeErr == nil {
		writeTestOutput(stdout, events)
	} else {
		writeDiagnostic(stderr, "go test stdout", capture.Stdout)
	}
	writeDiagnostic(stderr, "go test stderr", capture.Stderr)
	if capture.StdoutExceeded {
		_, _ = fmt.Fprintf(stderr, "go test stdout exceeded capture limit of %d bytes; test evidence is incomplete\n", limits.StdoutBytes) //nolint:errcheck // CLI diagnostic is best-effort
	}
	if capture.StderrExceeded {
		_, _ = fmt.Fprintf(stderr, "go test stderr exceeded capture limit of %d bytes\n", limits.StderrBytes) //nolint:errcheck // CLI diagnostic is best-effort
	}

	if decodeErr != nil {
		_, _ = fmt.Fprintf(stderr, "go test emitted invalid JSON events: %v\n", decodeErr) //nolint:errcheck // CLI diagnostic is best-effort
	}
	if len(counts.FailedDetails) > 0 {
		for _, failure := range counts.FailedDetails {
			_, _ = fmt.Fprintf(stderr, "go test event failure: %s\n", failure) //nolint:errcheck // CLI diagnostic is best-effort
		}
	}
	if capture.StdoutExceeded || capture.StderrExceeded {
		return 1
	}
	if err != nil {
		if counts.Started > 0 {
			_, _ = fmt.Fprintf(stderr, "go test observed %d test function(s): %d passed, %d failed, %d skipped across %d package(s)\n", counts.Started, counts.Passed, counts.Failed, counts.Skipped, counts.Packages) //nolint:errcheck // CLI diagnostic is best-effort
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_, _ = fmt.Fprintf(stderr, "go test timed out after %s\n", *timeout) //nolint:errcheck // CLI diagnostic is best-effort
			return 124
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			_, _ = fmt.Fprintf(stderr, "go test command failed with exit status %d (%s %s)\n", exitErr.ExitCode(), *goCommand, strings.Join(commandArgs, " ")) //nolint:errcheck // CLI diagnostic is best-effort
			return exitErr.ExitCode()
		}
		_, _ = fmt.Fprintf(stderr, "could not start go test command %q: %v\n", *goCommand, err) //nolint:errcheck // CLI diagnostic is best-effort
		return 1
	}
	if decodeErr != nil {
		return 1
	}
	if counts.Failed > 0 || counts.PackageFailed > 0 {
		_, _ = fmt.Fprintf(stderr, "go test JSON reports %d failed test event(s) and %d failed package event(s)\n", counts.Failed, counts.PackageFailed) //nolint:errcheck // CLI diagnostic is best-effort
		return 1
	}
	if len(counts.Unfinished) > 0 {
		for _, testName := range counts.Unfinished {
			_, _ = fmt.Fprintf(stderr, "go test test %s has no terminal event (pass, fail, or skip)\n", testName) //nolint:errcheck // CLI diagnostic is best-effort
		}
		return 1
	}
	if len(counts.UnfinishedPackages) > 0 {
		for _, packageName := range counts.UnfinishedPackages {
			_, _ = fmt.Fprintf(stderr, "go test package %s has no terminal event (pass, fail, or skip)\n", packageName) //nolint:errcheck // CLI diagnostic is best-effort
		}
		return 1
	}
	if counts.Started == 0 {
		_, _ = fmt.Fprintf(stderr, "go test matched zero test functions across %d package(s); command: %s %s\n", counts.Packages, *goCommand, strings.Join(commandArgs, " ")) //nolint:errcheck // CLI diagnostic is best-effort
		return 1
	}
	if counts.Passed == 0 {
		_, _ = fmt.Fprintf(stderr, "go test ran %d test function(s) but no test or example passed (benchmarked=%d failed=%d skipped=%d packages=%d)\n", counts.Started, counts.Benchmarked, counts.Failed, counts.Skipped, counts.Packages) //nolint:errcheck // CLI diagnostic is best-effort
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "go test gate passed: %d test function(s) started, %d passed, %d benchmarked, %d failed, %d skipped across %d package(s)\n", counts.Started, counts.Passed, counts.Benchmarked, counts.Failed, counts.Skipped, counts.Packages) //nolint:errcheck // CLI output is best-effort
	return 0
}

func parseTestEvents(data []byte) ([]testEvent, testCounts, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var events []testEvent
	packages := make(map[string]struct{})
	packageStates := make(map[string]packageLifecycle)
	tests := make(map[testKey]testLifecycle)
	counts := testCounts{}
	failures := make(map[string]struct{})
	addFailure := func(detail string) {
		failures[detail] = struct{}{}
	}
	for {
		var event testEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return events, counts, fmt.Errorf("decode event: %w", err)
		}
		eventNumber := len(events) + 1
		if strings.TrimSpace(event.Package) == "" {
			return events, counts, fmt.Errorf("event %d action %q has missing package", eventNumber, event.Action)
		}
		packages[event.Package] = struct{}{}
		events = append(events, event)
		if event.Action == "start" {
			if event.Test != "" {
				return events, counts, fmt.Errorf("event %d action start must not name a test", eventNumber)
			}
			if packageStates[event.Package].started {
				return events, counts, fmt.Errorf("event %d repeats package start for %s", eventNumber, event.Package)
			}
			packageStates[event.Package] = packageLifecycle{started: true}
			continue
		}
		packageState, packageStarted := packageStates[event.Package]
		if !packageStarted || !packageState.started {
			return events, counts, fmt.Errorf("event %d action %q for package %s has no preceding start", eventNumber, event.Action, event.Package)
		}
		if packageState.terminal {
			return events, counts, fmt.Errorf("event %d occurs after package %s terminal event", eventNumber, event.Package)
		}
		if event.Test != "" {
			key := testKey{packageName: event.Package, testName: event.Test}
			if strings.TrimSpace(event.Test) != event.Test {
				return events, counts, fmt.Errorf("event %d action %q has invalid test name %q", eventNumber, event.Action, event.Test)
			}
			switch event.Action {
			case "run":
				state := tests[key]
				if state.started && !state.terminal {
					return events, counts, fmt.Errorf("event %d repeats run for active test %s", eventNumber, key)
				}
				tests[key] = testLifecycle{started: true}
				counts.Started++
			case "pause", "cont", "pass", "bench", "fail", "skip", "output":
				state, ok := tests[key]
				if !ok || !state.started || state.terminal {
					return events, counts, fmt.Errorf("event %d action %q for test %s has no active run", eventNumber, event.Action, key)
				}
				switch event.Action {
				case "pause":
					if state.paused {
						return events, counts, fmt.Errorf("event %d pauses already paused test %s", eventNumber, key)
					}
					state.paused = true
				case "cont":
					if !state.paused {
						return events, counts, fmt.Errorf("event %d continues test %s that was not paused", eventNumber, key)
					}
					state.paused = false
				case "pass", "bench", "fail", "skip":
					if state.paused {
						return events, counts, fmt.Errorf("event %d terminates paused test %s", eventNumber, key)
					}
					state.terminal = true
					if event.Action == "pass" {
						counts.Passed++
					} else if event.Action == "bench" {
						counts.Benchmarked++
					} else if event.Action == "fail" {
						counts.Failed++
						addFailure("test " + key.String() + " failed")
					} else {
						counts.Skipped++
					}
				}
				tests[key] = state
			default:
				return events, counts, fmt.Errorf("event %d has unknown action %q", eventNumber, event.Action)
			}
			continue
		}
		switch event.Action {
		case "fail":
			counts.PackageFailed++
			addFailure("package " + event.Package + " failed")
			packageState.terminal = true
		case "skip", "pass":
			packageState.terminal = true
		case "output":
		case "run":
			return events, counts, fmt.Errorf("event %d action run requires a test name", eventNumber)
		case "pause", "cont", "bench":
			return events, counts, fmt.Errorf("event %d action %q requires a test name", eventNumber, event.Action)
		default:
			return events, counts, fmt.Errorf("event %d has unknown action %q", eventNumber, event.Action)
		}
		packageStates[event.Package] = packageState
	}
	counts.Packages = len(packages)
	for packageName, state := range packageStates {
		if state.started && !state.terminal {
			counts.UnfinishedPackages = append(counts.UnfinishedPackages, packageName)
		}
	}
	for key, state := range tests {
		if state.started && !state.terminal {
			counts.Unfinished = append(counts.Unfinished, key.String())
		}
	}
	for failure := range failures {
		counts.FailedDetails = append(counts.FailedDetails, failure)
	}
	sort.Strings(counts.Unfinished)
	sort.Strings(counts.UnfinishedPackages)
	sort.Strings(counts.FailedDetails)
	return events, counts, nil
}

type testKey struct {
	packageName string
	testName    string
}

func (key testKey) String() string { return key.packageName + "." + key.testName }

type testLifecycle struct {
	started  bool
	paused   bool
	terminal bool
}

type packageLifecycle struct {
	started  bool
	terminal bool
}

func writeTestOutput(writer io.Writer, events []testEvent) {
	for _, event := range events {
		if event.Action == "output" && event.Output != "" {
			_, _ = io.WriteString(writer, event.Output) //nolint:errcheck // test output is best-effort
		}
	}
}

func writeDiagnostic(writer io.Writer, label string, data []byte) {
	if len(data) == 0 {
		return
	}
	_, _ = fmt.Fprintf(writer, "%s:\n%s", label, data) //nolint:errcheck // CLI diagnostic is best-effort
	if data[len(data)-1] != '\n' {
		_, _ = io.WriteString(writer, "\n") //nolint:errcheck // CLI diagnostic is best-effort
	}
}
