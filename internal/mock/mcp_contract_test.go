package mock

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestMCPContractNormalizedName checks that wrapping a MockMCPServerImpl via
// MCPToolAdapter and registering it into a tools registry normalizes the tool
// name to mcp__mock__<tool> and that execution flows through correctly.
func TestMCPContractNormalizedName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	server := NewMockMCPServer("")
	server.RegisterTool("echo", "echoes a message", func(args map[string]any) (any, error) {
		return "echo:" + fmt.Sprintf("%v", args["msg"]), nil
	})

	// The MockMCPServerImpl satisfies both MockMCPServer and mcp.MCPClient.
	var _ mcp.MCPClient = server

	adapter := mcp.NewMCPToolAdapter(server, mcp.MCPTool{Name: "echo", Description: "echoes a message"})
	require.Equal(t, "mcp__mock__echo", adapter.Name())

	reg := tools.NewDefaultToolRegistry()
	ctx := context.Background()
	require.NoError(t, reg.Register(ctx, adapter))

	// Executing through the registry returns the expected result.
	call := tools.ToolCall{ID: "c1", Name: "mcp__mock__echo", Args: map[string]any{"msg": "hi"}}
	result, err := reg.Execute(ctx, call)
	require.NoError(t, err)
	assert.Equal(t, "echo:hi", result.Output)
	assert.Equal(t, "c1", result.ToolCallID)

	// The invocation is recorded for assertions.
	logs := server.CallLog()
	require.Len(t, logs, 1)
	assert.Equal(t, "echo", logs[0].ToolName)
	assert.Equal(t, map[string]any{"msg": "hi"}, logs[0].Args)
	assert.Equal(t, "echo:hi", logs[0].Result)
	assert.False(t, logs[0].Timestamp.IsZero(), "call should carry a timestamp")
}

// TestMCPContractNameRoundTrip verifies that NormalizeToolName and
// ParseToolName round-trip deterministically, including a tool name that
// itself contains "__".
func TestMCPContractNameRoundTrip(t *testing.T) {
	server := NewMockMCPServer("github")

	name := mcp.NormalizeToolName(server.Name(), "compare__commits")
	require.Equal(t, "mcp__github__compare__commits", name)

	srv, tool, ok := mcp.ParseToolName(name)
	require.True(t, ok)
	assert.Equal(t, "github", srv)
	assert.Equal(t, "compare__commits", tool)

	// Names that are not MCP-prefixed are not treated as MCP tools.
	_, _, ok = mcp.ParseToolName("read")
	assert.False(t, ok)
}

// mockServerBridge forwards newline-delimited JSON-RPC requests read from the
// server side of a pipe to a MockMCPServerImpl and writes the responses back.
// It lets an OfficialSDKAdapter connect to the mock server over the same wire
// protocol the adapters speak.
func mockServerBridge(conn net.Conn, server *MockMCPServerImpl) {
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var req struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}

		var res map[string]any
		switch req.Method {
		case "tools/list":
			tls, err := server.ListTools(context.Background())
			if err == nil {
				items := make([]map[string]any, 0, len(tls))
				for _, t := range tls {
					items = append(items, map[string]any{
						"name":        t.Name,
						"description": t.Description,
					})
				}
				res = map[string]any{"tools": items}
			}
		case "tools/call":
			name, ok := req.Params["name"].(string)
			if !ok {
				return
			}
			args, okArgs := req.Params["arguments"].(map[string]any)
			if !okArgs {
				args = map[string]any{}
			}
			result, err := server.CallTool(context.Background(), name, args)
			if err != nil {
				res = map[string]any{"isError": true, "content": err.Error()}
			} else {
				res = map[string]any{"content": result.Content}
			}
		}

		frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res}
		if b, err := json.Marshal(frame); err == nil {
			if _, err := fmt.Fprintf(conn, "%s\n", b); err != nil {
				return
			}
		}
	}
	closeConn(conn)
}

// closeConn best-effort closes a connection during test cleanup.
func closeConn(c io.Closer) { _ = c.Close() } //nolint:errcheck // best-effort cleanup

// TestMCPContractAdapterConnectsToMockServer drives an OfficialSDKAdapter
// against a MockMCPServerImpl over an in-memory pipe, proving the adapter's
// ListTools/CallTool contract against the mock server.
func TestMCPContractAdapterConnectsToMockServer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	server := NewMockMCPServer("github")
	server.RegisterTool("list_issues", "lists issues", func(args map[string]any) (any, error) {
		return "issues:" + fmt.Sprintf("%v", args["repo"]), nil
	})

	serverConn, clientConn := net.Pipe()
	go mockServerBridge(serverConn, server)

	conn := mcp.NewJSONRPCLineTransport(clientConn, clientConn, clientConn.Close)

	cfg := mcp.MCPServerConfig{Name: "github", Transport: mcp.MCPTransportStdio}
	adapter := mcp.NewOfficialSDKAdapter(cfg, mcp.WithConnection(conn))
	ctx := context.Background()

	require.NoError(t, adapter.Connect(ctx))

	toolsList, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, toolsList, 1)
	assert.Equal(t, "list_issues", toolsList[0].Name)
	assert.Equal(t, "mcp__github__list_issues", mcp.NormalizeToolName(adapter.Name(), toolsList[0].Name))

	result, err := adapter.CallTool(ctx, "list_issues", map[string]any{"repo": "go-cli"})
	require.NoError(t, err)
	assert.Equal(t, "issues:go-cli", result.Content)
	assert.False(t, result.IsError)

	require.Len(t, server.CallLog(), 1)
	assert.Equal(t, "list_issues", server.CallLog()[0].ToolName)

	// Close the transport so the bridge goroutine observes EOF and exits
	// before the deferred goroutine-leak check runs.
	closeConn(conn)
}
