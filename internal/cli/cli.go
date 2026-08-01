// Package cli provides the core CLI execution framework.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
)

// Version is the CLI version, set at build time via ldflags.
var Version = "0.1.0"

// Config holds the CLI configuration.
type Config interface {
	// Verbose returns whether verbose output is enabled.
	Verbose() bool
}

// Run executes the CLI with the given configuration and arguments.
func Run(ctx context.Context, cfg Config, args []string) error {
	fs := flag.NewFlagSet("go-cli", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: go-cli [options] <command> [args]\n\n")
		fmt.Fprintf(fs.Output(), "Options:\n")
		fs.PrintDefaults()
		fmt.Fprintf(fs.Output(), "\nCommands:\n")
		fmt.Fprintf(fs.Output(), "  version   Print version\n")
		fmt.Fprintf(fs.Output(), "  help      Print help\n")
	}

	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if showVersion {
		fmt.Fprintf(os.Stdout, "go-cli %s\n", Version)
		return nil
	}

	subArgs := fs.Args()
	if len(subArgs) == 0 {
		fs.Usage()
		return nil
	}

	switch subArgs[0] {
	case "version":
		fmt.Fprintf(os.Stdout, "go-cli %s\n", Version)
		return nil
	case "help":
		fs.Usage()
		return nil
	default:
		return fmt.Errorf("unknown command: %s", subArgs[0])
	}
}

// OutputWriter wraps an io.Writer for structured CLI output.
type OutputWriter struct {
	w       io.Writer
	verbose bool
}

// NewOutputWriter creates a new OutputWriter.
func NewOutputWriter(w io.Writer, verbose bool) *OutputWriter {
	return &OutputWriter{w: w, verbose: verbose}
}

// Print writes a formatted message to the output.
func (ow *OutputWriter) Print(format string, args ...interface{}) {
	fmt.Fprintf(ow.w, format, args...)
}

// Verbose writes a formatted message only when verbose mode is enabled.
func (ow *OutputWriter) Verbose(format string, args ...interface{}) {
	if ow.verbose {
		fmt.Fprintf(ow.w, format, args...)
	}
}
