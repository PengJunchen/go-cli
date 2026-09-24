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
	"strings"
	"time"
)

const defaultTestTimeout = 15 * time.Minute

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
	Packages int
	Started  int
	Passed   int
	Failed   int
	Skipped  int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
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
	commandArgs := append([]string{"test", "-json"}, goArgs...)
	commandArgs = append(commandArgs, packages...)
	cmd := exec.CommandContext(ctx, *goCommand, commandArgs...)
	cmd.Dir = *dir
	var childStdout, childStderr bytes.Buffer
	cmd.Stdout = &childStdout
	cmd.Stderr = &childStderr
	err := cmd.Run()

	events, counts, decodeErr := parseTestEvents(childStdout.Bytes())
	if decodeErr == nil {
		writeTestOutput(stdout, events)
	} else {
		writeDiagnostic(stderr, "go test stdout", childStdout.Bytes())
	}
	writeDiagnostic(stderr, "go test stderr", childStderr.Bytes())

	if decodeErr != nil {
		_, _ = fmt.Fprintf(stderr, "go test emitted invalid JSON events: %v\n", decodeErr) //nolint:errcheck // CLI diagnostic is best-effort
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
	if counts.Started == 0 {
		_, _ = fmt.Fprintf(stderr, "go test matched zero test functions across %d package(s); command: %s %s\n", counts.Packages, *goCommand, strings.Join(commandArgs, " ")) //nolint:errcheck // CLI diagnostic is best-effort
		return 1
	}
	if counts.Passed == 0 {
		_, _ = fmt.Fprintf(stderr, "go test ran %d test function(s) but none passed (failed=%d skipped=%d packages=%d)\n", counts.Started, counts.Failed, counts.Skipped, counts.Packages) //nolint:errcheck // CLI diagnostic is best-effort
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "go test gate passed: %d test function(s) started, %d passed, %d failed, %d skipped across %d package(s)\n", counts.Started, counts.Passed, counts.Failed, counts.Skipped, counts.Packages) //nolint:errcheck // CLI output is best-effort
	return 0
}

func parseTestEvents(data []byte) ([]testEvent, testCounts, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var events []testEvent
	packages := make(map[string]struct{})
	counts := testCounts{}
	for {
		var event testEvent
		if err := decoder.Decode(&event); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return events, counts, fmt.Errorf("decode event: %w", err)
		}
		events = append(events, event)
		if event.Package != "" {
			packages[event.Package] = struct{}{}
		}
		if event.Test == "" {
			continue
		}
		switch event.Action {
		case "run":
			counts.Started++
		case "pass":
			counts.Passed++
		case "fail":
			counts.Failed++
		case "skip":
			counts.Skipped++
		}
	}
	counts.Packages = len(packages)
	return events, counts, nil
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
