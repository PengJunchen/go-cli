package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// HTTPClientAdapter is an MCPClient that communicates with an MCP server over
// HTTP/SSE. It implements the standard MCP SSE handshake: connect to the
// server URL via GET, receive an "endpoint" event with the POST-back URL,
// then send JSON-RPC requests via HTTP POST and read responses from the SSE
// stream.
type HTTPClientAdapter struct {
	cfg       MCPServerConfig
	client    *http.Client
	endpoint  string // resolved POST endpoint (may be relative to base URL)
	mu        sync.Mutex
	connected bool
	nextID    int64
}

var _ MCPClient = (*HTTPClientAdapter)(nil)

// defaultHTTPTimeout bounds individual HTTP requests. Network fetch
// operations (e.g. web search) can legitimately take several seconds, but
// without a cap a hung server would block the agent loop forever.
const defaultHTTPTimeout = 60 * time.Second

// NewHTTPClientAdapter returns an HTTPClientAdapter for the given config.
func NewHTTPClientAdapter(cfg MCPServerConfig) *HTTPClientAdapter {
	return &HTTPClientAdapter{
		cfg: cfg,
		client: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

// NewHTTPClientAdapterWithClient returns an HTTPClientAdapter using the given
// *http.Client, allowing callers to override transport or timeouts.
func NewHTTPClientAdapterWithClient(cfg MCPServerConfig, client *http.Client) *HTTPClientAdapter {
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &HTTPClientAdapter{cfg: cfg, client: client}
}

// Connect verifies the server is reachable and prepares the POST endpoint.
// Streamable HTTP MCP servers respond to JSON-RPC via POST directly; some
// servers (SSE-style) send an "endpoint" event on the initial GET. We attempt
// the GET handshake first, then fall back to using the configured URL as the
// direct POST endpoint.
func (a *HTTPClientAdapter) Connect(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.connected {
		return nil
	}

	// Determine the POST endpoint. Most modern MCP servers (streamable HTTP)
	// use the configured URL directly; SSE-style servers send a POST-back
	// endpoint on the initial GET stream.
	a.endpoint = a.cfg.URL

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.URL, nil)
	if err != nil {
		return fmt.Errorf("mcp: build SSE request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := a.client.Do(req)
	if err != nil {
		// Server unreachable via GET; still try direct POST on the URL.
		slog.Warn("mcp.sse.connect_get_failed", "server", a.cfg.Name, "err", err)
		a.connected = true
		return nil
	}

	// Read the first event from the SSE stream to find the endpoint.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: endpoint") {
			// Next data: line has the endpoint URL
			if scanner.Scan() {
				dataLine := scanner.Text()
				if strings.HasPrefix(dataLine, "data: ") {
					a.endpoint = strings.TrimPrefix(dataLine, "data: ")
					break
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("mcp.sse.scan", "server", a.cfg.Name, "err", err)
	}
	resp.Body.Close() //nolint:errcheck,gosec

	if a.endpoint == "" {
		// No endpoint event: streamable HTTP server, use the URL directly.
		a.endpoint = a.cfg.URL
	} else if !strings.HasPrefix(a.endpoint, "http") {
		// Resolve relative endpoint against the base URL
		baseEnd := strings.LastIndex(a.cfg.URL, "/")
		if baseEnd > 0 {
			a.endpoint = a.cfg.URL[:baseEnd+1] + strings.TrimPrefix(a.endpoint, "/")
		}
	}

	a.connected = true
	slog.Info("mcp.http.connect", "server", a.cfg.Name, "endpoint", a.endpoint)
	return nil
}

// Disconnect tears down the connection.
func (a *HTTPClientAdapter) Disconnect(_ context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.connected = false
	return nil
}

// Name returns the logical server name.
func (a *HTTPClientAdapter) Name() string { return a.cfg.Name }

// ListTools calls the tools/list JSON-RPC method.
func (a *HTTPClientAdapter) ListTools(ctx context.Context) ([]MCPTool, error) {
	res, err := a.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	raw, ok := res["tools"].([]any)
	if !ok {
		return nil, fmt.Errorf("mcp: unexpected tools/list result")
	}
	tools := make([]MCPTool, 0, len(raw))
	for _, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		tools = append(tools, MCPTool{
			Name:        stringOf(obj["name"]),
			Description: stringOf(obj["description"]),
			ArgsSchema:  mapOf(obj["inputSchema"]),
		})
	}
	return tools, nil
}

// CallTool invokes the named tool via the tools/call JSON-RPC method.
func (a *HTTPClientAdapter) CallTool(ctx context.Context, name string, args map[string]any) (*MCPToolResult, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "mcp.tool.call", tracing.SpanKindInternal)
	span.SetAttributes(
		tracing.Attribute{Key: "tool_name", Value: name},
		tracing.Attribute{Key: "server", Value: a.cfg.Name},
		tracing.Attribute{Key: "transport", Value: "sse"},
	)
	defer span.End()

	res, err := a.request(spanCtx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: call tool %q: %w", name, err)
	}

	result := &MCPToolResult{
		Content: res["content"],
		IsError: boolVal(res["isError"]),
	}
	if result.Content == nil {
		result.Content = ""
	}
	return result, nil
}

// request sends a JSON-RPC request via HTTP POST and returns the result.
func (a *HTTPClientAdapter) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	a.mu.Lock()
	id := a.nextID
	a.nextID++
	a.mu.Unlock()

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("mcp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mcp: read response: %w", err)
	}

	// The response may be plain JSON or SSE-formatted. Streamable HTTP MCP
	// servers (e.g. Zhipu) wrap the JSON-RPC payload in SSE:
	//
	//   id:1
	//   event:message
	//   data:{"jsonrpc":"2.0","id":1,"result":{...}}
	//
	// Extract the JSON from data: lines when the body is not valid JSON.
	jsonBytes := raw
	if len(raw) > 0 && raw[0] != '{' {
		jsonBytes = extractSSEData(raw)
	}

	var frame map[string]any
	if err := json.Unmarshal(jsonBytes, &frame); err != nil {
		return nil, fmt.Errorf("mcp: decode response: %w (body: %s)", err, truncate(string(raw), 500))
	}

	if errVal, hasErr := frame["error"]; hasErr && errVal != nil {
		return nil, fmt.Errorf("mcp: rpc error: %v", errVal)
	}

	result, ok := frame["result"].(map[string]any)
	if !ok {
		return nil, nil
	}
	return result, nil
}

// extractSSEData scans SSE-formatted text and concatenates all data: line
// payloads into a single JSON byte slice.
func extractSSEData(raw []byte) []byte {
	var buf strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			buf.WriteString(data)
		}
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return raw
	}
	return []byte(out)
}

// truncate returns s trimmed to at most n characters with an ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
