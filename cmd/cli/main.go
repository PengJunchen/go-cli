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
	"sync"
	"syscall"
	"time"

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
	return runWithInvocationWaitBudget(argv, stdout, stderr, loadConfig, defaultInvocationProducerWaitBudget)
}

const defaultInvocationProducerWaitBudget = 20 * time.Second

func runWithInvocationWaitBudget(
	argv []string,
	stdout, stderr io.Writer,
	loadConfig func() (*config.Config, error),
	producerWaitBudget time.Duration,
) int {
	previousLogger := slog.Default()
	logHandler := previousLogger.Handler()
	var logFile *os.File
	var invocationLogger *slog.Logger
	ownershipTransferred := false
	defer func() {
		if ownershipTransferred {
			return
		}
		if logFile != nil {
			_ = logFile.Close()
		}
		if slog.Default() == invocationLogger {
			slog.SetDefault(previousLogger)
		}
	}()

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
	invocationLogger = slog.New(tracing.NewRedactingSlogHandler(logHandler))
	slog.SetDefault(invocationLogger)

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

	result := runCLI(tctx, cfg, argv, stdout, stderr, root)
	producerDone := closedInvocationBarrier()
	if result.err != nil && errors.Is(result.err, cli.ErrAssemblyClosePending) {
		channels, valid := cli.AssemblyCloseCompletions(result.err)
		producerDone = invocationProducerBarrier(channels, valid)
	}
	owner := newInvocationCloseOwner(producerDone, invocationFinalizer{
		root:               root,
		tracer:             tracer,
		exporter:           exporter,
		stderr:             stderr,
		logFile:            logFile,
		previousLogger:     previousLogger,
		invocationLogger:   invocationLogger,
		exitCode:           result.exitCode,
		exporterWaitBudget: tracing.DefaultAsyncExporterShutdownWaitBudget,
	})
	ownershipTransferred = true
	code, completed := waitForInvocationClose(owner, producerWaitBudget, stderr)
	if result.err != nil && errors.Is(result.err, cli.ErrAssemblyClosePending) && completed {
		_, _ = fmt.Fprintln(stderr, "error: invocation close completed after the command reported Assembly close pending") //nolint:errcheck // CLI error output is best-effort
	}
	return code
}

type tracingShutdownReporter interface {
	ShutdownWithReport(context.Context) (tracing.AsyncExporterReport, error)
}

// finalizeTracingExporter keeps the root process alive for the bounded owner
// close even when the signal context has already been canceled. It emits only
// safe status and numeric counts; callback error text can contain span data.
func finalizeTracingExporter(ctx context.Context, exporter tracing.TraceExporter, stderr io.Writer, waitBudget time.Duration) int {
	_, _, code := finalizeTracingExporterWithReport(ctx, exporter, stderr, waitBudget)
	return code
}

func finalizeTracingExporterWithReport(
	ctx context.Context,
	exporter tracing.TraceExporter,
	stderr io.Writer,
	waitBudget time.Duration,
) (report tracing.AsyncExporterReport, ownerFinished bool, code int) {
	if exporter == nil {
		return tracing.AsyncExporterReport{OwnerFinished: true}, true, 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if waitBudget <= 0 {
		waitBudget = tracing.DefaultAsyncExporterShutdownWaitBudget
	}
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), waitBudget)
	defer cancel()

	reporter, ok := exporter.(tracingShutdownReporter)
	if !ok {
		_, _ = fmt.Fprintln(stderr, "error: tracing exporter shutdown unavailable (missing bounded owner)") //nolint:errcheck // CLI error output is best-effort
		return tracing.AsyncExporterReport{}, false, 1
	}
	report, err := reporter.ShutdownWithReport(waitCtx)
	if err == nil && report.OwnerFinished {
		return report, true, 0
	}
	state := "failed"
	if errors.Is(err, tracing.ErrShutdownPending) || !report.OwnerFinished {
		state = "pending"
	}
	_, _ = fmt.Fprintf(stderr, "error: tracing exporter shutdown %s (accepted=%d delivered=%d pending=%d)\n", state, report.Stats.Accepted, report.Stats.Delivered, report.Stats.Pending) //nolint:errcheck // CLI error output is best-effort
	return report, report.OwnerFinished, 1
}

type invocationFinalizer struct {
	root               tracing.TraceSpan
	tracer             *tracing.Tracer
	exporter           tracing.TraceExporter
	stderr             io.Writer
	logFile            io.Closer
	previousLogger     *slog.Logger
	invocationLogger   *slog.Logger
	exitCode           int
	exporterWaitBudget time.Duration
}

type invocationCloseOwner struct {
	producerDone <-chan struct{}
	finalizer    invocationFinalizer
	startOnce    sync.Once
	initialDone  chan struct{}
	done         chan struct{}

	mu              sync.Mutex
	exitCode        int
	exporterPending bool
}

func newInvocationCloseOwner(producerDone <-chan struct{}, finalizer invocationFinalizer) *invocationCloseOwner {
	return &invocationCloseOwner{
		producerDone: producerDone,
		finalizer:    finalizer,
		initialDone:  make(chan struct{}),
		done:         make(chan struct{}),
		exitCode:     finalizer.exitCode,
	}
}

func (o *invocationCloseOwner) Done() <-chan struct{} {
	if o == nil {
		return nil
	}
	return o.done
}

func (o *invocationCloseOwner) start() {
	if o == nil {
		return
	}
	o.startOnce.Do(func() {
		go o.run()
	})
}

func (o *invocationCloseOwner) run() {
	// A nil barrier means that Assembly completion could not be proven. Keep
	// the invocation resources owned until the process exits or evidence arrives.
	if o.producerDone == nil {
		select {}
	}
	<-o.producerDone

	exporterFinished, finalizerCode := o.finalizer.finalize()
	o.mu.Lock()
	if finalizerCode != 0 && o.exitCode == 0 {
		o.exitCode = finalizerCode
	}
	if !exporterFinished && o.exitCode == 0 {
		o.exitCode = 1
	}
	o.exporterPending = !exporterFinished
	o.mu.Unlock()
	close(o.initialDone)

	if !exporterFinished {
		// The bounded caller result is not callback completion. Rejoin the same
		// shared AsyncExporter owner without a caller deadline; only its positive
		// OwnerFinished report permits the logger/file teardown below.
		reporter, ok := o.finalizer.exporter.(tracingShutdownReporter)
		if !ok {
			return
		}
		backgroundReport, err := reporter.ShutdownWithReport(context.Background())
		if !backgroundReport.OwnerFinished {
			return
		}
		if err != nil {
			o.mu.Lock()
			if o.exitCode == 0 {
				o.exitCode = 1
			}
			o.mu.Unlock()
		}
	}

	o.finalizer.closeLogAndRestoreLogger()
	close(o.done)
}

func (f invocationFinalizer) finalize() (bool, int) {
	if f.root != nil {
		f.root.SetAttributes(tracing.Attribute{Key: "exit_code", Value: f.exitCode})
		if f.exitCode != 0 {
			f.root.SetStatus(tracing.SpanStatusError, fmt.Sprintf("exit code %d", f.exitCode))
		} else {
			f.root.SetStatus(tracing.SpanStatusOK, "")
		}
		f.root.End()
	}
	if f.tracer != nil {
		// Tracer.Flush is retained for compatibility and is currently a no-op.
		f.tracer.Flush()
	}
	_, ownerFinished, code := finalizeTracingExporterWithReport(context.Background(), f.exporter, f.stderr, f.exporterWaitBudget)
	return ownerFinished, code
}

func (f invocationFinalizer) closeLogAndRestoreLogger() {
	if f.logFile != nil {
		_ = f.logFile.Close()
	}
	if slog.Default() == f.invocationLogger {
		slog.SetDefault(f.previousLogger)
	}
}

func waitForInvocationClose(owner *invocationCloseOwner, producerWaitBudget time.Duration, stderr io.Writer) (int, bool) {
	if owner == nil {
		return 1, false
	}
	owner.start()
	if !waitForProducerBarrier(owner.producerDone, producerWaitBudget) {
		_, _ = fmt.Fprintln(stderr, "error: invocation shutdown remains pending; trace and log finalization was deferred") //nolint:errcheck // CLI error output is best-effort
		return 1, false
	}
	<-owner.initialDone

	owner.mu.Lock()
	code := owner.exitCode
	exporterPending := owner.exporterPending
	owner.mu.Unlock()
	if exporterPending {
		if code == 0 {
			code = 1
		}
		return code, false
	}
	<-owner.done
	owner.mu.Lock()
	code = owner.exitCode
	owner.mu.Unlock()
	return code, true
}

func waitForProducerBarrier(done <-chan struct{}, waitBudget time.Duration) bool {
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
	}
	if waitBudget <= 0 {
		return false
	}
	timer := time.NewTimer(waitBudget)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
}

func closedInvocationBarrier() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func invocationProducerBarrier(channels []<-chan struct{}, valid bool) <-chan struct{} {
	if !valid {
		return nil
	}
	if len(channels) == 0 {
		return closedInvocationBarrier()
	}
	for _, channel := range channels {
		if channel == nil {
			return nil
		}
	}
	done := make(chan struct{})
	go func() {
		for _, channel := range channels {
			<-channel
		}
		close(done)
	}()
	return done
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

type cliRunResult struct {
	exitCode int
	err      error
}

// runCLI invokes the CLI framework and preserves its error chain so run can
// join a real Assembly-close owner before ending the invocation span.
func runCLI(ctx context.Context, cfg *config.Config, argv []string, stdout, stderr io.Writer, root tracing.TraceSpan) cliRunResult {
	if err := cli.Run(ctx, cfg, argv, stdout); err != nil {
		if errors.Is(err, cli.ErrAssemblyClosePending) {
			return cliRunResult{exitCode: 1, err: err}
		}
		var usageErr *cli.UsageError
		if errors.As(err, &usageErr) {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err) //nolint:errcheck // CLI error output is best-effort
			return cliRunResult{exitCode: 2, err: err}
		}
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err) //nolint:errcheck // CLI error output is best-effort
		return cliRunResult{exitCode: 1, err: err}
	}
	return cliRunResult{}
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
