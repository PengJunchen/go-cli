// verify-scanner: AST scanner CLI for mock/hardcoded bypass detection.
// Usage: go run ./internal/verify/cmd/scanner -dir ./internal -format json
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the scanner CLI with the given arguments and writes output to
// stdout/stderr. It returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify-scanner", flag.ContinueOnError)
	dir := fs.String("dir", ".", "directory to scan")
	format := fs.String("format", "text", "output format: text or json")
	includeTests := fs.Bool("include-tests", false, "include _test.go files in scan")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg := verify.DefaultScanConfig(*dir)
	if *includeTests {
		cfg.ExcludeFiles = []string{}
	}

	report, err := verify.Scan(cfg)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "scan error: %v\n", err) //nolint:errcheck // CLI error output is best-effort
		return 1
	}

	switch *format {
	case "json":
		data, _ := report.ToJSON()                //nolint:errcheck // CLI output, error is non-critical
		_, _ = fmt.Fprintln(stdout, string(data)) //nolint:errcheck // CLI output is best-effort
	case "text":
		_, _ = fmt.Fprintln(stdout, report.FormatText()) //nolint:errcheck // CLI output is best-effort
	default:
		_, _ = fmt.Fprintf(stderr, "unknown format: %s\n", *format) //nolint:errcheck // CLI error output is best-effort
		return 1
	}

	if report.HasErrors() {
		return 1
	}
	return 0
}
