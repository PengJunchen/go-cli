// verify-scanner-gate runs the scanner as a subprocess and validates its JSON
// output before returning success. Keeping this boundary in a small command
// makes process errors and malformed reports visible to local and CI callers.
package main

import (
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

	"github.com/pengjunchen/go-cli/internal/verify"
	"github.com/pengjunchen/go-cli/internal/verify/cmd/gatecapture"
)

const maxDiagnosticBytes = 4096

var defaultCaptureLimits = gatecapture.Limits{
	StdoutBytes: 32 << 20,
	StderrBytes: 8 << 20,
	WaitDelay:   gatecapture.DefaultWaitDelay,
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, " ") }

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithCaptureLimits(args, stdout, stderr, defaultCaptureLimits)
}

func runWithCaptureLimits(args []string, stdout, stderr io.Writer, limits gatecapture.Limits) int {
	fs := flag.NewFlagSet("verify-scanner-gate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "./internal", "directory passed to the scanner")
	command := fs.String("scanner-command", "", "scanner executable")
	timeout := fs.Duration("timeout", 2*time.Minute, "maximum scanner runtime")
	var scannerArgs stringList
	fs.Var(&scannerArgs, "scanner-arg", "one scanner argument; may be repeated")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		_, _ = fmt.Fprintf(stderr, "unexpected arguments: %s\n", strings.Join(fs.Args(), " ")) //nolint:errcheck // CLI diagnostic is best-effort
		return 2
	}
	if *command == "" {
		_, _ = fmt.Fprintln(stderr, "scanner-command is required") //nolint:errcheck // CLI diagnostic is best-effort
		return 2
	}
	if *timeout <= 0 {
		_, _ = fmt.Fprintln(stderr, "timeout must be positive") //nolint:errcheck // CLI diagnostic is best-effort
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, *command, scannerArgs...)
	capture, err := gatecapture.Run(cmd, limits)
	writeProcessCleanupDiagnostic(stderr, err)
	if capture.StdoutExceeded {
		_, _ = fmt.Fprintf(stderr, "scanner stdout exceeded capture limit of %d bytes; report is incomplete\n", limits.StdoutBytes) //nolint:errcheck // CLI diagnostic is best-effort
	}
	if capture.StderrExceeded {
		_, _ = fmt.Fprintf(stderr, "scanner stderr exceeded capture limit of %d bytes\n", limits.StderrBytes) //nolint:errcheck // CLI diagnostic is best-effort
	}
	if capture.StdoutExceeded || capture.StderrExceeded {
		writeDiagnostic(stderr, "scanner stderr", capture.Stderr)
		writeDiagnostic(stderr, "scanner stdout", capture.Stdout)
		return 1
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_, _ = fmt.Fprintf(stderr, "scanner command timed out after %s: %s\n", *timeout, *command) //nolint:errcheck // CLI diagnostic is best-effort
		} else {
			_, _ = fmt.Fprintf(stderr, "scanner command failed: %s: %v\n", *command, err) //nolint:errcheck // CLI diagnostic is best-effort
		}
		writeDiagnostic(stderr, "scanner stderr", capture.Stderr)
		writeDiagnostic(stderr, "scanner stdout", capture.Stdout)
		return 1
	}

	report, err := decodeReport(capture.Stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scanner produced invalid JSON: %v\n", err) //nolint:errcheck // CLI diagnostic is best-effort
		writeDiagnostic(stderr, "scanner stderr", capture.Stderr)
		writeDiagnostic(stderr, "scanner stdout", capture.Stdout)
		return 1
	}

	if report.HasErrors() {
		_, _ = fmt.Fprintf(stdout, "%s\n", report.FormatText())                                                 //nolint:errcheck // CLI output is best-effort
		_, _ = fmt.Fprintf(stderr, "scanner reported %d ERROR finding(s) in %s\n", report.Summary.Errors, *dir) //nolint:errcheck // CLI diagnostic is best-effort
		return 1
	}

	_, _ = fmt.Fprintf(stdout, "%s\n", report.FormatText()) //nolint:errcheck // CLI output is best-effort
	return 0
}

func writeProcessCleanupDiagnostic(writer io.Writer, err error) {
	switch {
	case errors.Is(err, gatecapture.ErrProcessCleanupUnconfirmed):
		_, _ = fmt.Fprintln(writer, "scanner process cleanup could not be confirmed; gate result is failed") //nolint:errcheck // CLI diagnostic is best-effort
	case errors.Is(err, gatecapture.ErrResidualDescendants):
		_, _ = fmt.Fprintln(writer, "scanner left descendants in its owned process group; gate result is failed") //nolint:errcheck // CLI diagnostic is best-effort
	case errors.Is(err, gatecapture.ErrProcessGroupUnsupported):
		_, _ = fmt.Fprintln(writer, "scanner cannot run safely because owned process groups are unsupported on this operating system") //nolint:errcheck // CLI diagnostic is best-effort
	case errors.Is(err, gatecapture.ErrForeignProcessGroup), errors.Is(err, gatecapture.ErrProcessGroupConfiguration), errors.Is(err, gatecapture.ErrProcessAlreadyStarted):
		_, _ = fmt.Fprintln(writer, "scanner command has incompatible process-group ownership settings") //nolint:errcheck // CLI diagnostic is best-effort
	}
}

func decodeReport(data []byte) (*verify.ScanReport, error) {
	var shape struct {
		Dir      *string             `json:"dir"`
		Findings *[]verify.Finding   `json:"findings"`
		Summary  *verify.ScanSummary `json:"summary"`
	}
	if err := json.Unmarshal(data, &shape); err != nil {
		return nil, fmt.Errorf("decode scanner report: %w", err)
	}
	if shape.Dir == nil || strings.TrimSpace(*shape.Dir) == "" {
		return nil, errors.New("missing or empty dir field")
	}
	if shape.Findings == nil {
		return nil, errors.New("missing or null findings field")
	}
	if shape.Summary == nil {
		return nil, errors.New("missing or null summary field")
	}

	report := &verify.ScanReport{Dir: *shape.Dir, Findings: *shape.Findings, Summary: *shape.Summary}
	if report.Summary.TotalFindings != len(report.Findings) {
		return nil, fmt.Errorf("summary total_findings=%d does not match %d findings", report.Summary.TotalFindings, len(report.Findings))
	}
	var errorsCount, warningsCount, infosCount int
	for _, finding := range report.Findings {
		switch finding.Severity {
		case verify.SeverityError:
			errorsCount++
		case verify.SeverityWarn:
			warningsCount++
		case verify.SeverityInfo:
			infosCount++
		default:
			return nil, fmt.Errorf("finding %s has unknown severity %q", finding.RuleID, finding.Severity)
		}
	}
	if report.Summary.Errors != errorsCount || report.Summary.Warnings != warningsCount || report.Summary.Infos != infosCount {
		return nil, fmt.Errorf("summary severity counts do not match findings: summary ERROR/WARN/INFO=%d/%d/%d, observed=%d/%d/%d", report.Summary.Errors, report.Summary.Warnings, report.Summary.Infos, errorsCount, warningsCount, infosCount)
	}
	return report, nil
}

func writeDiagnostic(w io.Writer, label string, data []byte) {
	if len(data) == 0 {
		return
	}
	if len(data) > maxDiagnosticBytes {
		data = append(append([]byte(nil), data[:maxDiagnosticBytes]...), []byte("...<truncated>")...)
	}
	_, _ = fmt.Fprintf(w, "%s:\n%s\n", label, data) //nolint:errcheck // CLI diagnostic is best-effort
}
