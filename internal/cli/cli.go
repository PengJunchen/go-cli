// Package cli provides the core CLI execution framework.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
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
// with the built-in commands.
func Run(ctx context.Context, cfg Config, args []string, out io.Writer) error {
	reg := NewDefaultCommandRegistry()
	return RunWithRegistry(ctx, cfg, args, out, reg)
}

type globalFlags struct {
	showVersion bool
	printUsage  bool
}

func newGlobalFlagSet(out io.Writer) (*flag.FlagSet, *globalFlags) {
	fs := flag.NewFlagSet("go-cli", flag.ContinueOnError)
	fs.SetOutput(out)

	options := &globalFlags{}
	fs.BoolVar(&options.showVersion, "version", false, "print version and exit")
	fs.BoolVar(&options.printUsage, "help", false, "print usage and exit")
	return fs, options
}

// IsGlobalHelpOrVersion reports whether the root flag parser accepts args and
// recognizes a global help or version request. Invalid flags and flags after a
// command do not qualify for config-independent dispatch.
func IsGlobalHelpOrVersion(args []string) bool {
	fs, options := newGlobalFlagSet(io.Discard)
	if err := fs.Parse(args); err != nil {
		return false
	}
	return options.showVersion || options.printUsage
}

// RegisterBuiltinCommands adds the built-in commands to reg unless the caller
// has already registered a command with the same name. This lets callers
// initialize one registry before config loading and reuse it for dispatch.
func RegisterBuiltinCommands(reg CommandRegistry, out io.Writer) {
	builtinNames := []string{"version", "help", "prompt", "interactive", "doctor", "serve", "models"}
	allPresent := true
	for _, name := range builtinNames {
		if _, exists := reg.Get(name); !exists {
			allPresent = false
			break
		}
	}
	if allPresent {
		return
	}

	usage := func() { writeUsage(out, reg) }
	builtins := []Command{
		newVersionCmd(out),
		newHelpCmd(out, usage),
		newPromptCmd(out),
		newInteractiveCmd(nil, out),
		newDoctorCmd(out),
		newServeCmd(out),
		newModelsCmd(out),
	}
	for _, cmd := range builtins {
		if _, exists := reg.Get(cmd.Name()); exists {
			slog.Default().Info("built-in command already registered; keeping caller's", "command", cmd.Name())
			continue
		}
		if err := reg.Register(cmd); err != nil {
			slog.Default().Info("built-in command registration failed; keeping caller's", "command", cmd.Name(), "err", err)
		}
	}
}

func writeUsage(out io.Writer, reg CommandRegistry) {
	_, _ = fmt.Fprintf(out, "Usage: go-cli [options] <command> [args]\n\nOptions:\n") //nolint:errcheck // usage output is best-effort
	fs, _ := newGlobalFlagSet(out)
	fs.PrintDefaults()
	_, _ = fmt.Fprintf(out, "\nCommands:\n") //nolint:errcheck // usage output is best-effort
	for _, cmd := range reg.List() {
		_, _ = fmt.Fprintf(out, "  %-10s %s\n", cmd.Name(), cmd.Synopsis()) //nolint:errcheck // usage output is best-effort
	}
}

// PromptArgsFromCLI applies the same global flag parsing and first-positional
// command routing as RunWithRegistry, without executing a command. It reports
// false when a global help/version flag takes precedence or when the selected
// command is not prompt.
func PromptArgsFromCLI(argv []string) ([]string, bool, error) {
	fs, options := newGlobalFlagSet(io.Discard)
	if err := fs.Parse(argv); err != nil {
		return nil, false, newUsageError("flag parse: %v", err)
	}
	if options.showVersion || options.printUsage {
		return nil, false, nil
	}
	args := fs.Args()
	if len(args) == 0 || args[0] != "prompt" {
		return nil, false, nil
	}
	return args[1:], true, nil
}

// RunWithRegistry executes the CLI with the given configuration, arguments,
// output writer, and command registry. It parses top-level flags, resolves the
// requested subcommand through reg, and executes it.
func RunWithRegistry(ctx context.Context, cfg Config, args []string, out io.Writer, reg CommandRegistry) error {
	if err := ctx.Err(); err != nil {
		slog.Default().Warn("context canceled before run", "err", err)
		return err
	}

	RegisterBuiltinCommands(reg, out)
	fs, options := newGlobalFlagSet(out)
	fs.Usage = func() { writeUsage(out, reg) }

	if err := fs.Parse(args); err != nil {
		return newUsageError("flag parse: %v", err)
	}

	if cfg.Verbose() {
		slog.Default().Debug("verbose mode enabled")
	}

	if options.showVersion {
		cmd, _ := reg.Get("version")
		return runCommand(ctx, cfg, cmd, nil)
	}

	if options.printUsage {
		cmd, _ := reg.Get("help")
		return runCommand(ctx, cfg, cmd, nil)
	}

	subArgs := fs.Args()
	if len(subArgs) == 0 {
		// Default to interactive mode when no subcommand is given.
		interactiveCmd, _ := reg.Get("interactive")
		if interactiveCmd != nil {
			return runCommand(ctx, cfg, interactiveCmd, nil)
		}
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
// for context cancellation before executing. It emits a command.dispatch span
// so downstream harness/turn/tool spans are nested under it.
func runCommand(ctx context.Context, cfg Config, cmd Command, args []string) error {
	name := cmd.Name()
	start := time.Now()

	span, spanCtx := tracing.SpanFromContext(ctx, "command.dispatch", tracing.SpanKindInternal)
	span.SetAttributes(
		tracing.Attribute{Key: "command", Value: name},
		tracing.Attribute{Key: "args_count", Value: len(args)},
	)
	slog.Default().Debug("command_start", "command", name, "args_len", len(args))

	if err := ctx.Err(); err != nil {
		slog.Default().Warn("context canceled before command", "command", name, "err", err)
		span.SetStatus(tracing.SpanStatusError, err.Error())
		span.End()
		return err
	}

	if err := cmd.Run(spanCtx, cfg, args); err != nil {
		if err2 := ctx.Err(); err2 != nil {
			slog.Default().Warn("context canceled during command", "command", name, "err", err2)
		}
		slog.Default().Info("command_end", "command", name, "success", false, "duration_ms", time.Since(start).Milliseconds())
		span.SetStatus(tracing.SpanStatusError, err.Error())
		span.End()
		// Usage errors (bad flags, unknown commands) should pass through
		// without a generic hint — they already explain the fix.
		var ue *UsageError
		if !errors.As(err, &ue) {
			if ufe := classifyError(err); ufe != nil {
				return &ExecutionError{
					msg: fmt.Sprintf("%s: %s", name, ufe.Action),
					err: ufe,
				}
			}
		}
		return newExecutionError(name, err)
	}

	slog.Default().Info("command_end", "command", name, "success", true, "duration_ms", time.Since(start).Milliseconds())
	span.SetStatus(tracing.SpanStatusOK, "")
	span.End()
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
