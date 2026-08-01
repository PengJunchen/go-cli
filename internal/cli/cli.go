// Package cli provides the core CLI execution framework.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// Version is the CLI version, set at build time via ldflags.
var Version = "0.1.0"

// Config holds the CLI configuration.
type Config interface {
	// Verbose returns whether verbose output is enabled.
	Verbose() bool
}

// defaultConfig is a minimal Config implementation that keeps verbose off by
// default. Real programs typically supply their own Config (e.g. loaded from
// the environment) and satisfy this interface structurally.
type defaultConfig struct{ verbose bool }

// var _ ensures defaultConfig satisfies Config.
var _ Config = (*defaultConfig)(nil)

// Verbose returns whether verbose output is enabled.
func (c *defaultConfig) Verbose() bool { return c.verbose }

// NewDefaultConfig returns a Config with verbose mode set to the given value.
func NewDefaultConfig(verbose bool) Config { return &defaultConfig{verbose: verbose} }

// UsageError indicates the CLI was invoked with invalid arguments, such as an
// unknown command or a flag parsing failure. Callers should report it with an
// exit code of 2.
type UsageError struct {
	msg string
}

// Error implements error.
func (e *UsageError) Error() string { return e.msg }

// newUsageError creates a UsageError wrapping the given message.
func newUsageError(format string, args ...interface{}) *UsageError {
	return &UsageError{msg: fmt.Sprintf(format, args...)}
}

// ExecutionError indicates a runtime failure while executing a command.
// Callers should report it with an exit code of 1.
type ExecutionError struct {
	msg string
	err error
}

// Error implements error.
func (e *ExecutionError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %v", e.msg, e.err)
	}
	return e.msg
}

// Unwrap returns the underlying error for errors.As / errors.Is use.
func (e *ExecutionError) Unwrap() error { return e.err }

// newExecutionError creates an ExecutionError wrapping err with msg context.
func newExecutionError(msg string, err error) *ExecutionError {
	return &ExecutionError{msg: msg, err: err}
}

// Run executes the CLI with the given configuration and arguments, writing
// output to out. It creates a default registry that RunWithRegistry populates
// with the built-in version and help commands.
func Run(ctx context.Context, cfg Config, args []string, out io.Writer) error {
	reg := NewDefaultCommandRegistry()
	return RunWithRegistry(ctx, cfg, args, out, reg)
}

// RunWithRegistry executes the CLI with the given configuration, arguments,
// output writer, and command registry. It parses top-level flags, resolves the
// requested subcommand through reg, and executes it.
func RunWithRegistry(ctx context.Context, cfg Config, args []string, out io.Writer, reg CommandRegistry) error {
	if err := ctx.Err(); err != nil {
		slog.Default().Warn("context canceled before run", "err", err)
		return err
	}

	fs := flag.NewFlagSet("go-cli", flag.ContinueOnError)
	fs.SetOutput(out)

	var showVersion bool
	fs.BoolVar(&showVersion, "version", false, "print version and exit")

	var printUsage bool
	fs.BoolVar(&printUsage, "help", false, "print usage and exit")

	fs.Usage = func() {
		_, _ = fmt.Fprintf(out, "Usage: go-cli [options] <command> [args]\n\nOptions:\n") //nolint:errcheck // usage output is best-effort
		fs.PrintDefaults()
		_, _ = fmt.Fprintf(out, "\nCommands:\n") //nolint:errcheck // usage output is best-effort
		for _, cmd := range reg.List() {
			_, _ = fmt.Fprintf(out, "  %-10s %s\n", cmd.Name(), cmd.Synopsis()) //nolint:errcheck // usage output is best-effort
		}
	}

	if err := fs.Parse(args); err != nil {
		return newUsageError("flag parse: %v", err)
	}

	if cfg.Verbose() {
		slog.Default().Debug("verbose mode enabled")
	}

	// Ensure the built-in commands are resolvable from the registry. Errors are
	// tolerated so that a caller-supplied registry may provide its own versions.
	if err := reg.Register(newVersionCmd(out)); err != nil {
		slog.Default().Info("built-in version command already registered; keeping caller's", "err", err)
	}
	if err := reg.Register(newHelpCmd(out, fs.Usage)); err != nil {
		slog.Default().Info("built-in help command already registered; keeping caller's", "err", err)
	}
	if err := reg.Register(newPromptCmd(out)); err != nil {
		slog.Default().Info("built-in prompt command already registered; keeping caller's", "err", err)
	}

	if showVersion {
		return runCommand(ctx, cfg, newVersionCmd(out), nil)
	}

	if printUsage {
		fs.Usage()
		return nil
	}

	subArgs := fs.Args()
	if len(subArgs) == 0 {
		fs.Usage()
		return nil
	}

	cmd, ok := reg.Get(subArgs[0])
	if !ok {
		return newUsageError("unknown command: %s", subArgs[0])
	}

	return runCommand(ctx, cfg, cmd, subArgs[1:])
}

// runCommand executes cmd with args, logging start/end telemetry and checking
// for context cancellation before executing.
func runCommand(ctx context.Context, cfg Config, cmd Command, args []string) error {
	name := cmd.Name()
	start := time.Now()
	slog.Default().Debug("command_start", "command", name, "args_len", len(args))

	if err := ctx.Err(); err != nil {
		slog.Default().Warn("context canceled before command", "command", name, "err", err)
		return err
	}

	if err := cmd.Run(ctx, cfg, args); err != nil {
		if err2 := ctx.Err(); err2 != nil {
			slog.Default().Warn("context canceled during command", "command", name, "err", err2)
		}
		slog.Default().Info("command_end", "command", name, "success", false, "duration_ms", time.Since(start).Milliseconds())
		return newExecutionError(name, err)
	}

	slog.Default().Info("command_end", "command", name, "success", true, "duration_ms", time.Since(start).Milliseconds())
	return nil
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
	_, _ = fmt.Fprintf(ow.w, format, args...) //nolint:errcheck // output writing is best-effort
}

// Verbose writes a formatted message only when verbose mode is enabled.
func (ow *OutputWriter) Verbose(format string, args ...interface{}) {
	if ow.verbose {
		_, _ = fmt.Fprintf(ow.w, format, args...) //nolint:errcheck // output writing is best-effort
	}
}
