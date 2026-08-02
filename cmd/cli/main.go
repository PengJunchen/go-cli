// Command go-cli is the command-line entry point for the go-cli tool.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, config.Load))
}

// run is the testable entry point. It wires signal cancellation, config
// loading, and tracing, then dispatches to cli.Run. It returns the process
// exit code so main can simply os.Exit(run(...)).
//
// argv are the arguments after the program name, stdout/stderr are the
// output streams, and loadConfig is injectable for tests.
func run(argv []string, stdout, stderr io.Writer, loadConfig func() (*config.Config, error)) int {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := loadConfig()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err) //nolint:errcheck // CLI error output is best-effort
		return 1
	}

	tracer, exporter := newTracing(cfg)
	var root tracing.TraceSpan
	tctx := ctx
	if tracer != nil {
		root, tctx = tracer.Start(ctx, "cli.invocation", tracing.SpanKindInternal)
		root.SetAttributes(
			tracing.Attribute{Key: "cli_version", Value: cli.Version},
			tracing.Attribute{Key: "args_count", Value: len(argv)},
		)
	}

	code := runCLI(tctx, cfg, argv, stdout, stderr, root)

	if root != nil {
		if code != 0 {
			root.SetStatus(tracing.SpanStatusError, fmt.Sprintf("exit code %d", code))
		} else {
			root.SetStatus(tracing.SpanStatusOK, "")
		}
		root.End()
	}
	if exporter != nil {
		// Allow the root span's asynchronous export goroutine to enqueue its
		// data before Shutdown drains the exporter, so the trace file captures
		// the full span chain (cli.invocation → command.dispatch) on exit.
		time.Sleep(50 * time.Millisecond)
		if err := exporter.Shutdown(ctx); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: shutdown exporter: %v\n", err) //nolint:errcheck // CLI error output is best-effort
		}
	}
	return code
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

// newTracing builds a *tracing.Tracer for the given config, or returns
// (nil, exporter) when tracing is disabled/misconfigured. When a nil tracer is
// returned, SpanFromContext yields noop spans and the CLI still runs.
func newTracing(cfg *config.Config) (*tracing.Tracer, tracing.TraceExporter) {
	enabled := true
	if cfg.Tracing.Enabled != nil {
		enabled = *cfg.Tracing.Enabled
	}
	if !enabled || cfg.Tracing.Exporter == "none" {
		return nil, nil
	}

	var exporter tracing.TraceExporter = tracing.NewStdoutTraceExporterWithWriter(false, os.Stdout)
	switch cfg.Tracing.Exporter {
	case "stdout":
		exporter = tracing.NewStdoutTraceExporterWithWriter(false, os.Stdout)
	case "jsonl":
		dir := cfg.Tracing.FilePath
		if dir == "" {
			dir = filepath.Join(".go-cli", "traces")
		}
		e, err := tracing.NewJSONLTraceExporter(dir, "session-"+time.Now().Format("20060102T150405"))
		if err == nil {
			exporter = e
		}
	}

	// Wrap the destination in an AsyncExporter so Shutdown reliably drains all
	// in-flight spans before the underlying file/writer is closed.
	exporter = tracing.NewAsyncExporter(exporter, 1024, 16)

	tracer := tracing.NewTracer("", exporter)
	return tracer, exporter
}
