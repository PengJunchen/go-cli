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
// with a CoreHandler, and serves until interrupted.
func (c *serveCmd) Run(ctx context.Context, cfg Config, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(c.out)

	var addr string
	fs.StringVar(&addr, "addr", ":9090", "listen address for the ACP HTTP server")
	if err := fs.Parse(args); err != nil {
		return newUsageError("serve: %v", err)
	}

	// The CoreHandler is nil for now — the server runs in echo/ack mode.
	// Agent dispatch can be wired in a future enhancement by assembling an
	// agent and passing its SubagentDispatcher to NewCoreHandler.
	handler := acp.NewCoreHandler(nil, nil)
	server := acp.NewHTTPServer("acp-server", addr, handler)

	if err := server.Start(ctx); err != nil {
		return newExecutionError("serve: start server", err)
	}

	fmt.Fprintf(c.out, "ACP HTTP server listening on %s\n", addr)
	fmt.Fprintf(c.out, "Routes:\n")
	fmt.Fprintf(c.out, "  POST /connect     - establish a session\n")
	fmt.Fprintf(c.out, "  POST /send        - deliver an ACP message\n")
	fmt.Fprintf(c.out, "  POST /disconnect  - tear down a session\n")
	fmt.Fprintf(c.out, "  GET  /stream      - drain pending response messages\n")
	fmt.Fprintf(c.out, "\nPress Ctrl+C to stop.\n")

	// Wait for context cancellation or interrupt signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
	case <-sigCh:
	}

	// Use a fresh context for shutdown so in-flight requests can complete
	// even if the parent context was canceled.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Stop(shutdownCtx); err != nil {
		slog.Warn("serve: server stop error", "err", err)
	}
	fmt.Fprintf(c.out, "ACP HTTP server stopped.\n")
	return nil
}

var _ Command = (*serveCmd)(nil)
