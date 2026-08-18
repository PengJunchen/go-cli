package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
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

// SymbolInformation represents a symbol found by a workspace/symbol query.
type SymbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

// Diagnostic reported by the language server for a document range.
type Diagnostic struct {
	Range    Range  `json:"range"`
	Severity int    `json:"severity"`
	Message  string `json:"message"`
	Source   string `json:"source"`
}

// CompletionItem represents a single completion suggestion returned by the
// language server.
type CompletionItem struct {
	Label  string `json:"label"`
	Kind   int    `json:"kind"`
	Detail string `json:"detail"`
}

// TextEdit is a text change applied to a specific range in a document.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// WorkspaceEdit represents a collection of textual edits across multiple
// documents, typically returned by rename or code action requests.
type WorkspaceEdit struct {
	Changes map[string][]TextEdit `json:"changes"`
}

// defaultDiagTTL is the default time-to-live for cached diagnostics.
const defaultDiagTTL = 5 * time.Minute

// diagEntry holds cached diagnostics alongside the time they were received.
type diagEntry struct {
	diagnostics []Diagnostic
	timestamp   time.Time
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
	// DidOpen sends a textDocument/didOpen notification to the server.
	DidOpen(ctx context.Context, uri string, content string, version int) error
	// DidChange sends a textDocument/didChange notification to the server.
	DidChange(ctx context.Context, uri string, content string, version int) error
	// Completion returns completion suggestions at the given position.
	Completion(ctx context.Context, uri string, line, character int) ([]CompletionItem, error)
	// TypeDefinition returns the type definition locations for the symbol
	// at the given position.
	TypeDefinition(ctx context.Context, uri string, line, character int) ([]Location, error)
	// Rename returns the workspace edits for renaming the symbol at the
	// given position.
	Rename(ctx context.Context, uri string, line, character int, newName string) (*WorkspaceEdit, error)
	// WorkspaceSymbol queries the server for symbols matching the query
	// string across the workspace.
	WorkspaceSymbol(ctx context.Context, query string) ([]SymbolInformation, error)
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
	diags   map[string]diagEntry
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
		diags:  make(map[string]diagEntry),
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
		diags: make(map[string]diagEntry),
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
			c.diags[notif.URI] = diagEntry{
				diagnostics: notif.Diagnostics,
				timestamp:   time.Now(),
			}
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
// Entries older than defaultDiagTTL are treated as expired and return empty.
func (c *DefaultLSPClient) Diagnostics(_ context.Context, uri string) ([]Diagnostic, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.diags[uri]
	if !ok || time.Since(entry.timestamp) > defaultDiagTTL {
		return []Diagnostic{}, nil
	}
	return entry.diagnostics, nil
}

// languageIDFromURI derives an LSP languageId from the file extension in
// the URI. Unknown extensions default to "plaintext".
func languageIDFromURI(uri string) string {
	switch {
	case strings.HasSuffix(uri, ".go"):
		return "go"
	case strings.HasSuffix(uri, ".ts"):
		return "typescript"
	case strings.HasSuffix(uri, ".tsx"):
		return "typescriptreact"
	case strings.HasSuffix(uri, ".js"):
		return "javascript"
	case strings.HasSuffix(uri, ".jsx"):
		return "javascriptreact"
	case strings.HasSuffix(uri, ".py"):
		return "python"
	case strings.HasSuffix(uri, ".rs"):
		return "rust"
	case strings.HasSuffix(uri, ".java"):
		return "java"
	case strings.HasSuffix(uri, ".rb"):
		return "ruby"
	case strings.HasSuffix(uri, ".c"):
		return "c"
	case strings.HasSuffix(uri, ".cpp"), strings.HasSuffix(uri, ".cc"):
		return "cpp"
	default:
		return "plaintext"
	}
}

// DidOpen sends a textDocument/didOpen notification so the server tracks
// the document content in memory.
func (c *DefaultLSPClient) DidOpen(ctx context.Context, uri string, content string, version int) error {
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageIDFromURI(uri),
			"version":    version,
			"text":       content,
		},
	}
	if err := c.rpc.Notify(ctx, "textDocument/didOpen", params); err != nil {
		return fmt.Errorf("lsp: didOpen: %w", err)
	}
	return nil
}

// DidChange sends a textDocument/didChange notification with a full-content
// sync (the simplest textDocumentSync mode).
func (c *DefaultLSPClient) DidChange(ctx context.Context, uri string, content string, version int) error {
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":     uri,
			"version": version,
		},
		"contentChanges": []map[string]any{
			{"text": content},
		},
	}
	if err := c.rpc.Notify(ctx, "textDocument/didChange", params); err != nil {
		return fmt.Errorf("lsp: didChange: %w", err)
	}
	return nil
}

// Completion returns completion suggestions at the given position.
func (c *DefaultLSPClient) Completion(ctx context.Context, uri string, line, character int) ([]CompletionItem, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
	}

	var items []CompletionItem
	if err := c.rpc.Call(ctx, "textDocument/completion", params, &items); err != nil {
		return nil, fmt.Errorf("lsp: completion: %w", err)
	}
	return items, nil
}

// TypeDefinition returns the type definition locations for the symbol at
// the given position.
func (c *DefaultLSPClient) TypeDefinition(ctx context.Context, uri string, line, character int) ([]Location, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
	}

	var locs []Location
	if err := c.rpc.Call(ctx, "textDocument/typeDefinition", params, &locs); err != nil {
		return nil, fmt.Errorf("lsp: typeDefinition: %w", err)
	}
	return locs, nil
}

// Rename returns the workspace edits for renaming the symbol at the given
// position to newName.
func (c *DefaultLSPClient) Rename(ctx context.Context, uri string, line, character int, newName string) (*WorkspaceEdit, error) {
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": character},
		"newName":      newName,
	}

	var edit WorkspaceEdit
	if err := c.rpc.Call(ctx, "textDocument/rename", params, &edit); err != nil {
		return nil, fmt.Errorf("lsp: rename: %w", err)
	}
	return &edit, nil
}

// WorkspaceSymbol queries the server for symbols matching the query string
// across the entire workspace.
func (c *DefaultLSPClient) WorkspaceSymbol(ctx context.Context, query string) ([]SymbolInformation, error) {
	params := map[string]any{"query": query}

	var symbols []SymbolInformation
	if err := c.rpc.Call(ctx, "workspace/symbol", params, &symbols); err != nil {
		return nil, fmt.Errorf("lsp: workspace/symbol: %w", err)
	}
	return symbols, nil
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
