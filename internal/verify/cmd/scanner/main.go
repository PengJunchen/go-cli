// verify-scanner: AST scanner CLI for mock/hardcoded bypass detection.
// Usage: go run ./internal/verify/cmd/scanner -dir ./internal -format json
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func main() {
	dir := flag.String("dir", ".", "directory to scan")
	format := flag.String("format", "text", "output format: text or json")
	includeTests := flag.Bool("include-tests", false, "include _test.go files in scan")
	flag.Parse()

	cfg := verify.DefaultScanConfig(*dir)
	if *includeTests {
		cfg.ExcludeFiles = []string{}
	}

	report, err := verify.Scan(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scan error: %v\n", err)
		os.Exit(1)
	}

	switch *format {
	case "json":
		data, _ := report.ToJSON() //nolint:errcheck // CLI output, error is non-critical
		fmt.Println(string(data))
	case "text":
		fmt.Println(report.FormatText())
	default:
		fmt.Fprintf(os.Stderr, "unknown format: %s\n", *format)
		os.Exit(1)
	}

	if report.HasErrors() {
		os.Exit(1)
	}
}
