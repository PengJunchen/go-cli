package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/privatefile"
)

// serveCmd implements Command and starts an ACP HTTP server.
type serveAssemblyFactory func(context.Context, *config.Config, string, string, io.Writer, ...AssembleOption) (*AgentAssembly, error)

type serveCmd struct {
	out             io.Writer
	errOut          io.Writer
	assemblyFactory serveAssemblyFactory
}

// ErrServeRemoteApprovalUnsupported indicates that remote approval is not
// available through the current ACP HTTP profile.
var ErrServeRemoteApprovalUnsupported = errors.New("serve: remote approval is unsupported")

// UnsupportedServeApprovalError reports a serve approval mode that requires a
// remote approval round-trip which this CLI profile does not provide.
type UnsupportedServeApprovalError struct{}

// Error implements error.
func (UnsupportedServeApprovalError) Error() string {
	return "serve: --approve ask is unsupported for remote clients; use --approve auto or --approve deny"
}

// Unwrap identifies the unsupported serve capability.
func (UnsupportedServeApprovalError) Unwrap() error { return ErrServeRemoteApprovalUnsupported }

// ErrServeRuntimeUnavailable indicates that serve cannot dispatch without a
// successfully assembled agent runtime.
var ErrServeRuntimeUnavailable = errors.New("serve: a configured agent runtime is required")

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

// Run implements Command. It parses the --addr flag, assembles a full agent
// runtime, creates an HTTPServer with a CoreHandler, and serves until
// interrupted.
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
		tlsCert      string
		tlsKey       string
	)
	fs.StringVar(&addr, "addr", "127.0.0.1:9090", "listen address for the ACP HTTP server")
	fs.StringVar(&modelFlag, "model", "", "model name to use")
	fs.StringVar(&providerFlag, "provider", "", "provider name to use")
	fs.Var(&approveMode, "approve", "approval mode: auto|deny (ask is unsupported; choose explicitly)")
	fs.StringVar(&tokenFlag, "token", "", "bearer token for HTTP auth (auto-generated if empty)")
	fs.StringVar(&tokenFile, "token-file", "", "path to write the auto-generated auth token (default ~/.config/go-cli/serve-token)")
	fs.BoolVar(&showToken, "show-token", false, "print the auth token to stderr")
	fs.BoolVar(&noAuth, "no-auth", false, "disable bearer-token authentication (insecure)")
	fs.BoolVar(&noSandbox, "no-sandbox", false, "disable bash sandbox enforcement")
	fs.StringVar(&tlsCert, "tls-cert", "", "TLS certificate file path")
	fs.StringVar(&tlsKey, "tls-key", "", "TLS key file path")
	if err := fs.Parse(args); err != nil {
		return newUsageError("serve: %v", err)
	}
	if approveMode == ApproveAsk {
		return &UnsupportedServeApprovalError{}
	}
	// --approve auto is dangerous: it automatically approves all tool
	// calls. Refuse to start if auth is disabled, since that would
	// allow unauthenticated remote RCE.
	if approveMode == ApproveAuto && noAuth {
		return newUsageError("serve: --approve auto requires authentication; cannot use with --no-auth")
	}

	// --- Non-loopback security checks (SEC-17) ---
	// When the server binds to a non-loopback address (e.g. 0.0.0.0),
	// it is reachable from the network. Without TLS the traffic is
	// unencrypted; without auth any client can execute tool calls.
	// We warn about the former and reject the latter.
	loopback := acp.IsLoopbackAddr(addr)
	if !loopback {
		if noAuth {
			return newUsageError("serve: --no-auth is not allowed with a non-loopback address %q (use --addr 127.0.0.1:9090 or enable auth)", addr)
		}
		if tlsCert == "" || tlsKey == "" {
			fmt.Fprintf(c.errWriter(), "WARNING: Serving on non-loopback address %s without TLS — traffic is unencrypted.\n", addr) //nolint:errcheck
			fmt.Fprintf(c.errWriter(), "  Consider using --tls-cert and --tls-key to enable HTTPS.\n")                              //nolint:errcheck
		}
	}

	// --- TLS certificate validation ---
	if tlsCert != "" || tlsKey != "" {
		if tlsCert == "" || tlsKey == "" {
			return newUsageError("serve: --tls-cert and --tls-key must both be provided")
		}
		if err := validateTLSFiles(tlsCert, tlsKey); err != nil {
			return newExecutionError("serve: TLS configuration", err)
		}
	}
	// A missing config cannot produce the real dispatcher required by serve.
	// Reject before generating or writing an authentication token.
	var assembly *AgentAssembly
	var rc *config.Config
	if v, ok := cfg.(*config.Config); ok {
		rc = v
	}
	if rc == nil {
		return newExecutionError("serve: runtime unavailable", ErrServeRuntimeUnavailable)
	}
	if rc.Session.StorePath != "" {
		return &UnsupportedDurableCLIError{Feature: "durable ACP sessions"}
	}

	modelName := resolveModelName(modelFlag, rc)
	providerName := resolveProviderName(providerFlag, rc)
	assembleOpts := []AssembleOption{WithApproveMode(approveMode)}
	if noSandbox {
		assembleOpts = append(assembleOpts, WithNoSandbox())
	}
	assemblyFactory := c.assemblyFactory
	if assemblyFactory == nil {
		assemblyFactory = AssembleAgent
	}
	var assembleErr error
	assembly, assembleErr = assemblyFactory(ctx, rc, providerName, modelName, c.out,
		assembleOpts...,
	)
	if assembleErr != nil {
		return errors.Join(
			newExecutionError("serve: assemble agent", assembleErr),
			closeAssemblyForCommand(context.Background(), assembly, "serve"),
		)
	}
	if assembly == nil || assembly.Dispatcher == nil {
		return errors.Join(
			newExecutionError("serve: runtime unavailable", ErrServeRuntimeUnavailable),
			closeAssemblyForCommand(context.Background(), assembly, "serve"),
		)
	}
	handler := acp.NewCoreHandler(assembly.Dispatcher, nil)
	handler.SetEventBus(assembly.EventBus)
	slog.Info("serve: agent dispatch enabled",
		"model", modelName,
		"provider", providerName,
	)

	// Generate and persist an auth token only after the runtime is ready. The
	// token and associated subject are wired into the HTTP server so all routes
	// require a Bearer header and stream routes are bound to the authenticated
	// subject.
	authToken := ""
	authSubject := "cli"
	tokenWrittenTo := ""
	if !noAuth {
		if tokenFlag != "" {
			authToken = tokenFlag
		} else {
			generated, err := generateServeToken()
			if err != nil {
				return errors.Join(
					newExecutionError("serve: generate token", err),
					closeAssemblyForCommand(context.Background(), assembly, "serve"),
				)
			}
			authToken = generated

			// Write the auto-generated token to a file with 0600 permissions
			// so it never leaks to stdout.
			path := tokenFile
			if path == "" {
				path = defaultServeTokenPath()
			}
			tokenWrittenTo = path
		}
	}

	server := acp.NewHTTPServer("acp-server", addr, handler)
	if noAuth {
		server.SetNoAuthLoopbackPolicy()
	}
	if authToken != "" {
		server.SetAuth(authToken, authSubject)
	}
	if tlsCert != "" && tlsKey != "" {
		server.SetTLS(tlsCert, tlsKey)
	}
	if err := assembly.setHTTPServer(server); err != nil {
		return errors.Join(
			newExecutionError("serve: register server owner", err),
			closeAssemblyForCommand(context.Background(), assembly, "serve"),
		)
	}

	if err := server.Start(ctx); err != nil {
		return errors.Join(
			newExecutionError("serve: start server", err),
			closeAssemblyForCommand(context.Background(), assembly, "serve"),
		)
	}
	if tokenWrittenTo != "" {
		if err := writeTokenFile(tokenWrittenTo, authToken); err != nil {
			return errors.Join(
				newExecutionError("serve: write token file", err),
				closeAssemblyForCommand(context.Background(), assembly, "serve"),
			)
		}
	}

	fmt.Fprintf(c.out, "ACP HTTP server listening on %s\n", addr) //nolint:errcheck
	if authToken != "" {
		ew := c.errWriter()
		if tokenWrittenTo != "" {
			fmt.Fprintf(ew, "Auth token written to %s (mode 0600)\n", tokenWrittenTo) //nolint:errcheck
		} else {
			fmt.Fprintf(ew, "Auth token configured via --token\n") //nolint:errcheck
		}
		if showToken {
			fmt.Fprintf(ew, "Auth token: %s\n", authToken)                //nolint:errcheck
			fmt.Fprintf(ew, "Use: Authorization: Bearer %s\n", authToken) //nolint:errcheck
		} else {
			fmt.Fprintf(ew, "Use --show-token to display the token on stderr.\n") //nolint:errcheck
		}
	}
	fmt.Fprintf(c.out, "Routes:\n")                                               //nolint:errcheck
	fmt.Fprintf(c.out, "  POST /connect     - establish a session\n")             //nolint:errcheck
	fmt.Fprintf(c.out, "  POST /send        - deliver an ACP message\n")          //nolint:errcheck
	fmt.Fprintf(c.out, "  POST /disconnect  - tear down a session\n")             //nolint:errcheck
	fmt.Fprintf(c.out, "  GET  /stream      - drain pending response messages\n") //nolint:errcheck
	fmt.Fprintf(c.out, "  GET  /events      - stream agent events (SSE)\n")       //nolint:errcheck
	fmt.Fprintf(c.out, "\nPress Ctrl+C to stop.\n")                               //nolint:errcheck

	// Wait for context cancellation or interrupt signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-ctx.Done():
	case <-sigCh:
	}
	signal.Stop(sigCh)

	closeErr := closeAssemblyForCommand(context.Background(), assembly, "serve")
	if errors.Is(closeErr, ErrAssemblyClosePending) {
		fmt.Fprintf(c.out, "ACP HTTP server shutdown is still pending.\n") //nolint:errcheck
	} else if closeErr != nil {
		slog.Warn("serve: shutdown error", "err", closeErr)
	} else {
		fmt.Fprintf(c.out, "ACP HTTP server stopped.\n") //nolint:errcheck
	}
	return closeErr
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
	return privatefile.WriteAtomic(path, []byte(token))
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

// validateTLSFiles verifies that the TLS certificate and key files exist and
// are readable.
func validateTLSFiles(certFile, keyFile string) error {
	if err := checkReadableFile(certFile, "certificate"); err != nil {
		return err
	}
	if err := checkReadableFile(keyFile, "key"); err != nil {
		return err
	}
	return nil
}

// checkReadableFile verifies that path exists and is readable, returning a
// descriptive error labeled with kind (e.g. "certificate", "key").
func checkReadableFile(path, kind string) error {
	if path == "" {
		return fmt.Errorf("TLS %s file path is empty", kind)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("TLS %s file %q: %w", kind, path, err)
	}
	_ = f.Close()
	return nil
}

var _ Command = (*serveCmd)(nil)
