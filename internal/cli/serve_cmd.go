package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
)

// serveCmd implements Command and starts an ACP HTTP server.
type serveCmd struct {
	out    io.Writer
	errOut io.Writer
}

// newServeCmd creates a serve command writing to out.
func newServeCmd(out io.Writer) *serveCmd {
	return &serveCmd{out: out}
}

// errWriter returns the writer used for token-related output. It defaults
// to os.Stderr so that tokens never leak to stdout.
func (c *serveCmd) errWriter() io.Writer {
	if c.errOut != nil {
		return c.errOut
	}
	return os.Stderr
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
		addr         string
		modelFlag    string
		providerFlag string
		approveMode  ApproveMode
		tokenFlag    string
		tokenFile    string
		showToken    bool
		noAuth       bool
		noSandbox    bool
	)
	fs.StringVar(&addr, "addr", "127.0.0.1:9090", "listen address for the ACP HTTP server")
	fs.StringVar(&modelFlag, "model", "", "model name to use")
	fs.StringVar(&providerFlag, "provider", "", "provider name to use")
	fs.Var(&approveMode, "approve", "approval mode: auto|deny|ask (default ask)")
	fs.StringVar(&tokenFlag, "token", "", "bearer token for HTTP auth (auto-generated if empty)")
	fs.StringVar(&tokenFile, "token-file", "", "path to write the auto-generated auth token (default ~/.config/go-cli/serve-token)")
	fs.BoolVar(&showToken, "show-token", false, "print the auth token to stderr")
	fs.BoolVar(&noAuth, "no-auth", false, "disable bearer-token authentication (insecure)")
	fs.BoolVar(&noSandbox, "no-sandbox", false, "disable bash sandbox enforcement")
	if err := fs.Parse(args); err != nil {
		return newUsageError("serve: %v", err)
	}

	// --approve auto is dangerous: it automatically approves all tool
	// calls. Refuse to start if auth is disabled, since that would
	// allow unauthenticated remote RCE.
	if approveMode == ApproveAuto && noAuth {
		return newUsageError("serve: --approve auto requires authentication; cannot use with --no-auth")
	}

	// Determine the auth token. When auth is enabled (default), a token
	// is either provided via --token or auto-generated. The token and
	// associated subject are wired into the HTTP server so that all
	// routes require a Bearer header and /stream, /events are bound to
	// the authenticated subject.
	authToken := ""
	authSubject := "cli"
	tokenWrittenTo := ""
	if !noAuth {
		if tokenFlag != "" {
			authToken = tokenFlag
		} else {
			generated, err := generateServeToken()
			if err != nil {
				return newExecutionError("serve: generate token", err)
			}
			authToken = generated

			// Write the auto-generated token to a file with 0600
			// permissions so it never leaks to stdout.
			path := tokenFile
			if path == "" {
				path = defaultServeTokenPath()
			}
			if err := writeTokenFile(path, authToken); err != nil {
				return newExecutionError("serve: write token file", err)
			}
			tokenWrittenTo = path
		}
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

		assembleOpts := []AssembleOption{WithApproveMode(approveMode)}
		if noSandbox {
			assembleOpts = append(assembleOpts, WithNoSandbox())
		}
		assembly, err := AssembleAgent(ctx, rc, providerName, modelName, c.out,
			assembleOpts...,
		)
		if err != nil {
			slog.Warn("serve: agent assembly failed, falling back to echo mode", "err", err)
			fmt.Fprintf(c.out, "WARNING: Agent assembly failed — falling back to echo mode (no agent dispatch).\n")
			fmt.Fprintf(c.out, "  Error: %v\n", err)
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
		if rc == nil {
			slog.Warn("serve: no config available, falling back to echo mode")
			fmt.Fprintf(c.out, "WARNING: No configuration found — running in echo mode (no agent dispatch).\n")
		}
		handler = acp.NewCoreHandler(nil, nil)
	}

	server := acp.NewHTTPServer("acp-server", addr, handler)
	if eventBus != nil {
		server.SetEventBus(eventBus)
	}
	if authToken != "" {
		server.SetAuth(authToken, authSubject)
	}

	if err := server.Start(ctx); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return newExecutionError("serve: start server", err)
	}

	fmt.Fprintf(c.out, "ACP HTTP server listening on %s\n", addr)
	if authToken != "" {
		ew := c.errWriter()
		if tokenWrittenTo != "" {
			fmt.Fprintf(ew, "Auth token written to %s (mode 0600)\n", tokenWrittenTo)
		} else {
			fmt.Fprintf(ew, "Auth token configured via --token\n")
		}
		if showToken {
			fmt.Fprintf(ew, "Auth token: %s\n", authToken)
			fmt.Fprintf(ew, "Use: Authorization: Bearer %s\n", authToken)
		} else {
			fmt.Fprintf(ew, "Use --show-token to display the token on stderr.\n")
		}
	}
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

// defaultServeTokenPath returns the default path for the serve token file,
// following the same config directory pattern (~/.config/go-cli/serve-token).
func defaultServeTokenPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".config", "go-cli", "serve-token")
	}
	return filepath.Join(home, ".config", "go-cli", "serve-token")
}

// writeTokenFile writes the token to path with 0600 permissions, creating
// the parent directory (0700) if necessary.
func writeTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token), 0o600)
}

// generateServeToken generates a cryptographically random hex token for
// bearer-token authentication.
func generateServeToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var _ Command = (*serveCmd)(nil)
