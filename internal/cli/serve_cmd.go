package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
)

// serveCmd implements Command and starts an ACP HTTP server.
type serveCmd struct {
	out io.Writer
}

// newServeCmd creates a serve command writing to out.
func newServeCmd(out io.Writer) *serveCmd {
	return &serveCmd{out: out}
}

// Name implements Command.
func (c *serveCmd) Name() string { return "serve" }

// Synopsis implements Command.
func (c *serveCmd) Synopsis() string { return "Start ACP HTTP server" }

// Run implements Command. It parses the --addr flag, creates an HTTPServer
// with a CoreHandler, and serves until interrupted. When a valid config and
// model are available, the server assembles a full agent runtime and wires
// the SubagentDispatcher and EventBus into the CoreHandler for SSE event
// bridging. When assembly fails (e.g. no config), the server falls back to
// echo/ack mode.
func (c *serveCmd) Run(ctx context.Context, cfg Config, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(c.out)

	var (
		addr        string
		modelFlag   string
		providerFlag string
	)
	fs.StringVar(&addr, "addr", ":9090", "listen address for the ACP HTTP server")
	fs.StringVar(&modelFlag, "model", "", "model name to use")
	fs.StringVar(&providerFlag, "provider", "", "provider name to use")
	if err := fs.Parse(args); err != nil {
		return newUsageError("serve: %v", err)
	}

	// Attempt to assemble a full agent runtime. When successful, the
	// dispatcher and EventBus are wired into the CoreHandler so that
	// /send dispatches to a real agent and /events streams agent events
	// via SSE. When assembly fails (e.g. no config), the server falls
	// back to echo/ack mode with a nil dispatcher.
	var handler *acp.CoreHandler
	var cleanup func()
	var eventBus core.EventBus

	var rc *config.Config
	if v, ok := cfg.(*config.Config); ok {
		rc = v
	}

	if rc != nil {
		modelName := resolveModelName(modelFlag, rc)
		providerName := resolveProviderName(providerFlag, rc)

		assembly, err := AssembleAgent(ctx, rc, providerName, modelName, c.out,
			WithApproveMode(ApproveAuto),
		)
		if err != nil {
			slog.Warn("serve: agent assembly failed, falling back to echo mode", "err", err)
		} else {
			cleanup = assembly.Cleanup
			handler = acp.NewCoreHandler(assembly.Dispatcher, nil)
			handler.SetEventBus(assembly.EventBus)
			eventBus = assembly.EventBus
			slog.Info("serve: agent dispatch enabled",
				"model", modelName,
				"provider", providerName,
			)
		}
	}

	if handler == nil {
		handler = acp.NewCoreHandler(nil, nil)
	}

	server := acp.NewHTTPServer("acp-server", addr, handler)
	if eventBus != nil {
		server.SetEventBus(eventBus)
	}

	if err := server.Start(ctx); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return newExecutionError("serve: start server", err)
	}

	fmt.Fprintf(c.out, "ACP HTTP server listening on %s\n", addr)
	fmt.Fprintf(c.out, "Routes:\n")
	fmt.Fprintf(c.out, "  POST /connect     - establish a session\n")
	fmt.Fprintf(c.out, "  POST /send        - deliver an ACP message\n")
	fmt.Fprintf(c.out, "  POST /disconnect  - tear down a session\n")
	fmt.Fprintf(c.out, "  GET  /stream      - drain pending response messages\n")
	fmt.Fprintf(c.out, "  GET  /events      - stream agent events (SSE)\n")
	fmt.Fprintf(c.out, "\nPress Ctrl+C to stop.\n")

	// Wait for context cancellation or interrupt signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
	case <-sigCh:
	}
	signal.Stop(sigCh)

	// Use a fresh context for shutdown so in-flight requests can complete
	// even if the parent context was canceled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Stop(shutdownCtx); err != nil {
		slog.Warn("serve: server stop error", "err", err)
	}
	if cleanup != nil {
		cleanup()
	}
	fmt.Fprintf(c.out, "ACP HTTP server stopped.\n")
	return nil
}

var _ Command = (*serveCmd)(nil)
