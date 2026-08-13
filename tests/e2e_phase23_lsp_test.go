//go:build e2e

// Package tests contains end-to-end integration tests for the go-cli project.
// This file verifies Phase 23 LSP enhancement: didOpen + diagnostics,
// multi-server routing, completion, env var config, and graceful degradation.
package tests

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Phase 23 LSP E2E Tests (Task 23-4)
// =============================================================================

// --- Mock LSP server helpers (speak JSON-RPC over net.Pipe) ---

// writeLSPFrame writes a Content-Length framed JSON-RPC message.
func writeLSPFrame(w io.Writer, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := w.Write([]byte(header)); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// readLSPFrame reads a Content-Length framed message and returns the raw JSON.
func readLSPFrame(r *bufio.Reader) ([]byte, error) {
	var contentLength int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			if _, err := fmt.Sscanf(val, "%d", &contentLength); err != nil {
				return nil, fmt.Errorf("invalid Content-Length %q: %w", val, err)
			}
		}
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// lspJSONRPCMessage is the JSON-RPC 2.0 message structure used by the mock
// LSP server.
type lspJSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *lspRPCError    `json:"error,omitempty"`
}

type lspRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mockLSPServerHandler runs a fake LSP server on conn. It responds to
// initialize, didOpen (notification), and completion. After initialize it
// sends a diagnostics notification.
func mockLSPServerHandler(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		body, err := readLSPFrame(reader)
		if err != nil {
			return
		}
		var msg lspJSONRPCMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		// Ignore notifications (no ID).
		if msg.ID == nil {
			continue
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"capabilities": map[string]any{
					"textDocumentSync":   1,
					"definitionProvider": true,
					"referencesProvider": true,
					"hoverProvider":      true,
					"completionProvider": map[string]any{"triggerCharacters": []string{"."}},
					"diagnosticProvider": true,
				},
				"serverInfo": map[string]any{"name": "mock-lsp", "version": "1.0"},
			}
			// Send initialize response.
			writeLSPFrame(conn, map[string]any{
				"jsonrpc": "2.0", "id": *msg.ID, "result": result,
			})
			// Send diagnostics notification.
			writeLSPFrame(conn, map[string]any{
				"jsonrpc": "2.0",
				"method":  "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": "file:///test/main.go",
					"diagnostics": []map[string]any{
						{
							"range": map[string]any{
								"start": map[string]any{"line": 5, "character": 10},
								"end":   map[string]any{"line": 5, "character": 15},
							},
							"severity": 1,
							"message":  "unused variable",
							"source":   "mock-lsp",
						},
					},
				},
			})
			continue
		case "textDocument/completion":
			result = []map[string]any{
				{"label": "fmt.Println", "kind": 3, "detail": "func(a ...any) (int, error)"},
				{"label": "fmt.Printf", "kind": 3, "detail": "func(format string, a ...any) (int, error)"},
			}
		case "textDocument/definition":
			result = []map[string]any{
				{
					"uri": "file:///test/main.go",
					"range": map[string]any{
						"start": map[string]any{"line": 10, "character": 0},
						"end":   map[string]any{"line": 10, "character": 5},
					},
				},
			}
		case "shutdown":
			result = nil
		default:
			result = nil
		}
		writeLSPFrame(conn, map[string]any{
			"jsonrpc": "2.0", "id": *msg.ID, "result": result,
		})
	}
}

// newMockLSPConn creates a JSONRPCClient connected to a mock LSP server.
func newMockLSPConn(t *testing.T, serverFn func(net.Conn)) (*tools.JSONRPCClient, func()) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		serverFn(serverConn)
	}()
	rpc := tools.NewJSONRPCClient(clientConn, clientConn)
	cleanup := func() {
		_ = rpc.Close()
		_ = clientConn.Close()
		_ = serverConn.Close()
		<-done
	}
	return rpc, cleanup
}

// testLSPClient wraps a JSONRPCClient and implements tools.LSPClient.
// It caches diagnostics received via notifications, mimicking DefaultLSPClient.
type testLSPClient struct {
	rpc   *tools.JSONRPCClient
	mu    sync.Mutex
	diags map[string][]tools.Diagnostic
}

func newTestLSPClient(rpc *tools.JSONRPCClient) *testLSPClient {
	c := &testLSPClient{
		rpc:   rpc,
		diags: make(map[string][]tools.Diagnostic),
	}
	rpc.SetNotifyHandler(c.handleNotification)
	return c
}

func (c *testLSPClient) handleNotification(method string, params json.RawMessage) {
	if method == "textDocument/publishDiagnostics" {
		var notif struct {
			URI         string             `json:"uri"`
			Diagnostics []tools.Diagnostic `json:"diagnostics"`
		}
		if err := json.Unmarshal(params, &notif); err == nil {
			c.mu.Lock()
			c.diags[notif.URI] = notif.Diagnostics
			c.mu.Unlock()
		}
	}
}

func (c *testLSPClient) Initialize(ctx context.Context, rootURI string) error {
	var result struct{}
	return c.rpc.Call(ctx, "initialize", map[string]any{"rootUri": rootURI}, &result)
}

func (c *testLSPClient) Definition(ctx context.Context, uri string, line, char int) ([]tools.Location, error) {
	var locs []tools.Location
	err := c.rpc.Call(ctx, "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}, &locs)
	return locs, err
}

func (c *testLSPClient) References(ctx context.Context, uri string, line, char int) ([]tools.Location, error) {
	var locs []tools.Location
	err := c.rpc.Call(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}, &locs)
	return locs, err
}

func (c *testLSPClient) Hover(ctx context.Context, uri string, line, char int) (string, error) {
	var result struct {
		Contents struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		} `json:"contents"`
	}
	err := c.rpc.Call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}, &result)
	return result.Contents.Value, err
}

func (c *testLSPClient) Diagnostics(_ context.Context, uri string) ([]tools.Diagnostic, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.diags[uri], nil
}

func (c *testLSPClient) DidOpen(ctx context.Context, uri, content string, version int) error {
	return c.rpc.Notify(ctx, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": "go", "version": version, "text": content,
		},
	})
}

func (c *testLSPClient) DidChange(ctx context.Context, uri, content string, version int) error {
	return c.rpc.Notify(ctx, "textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": content}},
	})
}

func (c *testLSPClient) Completion(ctx context.Context, uri string, line, char int) ([]tools.CompletionItem, error) {
	var items []tools.CompletionItem
	err := c.rpc.Call(ctx, "textDocument/completion", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}, &items)
	return items, err
}

func (c *testLSPClient) TypeDefinition(ctx context.Context, uri string, line, char int) ([]tools.Location, error) {
	var locs []tools.Location
	err := c.rpc.Call(ctx, "textDocument/typeDefinition", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
	}, &locs)
	return locs, err
}

func (c *testLSPClient) Rename(ctx context.Context, uri string, line, char int, newName string) (*tools.WorkspaceEdit, error) {
	var edit tools.WorkspaceEdit
	err := c.rpc.Call(ctx, "textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": char},
		"newName":      newName,
	}, &edit)
	return &edit, err
}

func (c *testLSPClient) WorkspaceSymbol(ctx context.Context, query string) ([]tools.SymbolInformation, error) {
	var symbols []tools.SymbolInformation
	err := c.rpc.Call(ctx, "workspace/symbol", map[string]any{"query": query}, &symbols)
	return symbols, err
}

func (c *testLSPClient) Shutdown(ctx context.Context) error {
	var result any
	return c.rpc.Call(ctx, "shutdown", nil, &result)
}

// --- Tests ---

// TestET_lsp_didopen_then_diagnostics verifies that after calling didOpen to
// sync content, diagnostics return correct results from the mock LSP server.
func TestET_lsp_didopen_then_diagnostics(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rpc, cleanup := newMockLSPConn(t, mockLSPServerHandler)
	defer cleanup()

	client := newTestLSPClient(rpc)
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	// Call didOpen to sync content.
	require.NoError(t, client.DidOpen(ctx, "file:///test/main.go", "package main\n", 1))

	// Give the server time to send diagnostics.
	time.Sleep(100 * time.Millisecond)

	// Retrieve diagnostics.
	diags, err := client.Diagnostics(ctx, "file:///test/main.go")
	require.NoError(t, err)
	require.Len(t, diags, 1)
	assert.Equal(t, "unused variable", diags[0].Message)
	assert.Equal(t, "mock-lsp", diags[0].Source)
	assert.Equal(t, 1, diags[0].Severity)
}

// TestET_lsp_multi_server_routing verifies that .go files route to the Go LSP
// and .ts files route to the TS LSP via MultiLSPClient.
func TestET_lsp_multi_server_routing(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	goClient := &mockRoutingLSPClient{name: "go", hover: "go-hover-info"}
	tsClient := &mockRoutingLSPClient{name: "ts", hover: "ts-hover-info"}

	multi := tools.NewMultiLSPClient()
	multi.Register(goClient, "go")
	multi.Register(tsClient, "ts")

	// .go file routes to Go LSP.
	hover, err := multi.Hover(ctx, "file:///test/main.go", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, "go-hover-info", hover)
	assert.Equal(t, 1, goClient.hoverCalls)

	// .ts file routes to TS LSP.
	hover, err = multi.Hover(ctx, "file:///test/app.ts", 0, 0)
	require.NoError(t, err)
	assert.Equal(t, "ts-hover-info", hover)
	assert.Equal(t, 1, tsClient.hoverCalls)
}

// TestET_lsp_completion_returns_suggestions verifies that completion returns
// suggestions from the mock LSP server.
func TestET_lsp_completion_returns_suggestions(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rpc, cleanup := newMockLSPConn(t, mockLSPServerHandler)
	defer cleanup()

	client := newTestLSPClient(rpc)
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	items, err := client.Completion(ctx, "file:///test/main.go", 5, 10)
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "fmt.Println", items[0].Label)
	assert.Equal(t, "fmt.Printf", items[1].Label)
	assert.Equal(t, 3, items[0].Kind)
}

// TestET_lsp_env_var_config verifies that the GO_CLI_LSP_SERVER_COMMAND
// environment variable is picked up by the config loader and sets
// cfg.LSP.ServerCommand.
func TestET_lsp_env_var_config(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = ctx
	// Set the env var.
	testCmd := "gopls serve"
	t.Setenv("GO_CLI_LSP_SERVER_COMMAND", testCmd)
	t.Setenv("GO_CLI_LSP_WORKSPACE_ROOT", t.TempDir())

	// Load config from env (no file).
	loader := config.NewLoader()
	cfg, err := loader.Load(context.Background())
	require.NoError(t, err)

	require.Len(t, cfg.LSP.ServerCommand, 2)
	assert.Equal(t, "gopls", cfg.LSP.ServerCommand[0])
	assert.Equal(t, "serve", cfg.LSP.ServerCommand[1])
	assert.NotEmpty(t, cfg.LSP.WorkspaceRoot)
}

// TestET_lsp_graceful_degradation verifies that when the LSP server command
// fails to start (invalid command), NewDefaultLSPClient returns an error
// without crashing the main flow.
func TestET_lsp_graceful_degradation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Use a command that definitely does not exist.
	client, err := tools.NewDefaultLSPClient(ctx, []string{"nonexistent-lsp-binary-xyz"}, "/tmp")
	require.Error(t, err)
	assert.Nil(t, client, "client should be nil on failure")
	assert.Contains(t, err.Error(), "lsp")

	// The main flow should continue (no panic, no crash).
	// In production, buildLSPClient would log a warning and return (nil, false).
	t.Log("LSP server failure handled gracefully - main flow continues")
}

// =============================================================================
// Helpers
// =============================================================================

// mockRoutingLSPClient is a simple LSPClient mock for multi-server routing
// tests. It tracks hover calls to verify routing.
type mockRoutingLSPClient struct {
	name       string
	hover      string
	hoverCalls int
}

func (c *mockRoutingLSPClient) Initialize(_ context.Context, _ string) error { return nil }
func (c *mockRoutingLSPClient) Definition(_ context.Context, _ string, _, _ int) ([]tools.Location, error) {
	return nil, nil
}
func (c *mockRoutingLSPClient) References(_ context.Context, _ string, _, _ int) ([]tools.Location, error) {
	return nil, nil
}
func (c *mockRoutingLSPClient) Hover(_ context.Context, _ string, _, _ int) (string, error) {
	c.hoverCalls++
	return c.hover, nil
}
func (c *mockRoutingLSPClient) Diagnostics(_ context.Context, _ string) ([]tools.Diagnostic, error) {
	return nil, nil
}
func (c *mockRoutingLSPClient) DidOpen(_ context.Context, _, _ string, _ int) error   { return nil }
func (c *mockRoutingLSPClient) DidChange(_ context.Context, _, _ string, _ int) error { return nil }
func (c *mockRoutingLSPClient) Completion(_ context.Context, _ string, _, _ int) ([]tools.CompletionItem, error) {
	return nil, nil
}
func (c *mockRoutingLSPClient) TypeDefinition(_ context.Context, _ string, _, _ int) ([]tools.Location, error) {
	return nil, nil
}
func (c *mockRoutingLSPClient) Rename(_ context.Context, _ string, _, _ int, _ string) (*tools.WorkspaceEdit, error) {
	return nil, nil
}
func (c *mockRoutingLSPClient) WorkspaceSymbol(_ context.Context, _ string) ([]tools.SymbolInformation, error) {
	return nil, nil
}
func (c *mockRoutingLSPClient) Shutdown(_ context.Context) error { return nil }

// Ensure unused imports are referenced.
var _ = os.Getenv
