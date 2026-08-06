package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// Mock LSP server helpers
// ---------------------------------------------------------------------------

// writeLSPFrame writes a single Content-Length framed JSON-RPC message.
func writeLSPFrame(w io.Writer, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := w.Write([]byte(header)); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

// readLSPFrame reads a single Content-Length framed message and returns the
// raw JSON body.
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

// mockLSPServer runs a fake LSP server on conn. It responds to the standard
// LSP methods and sends a diagnostics notification after initialize.
func mockLSPServer(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		body, err := readLSPFrame(reader)
		if err != nil {
			return
		}
		var msg jsonRPCMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}

		// Handle notifications (no ID) — just ignore them.
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
				},
				"serverInfo": map[string]any{
					"name":    "mock-lsp",
					"version": "1.0.0",
				},
			}
			// Write the initialize response first.
			writeLSPFrame(conn, map[string]any{ //nolint:errcheck,gosec
				"jsonrpc": "2.0",
				"id":      *msg.ID,
				"result":  result,
			})
			// Then send a diagnostics notification.
			writeLSPFrame(conn, map[string]any{ //nolint:errcheck,gosec
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

		case "textDocument/references":
			result = []map[string]any{
				{
					"uri": "file:///test/main.go",
					"range": map[string]any{
						"start": map[string]any{"line": 10, "character": 0},
						"end":   map[string]any{"line": 10, "character": 5},
					},
				},
				{
					"uri": "file:///test/util.go",
					"range": map[string]any{
						"start": map[string]any{"line": 3, "character": 6},
						"end":   map[string]any{"line": 3, "character": 11},
					},
				},
			}

		case "textDocument/hover":
			result = map[string]any{
				"contents": map[string]any{
					"kind":  "markdown",
					"value": "func main() — program entry point",
				},
			}

		case "shutdown":
			result = nil

		default:
			result = nil
		}

		writeLSPFrame(conn, map[string]any{ //nolint:errcheck,gosec
			"jsonrpc": "2.0",
			"id":      *msg.ID,
			"result":  result,
		})
	}
}

// mockLSPServerError runs a fake LSP server that always responds with an
// RPC error for every request.
func mockLSPServerError(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		body, err := readLSPFrame(reader)
		if err != nil {
			return
		}
		var msg jsonRPCMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}
		if msg.ID == nil {
			continue
		}
		writeLSPFrame(conn, map[string]any{ //nolint:errcheck,gosec
			"jsonrpc": "2.0",
			"id":      *msg.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": "method not found",
			},
		})
	}
}

// newMockLSPConn creates a JSONRPCClient connected to a mock LSP server.
// Returns the client and a cleanup function.
func newMockLSPConn(t *testing.T, serverFn func(net.Conn)) (*JSONRPCClient, func()) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		serverFn(serverConn)
	}()
	rpc := NewJSONRPCClient(clientConn, clientConn)
	cleanup := func() {
		_ = rpc.Close()        //nolint:errcheck
		_ = clientConn.Close() //nolint:errcheck
		<-done
	}
	return rpc, cleanup
}

// newMockDefaultLSPClient creates a DefaultLSPClient connected to a mock
// LSP server. Returns the client and a cleanup function.
func newMockDefaultLSPClient(t *testing.T) (*DefaultLSPClient, func()) {
	t.Helper()
	rpc, cleanup := newMockLSPConn(t, mockLSPServer)
	client := newDefaultLSPClientWithRPC(rpc)
	return client, cleanup
}

// ---------------------------------------------------------------------------
// JSONRPCClient tests
// ---------------------------------------------------------------------------

func TestJSONRPCCall(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	rpc, cleanup := newMockLSPConn(t, mockLSPServer)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result struct {
		Capabilities map[string]any `json:"capabilities"`
	}
	err := rpc.Call(ctx, "initialize", map[string]any{
		"rootUri": "file:///test",
	}, &result)
	require.NoError(t, err)
	assert.NotNil(t, result.Capabilities)
}

func TestJSONRPCNotify(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	rpc, cleanup := newMockLSPConn(t, mockLSPServer)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := rpc.Notify(ctx, "initialized", map[string]any{})
	require.NoError(t, err)
}

func TestJSONRPCCallError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	rpc, cleanup := newMockLSPConn(t, mockLSPServerError)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := rpc.Call(ctx, "initialize", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "method not found")
}

func TestJSONRPCCallContextCanceled(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	rpc, cleanup := newMockLSPConn(t, func(conn net.Conn) {
		// Server that reads but never responds.
		reader := bufio.NewReader(conn)
		readLSPFrame(reader) //nolint:errcheck,gosec // consume the request
		// Block on the next read; this will error when the connection closes.
		readLSPFrame(reader) //nolint:errcheck,gosec
	})
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := rpc.Call(ctx, "initialize", nil, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestJSONRPCClose(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	rpc, cleanup := newMockLSPConn(t, mockLSPServer)
	defer cleanup()

	// Close should not panic and should be idempotent.
	require.NoError(t, rpc.Close())
	require.NoError(t, rpc.Close())
}

func TestJSONRPCConcurrentCalls(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	rpc, cleanup := newMockLSPConn(t, mockLSPServer)
	defer cleanup()

	const n = 5
	var wg sync.WaitGroup
	wg.Add(n)
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := rpc.Call(ctx, "textDocument/hover", map[string]any{
				"textDocument": map[string]any{"uri": "file:///test.go"},
				"position":     map[string]any{"line": 0, "character": 0},
			}, nil)
			if err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)
	var errs []error
	for e := range errCh {
		errs = append(errs, e)
	}
	require.Empty(t, errs, "concurrent calls should each succeed")
}

func TestJSONRPCNotificationHandler(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	rpc, cleanup := newMockLSPConn(t, mockLSPServer)
	defer cleanup()

	received := make(chan string, 1)
	rpc.SetNotifyHandler(func(method string, params json.RawMessage) {
		if method == "textDocument/publishDiagnostics" {
			received <- method
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// initialize triggers the mock server to send a diagnostics notification.
	err := rpc.Call(ctx, "initialize", map[string]any{"rootUri": "file:///test"}, nil)
	require.NoError(t, err)

	select {
	case method := <-received:
		assert.Equal(t, "textDocument/publishDiagnostics", method)
	case <-time.After(2 * time.Second):
		t.Fatal("notification handler was not invoked")
	}
}

// ---------------------------------------------------------------------------
// DefaultLSPClient tests
// ---------------------------------------------------------------------------

func TestLSPInitialize(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.Initialize(ctx, "file:///workspace")
	require.NoError(t, err)
}

func TestLSPDefinition(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	locs, err := client.Definition(ctx, "file:///test/main.go", 5, 10)
	require.NoError(t, err)
	require.Len(t, locs, 1)
	assert.Equal(t, "file:///test/main.go", locs[0].URI)
	assert.Equal(t, 10, locs[0].Range.Start.Line)
	assert.Equal(t, 0, locs[0].Range.Start.Character)
}

func TestLSPReferences(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	locs, err := client.References(ctx, "file:///test/main.go", 5, 10)
	require.NoError(t, err)
	require.Len(t, locs, 2)
	assert.Equal(t, "file:///test/main.go", locs[0].URI)
	assert.Equal(t, "file:///test/util.go", locs[1].URI)
}

func TestLSPHover(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	hover, err := client.Hover(ctx, "file:///test/main.go", 0, 0)
	require.NoError(t, err)
	assert.Contains(t, hover, "func main()")
}

func TestLSPDiagnostics(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	// The mock server sends diagnostics for file:///test/main.go after
	// initialize. Wait for them to arrive.
	require.Eventually(t, func() bool {
		diags, _ := client.Diagnostics(ctx, "file:///test/main.go") //nolint:errcheck
		return len(diags) > 0
	}, 2*time.Second, 10*time.Millisecond, "diagnostics should arrive after initialize")

	diags, err := client.Diagnostics(ctx, "file:///test/main.go")
	require.NoError(t, err)
	require.Len(t, diags, 1)
	assert.Equal(t, "unused variable", diags[0].Message)
	assert.Equal(t, "mock-lsp", diags[0].Source)
	assert.Equal(t, 1, diags[0].Severity)
	assert.Equal(t, 5, diags[0].Range.Start.Line)
	assert.Equal(t, 10, diags[0].Range.Start.Character)
}

func TestLSPDiagnosticsEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	// No diagnostics for this URI.
	diags, err := client.Diagnostics(ctx, "file:///nonexistent.go")
	require.NoError(t, err)
	assert.Empty(t, diags)
}

func TestLSPShutdown(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	err := client.Shutdown(ctx)
	require.NoError(t, err)
}

func TestLSPNewDefaultLSPClientEmptyCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	_, err := NewDefaultLSPClient(context.Background(), nil, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

// ---------------------------------------------------------------------------
// lspTool tests
// ---------------------------------------------------------------------------

func TestLSPToolName(t *testing.T) {
	tool := NewLSPTool(nil)
	assert.Equal(t, "lsp_query", tool.Name())
}

func TestLSPToolDescription(t *testing.T) {
	tool := NewLSPTool(nil)
	desc := tool.Description()
	assert.Contains(t, desc, "lsp")
	assert.Contains(t, desc, "semantic")
}

func TestLSPToolParameters(t *testing.T) {
	tool := NewLSPTool(nil)
	params := tool.(Parameterized).Parameters() //nolint:errcheck
	m, ok := params.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", m["type"])

	props, ok := m["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "operation")
	assert.Contains(t, props, "uri")
	assert.Contains(t, props, "line")
	assert.Contains(t, props, "character")
}

func TestLSPToolExecuteDefinition(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	tool := NewLSPTool(client)
	res, err := tool.Execute(ctx, ToolCall{
		ID: "test-1",
		Args: map[string]any{
			"operation": "definition",
			"uri":       "file:///test/main.go",
			"line":      float64(5),
			"character": float64(10),
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "file:///test/main.go")
	assert.Equal(t, "test-1", res.ToolCallID)
	assert.Equal(t, "definition", res.Metadata["operation"])
}

func TestLSPToolExecuteReferences(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	tool := NewLSPTool(client)
	res, err := tool.Execute(ctx, ToolCall{
		Args: map[string]any{
			"operation": "references",
			"uri":       "file:///test/main.go",
			"line":      float64(5),
			"character": float64(10),
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "file:///test/main.go")
	assert.Contains(t, res.Output, "file:///test/util.go")
}

func TestLSPToolExecuteHover(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	tool := NewLSPTool(client)
	res, err := tool.Execute(ctx, ToolCall{
		Args: map[string]any{
			"operation": "hover",
			"uri":       "file:///test/main.go",
			"line":      float64(0),
			"character": float64(0),
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "func main()")
}

func TestLSPToolExecuteDiagnostics(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	client, cleanup := newMockDefaultLSPClient(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, client.Initialize(ctx, "file:///workspace"))

	// Wait for diagnostics to arrive.
	require.Eventually(t, func() bool {
		diags, _ := client.Diagnostics(ctx, "file:///test/main.go") //nolint:errcheck
		return len(diags) > 0
	}, 2*time.Second, 10*time.Millisecond)

	tool := NewLSPTool(client)
	res, err := tool.Execute(ctx, ToolCall{
		Args: map[string]any{
			"operation": "diagnostics",
			"uri":       "file:///test/main.go",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "unused variable")
	assert.Contains(t, res.Output, "mock-lsp")
}

func TestLSPToolExecuteMissingOperation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewLSPTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"uri": "file:///test.go"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "operation")
}

func TestLSPToolExecuteMissingURI(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewLSPTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"operation": "definition"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "uri")
}

func TestLSPToolExecuteUnknownOperation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewLSPTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"operation": "frobnicate",
			"uri":       "file:///test.go",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown operation")
}
