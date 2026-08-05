package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
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
	rootURI string
	mu      sync.Mutex
	diags   map[string][]Diagnostic
}

var _ LSPClient = (*DefaultLSPClient)(nil)

// NewDefaultLSPClient starts the LSP server subprocess and returns a client.
func NewDefaultLSPClient(ctx context.Context, command []string, workspaceRoot string) (*DefaultLSPClient, error) {
	return nil, fmt.Errorf("lsp: not implemented")
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

// handleNotification processes server-side notifications.
func (c *DefaultLSPClient) handleNotification(_ string, _ json.RawMessage) {}

// Initialize sends the LSP initialize request and initialized notification.
func (c *DefaultLSPClient) Initialize(ctx context.Context, rootURI string) error {
	return fmt.Errorf("lsp: not implemented")
}

// Definition returns definition locations for the symbol at the given position.
func (c *DefaultLSPClient) Definition(ctx context.Context, uri string, line, character int) ([]Location, error) {
	return nil, fmt.Errorf("lsp: not implemented")
}

// References returns reference locations for the symbol at the given position.
func (c *DefaultLSPClient) References(ctx context.Context, uri string, line, character int) ([]Location, error) {
	return nil, fmt.Errorf("lsp: not implemented")
}

// Hover returns hover information for the symbol at the given position.
func (c *DefaultLSPClient) Hover(ctx context.Context, uri string, line, character int) (string, error) {
	return "", fmt.Errorf("lsp: not implemented")
}

// Diagnostics returns cached diagnostics for the given document URI.
func (c *DefaultLSPClient) Diagnostics(ctx context.Context, uri string) ([]Diagnostic, error) {
	return nil, fmt.Errorf("lsp: not implemented")
}

// Shutdown sends the LSP shutdown request and exit notification.
func (c *DefaultLSPClient) Shutdown(ctx context.Context) error {
	return fmt.Errorf("lsp: not implemented")
}
