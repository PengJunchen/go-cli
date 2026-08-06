package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

// Position in a text document expressed as zero-based line and character
// offsets, following the LSP convention.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a (half-open) span in a text document.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range inside a specific document identified by URI.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// Diagnostic reported by the language server for a document range.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Message  string `json:"message"`
	Source   string `json:"source"`
}

// LSPClient is the contract for interacting with a Language Server.
type LSPClient interface {
	// Initialize sends the LSP initialize request and the initialized
	// notification.
	Initialize(ctx context.Context, rootURI string) error
	// Definition returns the definition locations for the symbol at the
	// given position.
	Definition(ctx context.Context, uri string, line, character int) ([]Location, error)
	// References returns all reference locations for the symbol at the
	// given position.
	References(ctx context.Context, uri string, line, character int) ([]Location, error)
	// Hover returns the hover information for the symbol at the given
	// position.
	Hover(ctx context.Context, uri string, line, character int) (string, error)
	// Diagnostics returns the cached diagnostics for the given document URI.
	Diagnostics(ctx context.Context, uri string) ([]Diagnostic, error)
	// Shutdown sends the LSP shutdown request and exit notification, then
	// terminates the server subprocess.
	Shutdown(ctx context.Context) error
}

// DefaultLSPClient implements LSPClient by managing an LSP server subprocess
// and communicating via JSON-RPC 2.0 with Content-Length framing.
type DefaultLSPClient struct {
	rpc     *JSONRPCClient
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	rootURI string
	mu      sync.Mutex
	diags   map[string][]Diagnostic
}

var _ LSPClient = (*DefaultLSPClient)(nil)

// NewDefaultLSPClient starts the LSP server subprocess and returns a client.
// The subprocess runs independently of the caller's context; Shutdown
// terminates it.
func NewDefaultLSPClient(_ context.Context, command []string, workspaceRoot string) (*DefaultLSPClient, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("lsp: empty server command")
	}

	// Use a dedicated context so the subprocess is not tied to the caller's
	// context. Shutdown cancels this context to terminate the subprocess.
	lspCtx, cancel := context.WithCancel(context.Background())

	cmd := exec.CommandContext(lspCtx, command[0], command[1:]...) //nolint:gosec // command from config
	if workspaceRoot != "" {
		cmd.Dir = workspaceRoot
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close() //nolint:errcheck
		cancel()
		return nil, fmt.Errorf("lsp: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()  //nolint:errcheck
		_ = stdout.Close() //nolint:errcheck
		cancel()
		return nil, fmt.Errorf("lsp: start server: %w", err)
	}

	rpc := NewJSONRPCClient(stdout, stdin)
	client := &DefaultLSPClient{
		rpc:    rpc,
		cmd:    cmd,
		cancel: cancel,
		diags:  make(map[string][]Diagnostic),
	}
	rpc.SetNotifyHandler(client.handleNotification)

	slog.Info("lsp.server_started", "command", command, "pid", cmd.Process.Pid)
	return client, nil
}

// newDefaultLSPClientWithRPC creates a DefaultLSPClient backed by a
// pre-configured JSONRPCClient (for testing).
func newDefaultLSPClientWithRPC(rpc *JSONRPCClient) *DefaultLSPClient {
	c := &DefaultLSPClient{
		rpc:   rpc,
		diags: make(map[string][]Diagnostic),
	}
	rpc.SetNotifyHandler(c.handleNotification)
	return c
}

// handleNotification processes server-side notifications, caching diagnostics.
func (c *DefaultLSPClient) handleNotification(method string, params json.RawMessage) {
	if method == "textDocument/publishDiagnostics" {
		var notif struct {
			URI         string       `json:"uri"`
			Diagnostics []Diagnostic `json:"diagnostics"`
		}
		if err := json.Unmarshal(params, &notif); err == nil {
			c.mu.Lock()
			c.diags[notif.URI] = notif.Diagnostics
			c.mu.Unlock()
		}
	}
}

// Initialize sends the LSP initialize request and the initialized notification.
func (c *DefaultLSPClient) Initialize(ctx context.Context, rootURI string) error {
	c.rootURI = rootURI

	params := map[string]any{
		"processId":    nil,
		"rootUri":      rootURI,
		"capabilities": map[string]any{},
	}

	var result struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := c.rpc.Call(ctx, "initialize", params, &result); err != nil {
		return fmt.Errorf("lsp: initialize: %w", err)
	}

	if err := c.rpc.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return fmt.Errorf("lsp: initialized notification: %w", err)
	}

	slog.Info("lsp.initialized", "rootUri", rootURI)
	return nil
}

// Definition returns definition locations for the symbol at the given position.
func (c *DefaultLSPClient) Definition(ctx context.Context, uri string, line, character int) ([]Location, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
	}

	var locs []Location
	if err := c.rpc.Call(ctx, "textDocument/definition", params, &locs); err != nil {
		return nil, fmt.Errorf("lsp: definition: %w", err)
	}
	return locs, nil
}

// References returns reference locations for the symbol at the given position.
func (c *DefaultLSPClient) References(ctx context.Context, uri string, line, character int) ([]Location, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
		"context":      map[string]any{"includeDeclaration": true},
	}

	var locs []Location
	if err := c.rpc.Call(ctx, "textDocument/references", params, &locs); err != nil {
		return nil, fmt.Errorf("lsp: references: %w", err)
	}
	return locs, nil
}

// Hover returns hover information for the symbol at the given position.
func (c *DefaultLSPClient) Hover(ctx context.Context, uri string, line, character int) (string, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
	}

	var result struct {
		Contents struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		} `json:"contents"`
	}
	if err := c.rpc.Call(ctx, "textDocument/hover", params, &result); err != nil {
		return "", fmt.Errorf("lsp: hover: %w", err)
	}
	return result.Contents.Value, nil
}

// Diagnostics returns cached diagnostics for the given document URI.
func (c *DefaultLSPClient) Diagnostics(_ context.Context, uri string) ([]Diagnostic, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	diags := c.diags[uri]
	if diags == nil {
		diags = []Diagnostic{}
	}
	return diags, nil
}

// Shutdown sends the LSP shutdown request and exit notification, then
// terminates the server subprocess.
func (c *DefaultLSPClient) Shutdown(ctx context.Context) error {
	// Best-effort shutdown request.
	shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_ = c.rpc.Call(shutdownCtx, "shutdown", nil, nil) //nolint:errcheck
	cancel()

	_ = c.rpc.Notify(ctx, "exit", nil) //nolint:errcheck
	_ = c.rpc.Close()                  //nolint:errcheck

	if c.cancel != nil {
		c.cancel()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill() //nolint:errcheck
		_ = c.cmd.Wait()         //nolint:errcheck
	}

	slog.Info("lsp.server_stopped")
	return nil
}
