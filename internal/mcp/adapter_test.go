package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// recordingExporter is a minimal in-test trace exporter used to assert that
// adapters emit an mcp.tool.call span. It mirrors MockTraceExporter so mcp
// tests do not need to import internal/mock (avoiding an import cycle).
type recordingExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

func (e *recordingExporter) ExportSpan(_ context.Context, s tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanToData(s))
	return nil
}

func (e *recordingExporter) Shutdown(context.Context) error { return nil }

func (e *recordingExporter) Spans() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]tracing.SpanData, len(e.spans))
	copy(out, e.spans)
	return out
}

// fakeMCPConn spins up an in-process JSON-RPC server speaking the same
// newline-delimited protocol as JSONRPCLineTransport. It returns the server
// connection (held by the caller to close and stop the goroutine) and a
// transport wired to that server that an adapter can use.
func fakeMCPConn(t *testing.T) (net.Conn, *JSONRPCLineTransport) {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	go runFakeMCPServer(serverConn)
	return serverConn, NewJSONRPCLineTransport(clientConn, clientConn, clientConn.Close)
}

// runFakeMCPServer responds to tools/list and tools/call requests.
func runFakeMCPServer(conn net.Conn) {
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}

		var res map[string]any
		switch req.Method {
		case "tools/list":
			res = map[string]any{
				"tools": []map[string]any{
					{"name": "echo", "description": "echoes arguments"},
				},
			}
		case "tools/call":
			res = map[string]any{"content": "hello"}
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

func newTestTransportAdapter(t *testing.T) (net.Conn, MCPClient) {
	t.Helper()
	serverConn, conn := fakeMCPConn(t)
	cfg := MCPServerConfig{Name: "srv", Transport: MCPTransportStdio}
	adapter := NewOfficialSDKAdapter(cfg, WithConnection(conn))
	return serverConn, adapter
}

func TestAdaptedListTools(t *testing.T) {
	t.Parallel()
	serverConn, adapter := newTestTransportAdapter(t)
	defer closeConn(serverConn)

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))
	tools, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0].Name)
	assert.Equal(t, "echoes arguments", tools[0].Description)
}

func TestAdaptedCallTool(t *testing.T) {
	t.Parallel()
	serverConn, adapter := newTestTransportAdapter(t)
	defer closeConn(serverConn)

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))
	result, err := adapter.CallTool(ctx, "echo", map[string]any{"msg": "hi"})
	require.NoError(t, err)
	assert.Equal(t, "hello", result.Content)
	assert.False(t, result.IsError)
}

func TestMark3labsAdapterIsInterchangeable(t *testing.T) {
	t.Parallel()
	serverConn, conn := fakeMCPConn(t)
	defer closeConn(serverConn)

	cfg := MCPServerConfig{Name: "srv", Transport: MCPTransportStdio}
	adapter := NewMark3labsAdapter(cfg, WithConnection(conn))

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))
	tools, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "mcp__srv__echo", NormalizeToolName(adapter.Name(), tools[0].Name))
}

func TestAdapterConnectUnsupportedTransport(t *testing.T) {
	t.Parallel()
	cfg := MCPServerConfig{Name: "srv", Transport: MCPTransportStreamableHTTP}
	adapter := NewOfficialSDKAdapter(cfg)
	err := adapter.Connect(context.Background())
	require.Error(t, err)
}

func TestAdapterConnectMissingCommand(t *testing.T) {
	t.Parallel()
	cfg := MCPServerConfig{Name: "srv", Transport: MCPTransportStdio}
	adapter := NewOfficialSDKAdapter(cfg)
	err := adapter.Connect(context.Background())
	require.Error(t, err)
}

func TestAdapterCallToolEmitsSpan(t *testing.T) {
	t.Parallel()
	serverConn, conn := fakeMCPConn(t)
	defer closeConn(serverConn)

	exporter := &recordingExporter{}
	tracer := tracing.NewTracer("trace-mcp-adapter", exporter)
	span, ctx := tracer.Start(context.Background(), "test.root", tracing.SpanKindInternal)
	defer span.End()

	cfg := MCPServerConfig{Name: "srv", Transport: MCPTransportStdio}
	adapter := NewOfficialSDKAdapter(cfg, WithConnection(conn))
	require.NoError(t, adapter.Connect(ctx))

	_, err := adapter.CallTool(ctx, "echo", map[string]any{"msg": "hi"})
	require.NoError(t, err)

	// The adapter's inner span is exported asynchronously.
	require.Eventually(t, func() bool {
		for _, s := range exporter.Spans() {
			if s.Name == "mcp.tool.call" {
				return true
			}
		}
		return false
	}, time.Second, 5*time.Millisecond, "expected mcp.tool.call span")

	// Verify the span attributes carry tool_name/server/transport.
	found := false
	for _, s := range exporter.Spans() {
		if s.Name != "mcp.tool.call" {
			continue
		}
		attrs := map[string]any{}
		for _, a := range s.Attributes {
			attrs[a.Key] = a.Value
		}
		assert.Equal(t, "echo", attrs["tool_name"])
		assert.Equal(t, "srv", attrs["server"])
		assert.Equal(t, "stdio", attrs["transport"])
		found = true
	}
	assert.True(t, found, "mcp.tool.call span with attributes was exported")
}
