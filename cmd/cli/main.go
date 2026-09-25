// Command go-cli is the command-line entry point for the go-cli tool.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func main() {
	tm := approval.NewDefaultTrustManager(nil)
	trustCheck := func(ctx context.Context, projectPath string) bool {
		return tm.IsTrusted(ctx, projectPath)
	}
	loadConfig := func() (*config.Config, error) {
		return config.LoadTrusted(trustCheck)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, loadConfig))
}

// run is the testable entry point. It wires signal cancellation, config
// loading, and tracing, then dispatches to cli.Run. It returns the process
// exit code so main can simply os.Exit(run(...)).
//
// argv are the arguments after the program name, stdout/stderr are the
// output streams, and loadConfig is injectable for tests.
func run(argv []string, stdout, stderr io.Writer, loadConfig func() (*config.Config, error)) int {
	previousLogger := slog.Default()
	defer slog.SetDefault(previousLogger)
	logHandler := previousLogger.Handler()
	var logFile *os.File

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Redirect slog output to a log file so the console stays clean for TUI
	// and user interaction. Logs are written to .go-cli/logs/go-cli.log by
	// default. This runs before config loading so even config_load spans are
	// captured in the file.
	logDir := filepath.Join(".go-cli", "logs")
	if err := os.MkdirAll(logDir, 0o750); err == nil {
		var fErr error
		logFile, fErr = os.OpenFile(filepath.Join(logDir, "go-cli.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if fErr == nil {
			logHandler = slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo})
		} else {
			logFile = nil
		}
	}
	slog.SetDefault(slog.New(tracing.NewRedactingSlogHandler(logHandler)))
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}

	cfg, err := loadConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err) //nolint:errcheck // CLI error output is best-effort
		return 1
	}
	if err := stdoutTracingOutputConflict(cfg, argv); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err) //nolint:errcheck // CLI error output is best-effort
		return 2
	}

	tracer, exporter, err := newTracing(cfg, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: tracing: %v\n", err) //nolint:errcheck // CLI error output is best-effort
		return 1
	}
	var root tracing.TraceSpan
	tctx := ctx
	if tracer != nil {
		root, tctx = tracer.Start(ctx, "cli.invocation", tracing.SpanKindInternal)
		cmdName := ""
		if len(argv) > 0 {
			cmdName = argv[0]
		}
		root.SetAttributes(
			tracing.Attribute{Key: "cli_version", Value: cli.Version},
			tracing.Attribute{Key: "args_count", Value: len(argv)},
			tracing.Attribute{Key: "command_name", Value: cmdName},
		)
	}

	code := runCLI(tctx, cfg, argv, stdout, stderr, root)

	if root != nil {
		root.SetAttributes(
			tracing.Attribute{Key: "exit_code", Value: code},
		)
		if code != 0 {
			root.SetStatus(tracing.SpanStatusError, fmt.Sprintf("exit code %d", code))
		} else {
			root.SetStatus(tracing.SpanStatusOK, "")
		}
		root.End()
	}
	if exporter != nil {
		if tracer != nil {
			tracer.Flush()
		}
		if err := exporter.Shutdown(ctx); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: shutdown exporter: %v\n", err) //nolint:errcheck // CLI error output is best-effort
			if code == 0 {
				code = 1
			}
		}
	}
	return code
}

func stdoutTracingOutputConflict(cfg *config.Config, argv []string) error {
	if cfg == nil || cfg.Tracing.Exporter != "stdout" {
		return nil
	}
	enabled := true
	if cfg.Tracing.Enabled != nil {
		enabled = *cfg.Tracing.Enabled
	}
	if !enabled {
		return nil
	}

	promptArgs, isPrompt, err := cli.PromptArgsFromCLI(argv)
	if err != nil || !isPrompt {
		return nil
	}
	mode, remaining, err := cli.ParsePromptOutputMode(promptArgs)
	if err != nil || len(remaining) == 0 {
		return nil
	}
	if mode == cli.OutputJSON || mode == cli.OutputStream {
		return &cli.StdoutTracingOutputConflictError{Mode: mode}
	}
	return nil
}

// runCLI invokes the CLI framework and maps the returned error to an exit code.
func runCLI(ctx context.Context, cfg *config.Config, argv []string, stdout, stderr io.Writer, root tracing.TraceSpan) int {
	if err := cli.Run(ctx, cfg, argv, stdout); err != nil {
		var usageErr *cli.UsageError
		if errors.As(err, &usageErr) {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err) //nolint:errcheck // CLI error output is best-effort
			return 2
		}
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err) //nolint:errcheck // CLI error output is best-effort
		return 1
	}
	return 0
}

// newTracing builds the one invocation-owned tracing runtime using the CLI's
// configured output writer for the explicit stdout sink.
func newTracing(cfg *config.Config, stdout io.Writer) (*tracing.Tracer, tracing.TraceExporter, error) {
	enabled := true
	if cfg.Tracing.Enabled != nil {
		enabled = *cfg.Tracing.Enabled
	}
	return tracing.BuildTracingRuntime(tracing.TracingRuntimeConfig{
		Enabled:        enabled,
		Exporter:       cfg.Tracing.Exporter,
		FilePath:       cfg.Tracing.FilePath,
		RedactionLevel: tracing.RedactionLevel(cfg.Tracing.RedactionLevel),
		Stdout:         stdout,
	})
}
