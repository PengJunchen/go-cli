package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// JSONRPCLineTransport is an in-process transport used by the SDK adapters to
// exchange newline-delimited JSON-RPC messages with an MCP server. It reads
// request/response frames from an io.Reader and writes them to an io.Writer.
//
// For the Stdio transport the reader/writer are the spawned subprocess's
// stdout/stdin pipes; for the HTTP/SSE transports they would be backed by an
// HTTP stream. Because it only needs a reader/writer pair, tests can drive the
// adapters over an in-memory connection without importing any external SDK.
type JSONRPCLineTransport struct {
	in     *bufio.Reader
	out    io.Writer
	closeF func() error

	mu     sync.Mutex
	nextID int64
}

// NewJSONRPCLineTransport returns a transport that writes frames to out and
// reads them from in. closeFn is invoked when Close is called and may be nil.
func NewJSONRPCLineTransport(in io.Reader, out io.Writer, closeFn func() error) *JSONRPCLineTransport {
	return &JSONRPCLineTransport{in: bufio.NewReader(in), out: out, closeF: closeFn}
}

// Request sends a JSON-RPC request with a fresh id and blocks until a response
// with a matching id arrives. It returns the parsed result object.
func (t *JSONRPCLineTransport) Request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	id := t.nextID
	t.nextID++
	req, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}

	if _, err := fmt.Fprintln(t.out, string(req)); err != nil {
		return nil, fmt.Errorf("mcp: write request: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		line, readErr := t.in.ReadString('\n')
		if readErr != nil {
			return nil, fmt.Errorf("mcp: read response: %w", readErr)
		}

		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		if fid, ok := frame["id"].(float64); ok && int64(fid) == id {
			if errVal, hasErr := frame["error"]; hasErr && errVal != nil {
				return nil, fmt.Errorf("mcp: rpc error: %v", errVal)
			}
			result, ok := frame["result"].(map[string]any)
			if !ok {
				return nil, nil
			}
			return result, nil
		}
	}
}

// Send sends a JSON-RPC notification (no id, no response expected) to the
// server. It is used for the notifications/initialized message in the MCP
// handshake.
func (t *JSONRPCLineTransport) Send(method string, params map[string]any) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return fmt.Errorf("mcp: marshal notification: %w", err)
	}
	if _, err := fmt.Fprintln(t.out, string(msg)); err != nil {
		return fmt.Errorf("mcp: write notification: %w", err)
	}
	return nil
}

// Close releases the underlying connection, if one was supplied.
func (t *JSONRPCLineTransport) Close() error {
	if t.closeF != nil {
		return t.closeF()
	}
	return nil
}

// AdapterOption configures an SDK adapter.
type AdapterOption func(*adapterCore)

// WithConnection overrides the default transport with a caller-supplied
// in-process connection. It is primarily used by tests to drive an adapter
// against an in-memory server without spawning a subprocess.
func WithConnection(conn *JSONRPCLineTransport) AdapterOption {
	return func(c *adapterCore) { c.conn = conn; c.externalConn = true }
}

// WithoutInitialize disables the automatic MCP initialize/initialized
// handshake during Connect. It is used by tests that drive lightweight fake
// servers which do not implement the initialize method.
func WithoutInitialize() AdapterOption {
	return func(c *adapterCore) { c.skipInitialize = true }
}

// adapterCore holds the shared state and MCPClient logic used by both SDK
// adapters. The OfficialSDKAdapter and Mark3labsAdapter embed it so they are
// interchangeable MCPClient implementations backed by the same self-contained
// transport (no external SDK dependency).
type adapterCore struct {
	cfg          MCPServerConfig
	conn         *JSONRPCLineTransport
	externalConn bool
	proc         *exec.Cmd

	mu                sync.Mutex
	connected         bool
	maxReconnect      int
	reconnectAttempts int
	skipInitialize    bool
	protocolVersion   string
}

// defaultMaxReconnect is the number of reconnection attempts made when a Call
// fails due to a transport-level connection loss before giving up.
const defaultMaxReconnect = 3

// newAdapterCore builds the core for a given config and options.
func newAdapterCore(cfg MCPServerConfig, opts []AdapterOption) *adapterCore {
	c := &adapterCore{cfg: cfg, maxReconnect: defaultMaxReconnect}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Connect establishes the session. For a Stdio server it spawns the configured
// command and wraps its stdin/stdout in a JSON-RPC line transport.
func (c *adapterCore) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.connected {
		return nil
	}

	if c.conn == nil {
		if c.cfg.Transport != MCPTransportStdio {
			return fmt.Errorf("mcp: transport %s not supported without an explicit connection", c.cfg.Transport)
		}
		if strings.TrimSpace(c.cfg.Command) == "" {
			return fmt.Errorf("mcp: server %q requires a command for stdio transport", c.cfg.Name)
		}

		cmd := exec.CommandContext(ctx, c.cfg.Command, c.cfg.Args...) //nolint:gosec // G204: server executable comes from trusted config
		cmd.Env = append(cmd.Environ(), c.cfg.Env...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("mcp: stdin pipe: %w", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("mcp: stdout pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("mcp: start server %q: %w", c.cfg.Name, err)
		}
		c.proc = cmd
		c.conn = NewJSONRPCLineTransport(stdout, stdin, func() error {
			// Closing the transport does not kill the process; Disconnect
			// handles process teardown explicitly to avoid a double-Kill.
			return nil
		})
	}

	c.connected = true

	if !c.skipInitialize {
		if err := c.doInitializeLocked(ctx); err != nil {
			c.connected = false
			if !c.externalConn && c.conn != nil {
				_ = c.conn.Close() //nolint:errcheck
				c.conn = nil
			}
			if c.proc != nil {
				_ = c.proc.Process.Kill() //nolint:errcheck
				c.proc = nil
			}
			return fmt.Errorf("mcp: initialize handshake: %w", err)
		}
	}

	return nil
}

// Disconnect tears down the session and stops any spawned subprocess.
func (c *adapterCore) Disconnect(_ context.Context) error {
	c.mu.Lock()

	if !c.connected {
		c.mu.Unlock()
		return nil
	}
	c.connected = false

	var err error
	var waitProc *exec.Cmd
	if !c.externalConn && c.conn != nil {
		if closeErr := c.conn.Close(); closeErr != nil {
			err = closeErr
		}
		c.conn = nil
	}
	if c.proc != nil {
		waitProc = c.proc
		if killErr := c.proc.Process.Kill(); err == nil {
			err = killErr
		}
		c.proc = nil
	}
	c.mu.Unlock()

	if waitProc != nil {
		_ = waitProc.Wait() //nolint:errcheck // best-effort wait for process exit
	}

	slog.Info("mcp.disconnect", "server", c.cfg.Name, "transport", c.cfg.Transport)
	return err
}

// reconnect tears down the current session and re-establishes it. It is called
// by callWithReconnect when a transport-level error suggests the connection was
// lost. The caller is responsible for enforcing the maxReconnect budget and
// backoff between attempts.
func (c *adapterCore) reconnect(ctx context.Context) error {
	// Disconnect ignoring errors — we are already in a degraded state.
	_ = c.Disconnect(ctx) //nolint:errcheck // best-effort teardown
	return c.Connect(ctx)
}

// reconnectBackoff returns the delay before the (zero-based) attempt-th
// reconnection try: 1s, 2s, 4s, 8s, ... capped at 8s.
func reconnectBackoff(attempt int) time.Duration {
	d := time.Second
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= 8*time.Second {
			return 8 * time.Second
		}
	}
	return d
}

// isConnectionError reports whether err likely indicates a transport-level
// connection loss rather than a server-side RPC error. RPC errors are
// server-level responses and must not trigger a reconnect.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "rpc error") {
		return false
	}
	return strings.Contains(msg, "write request") ||
		strings.Contains(msg, "read response") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "EOF")
}

// requestLocked snapshots c.conn under c.mu and sends a JSON-RPC request.
// Holding the lock only for the pointer read (not the blocking Request call)
// prevents a nil-pointer dereference when a concurrent Disconnect or reconnect
// nils out c.conn between the read and the call.
func (c *adapterCore) requestLocked(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return nil, fmt.Errorf("mcp: connection is not established")
	}
	return conn.Request(ctx, method, params)
}

// callWithReconnect sends a JSON-RPC request and, when the error looks like a
// transport-level connection loss, attempts to reconnect (with exponential
// backoff) up to maxReconnect times before returning. On a successful
// reconnect the request is retried once.
func (c *adapterCore) callWithReconnect(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	res, err := c.requestLocked(ctx, method, params)
	if err == nil {
		return res, nil
	}
	if !isConnectionError(err) {
		return nil, err
	}

	for attempt := 0; attempt < c.maxReconnect; attempt++ {
		c.mu.Lock()
		c.reconnectAttempts++
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(reconnectBackoff(attempt)):
		}

		if rErr := c.reconnect(ctx); rErr != nil {
			err = rErr
			continue
		}
		// Reconnected — retry the request.
		if res, err = c.requestLocked(ctx, method, params); err == nil {
			return res, nil
		}
		if !isConnectionError(err) {
			return nil, err
		}
	}
	return nil, err
}

// ListTools returns the tools declared by the server via the tools/list
// JSON-RPC method.
func (c *adapterCore) ListTools(ctx context.Context) ([]MCPTool, error) {
	res, err := c.callWithReconnect(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools: %w", err)
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
		t := MCPTool{
			Name:        stringOf(obj["name"]),
			Description: stringOf(obj["description"]),
			ArgsSchema:  mapOf(obj["inputSchema"]),
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// CallTool invokes the named tool via the tools/call JSON-RPC method, emitting
// an mcp.tool.call span with tool_name/server/transport attributes.
func (c *adapterCore) CallTool(ctx context.Context, name string, args map[string]any) (*MCPToolResult, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, "mcp.tool.call", tracing.SpanKindInternal)
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "tool_name", Value: name},
		tracing.Attribute{Key: "server", Value: c.cfg.Name},
		tracing.Attribute{Key: "transport", Value: c.cfg.Transport.String()},
	)
	defer span.End()

	start := time.Now()
	res, err := c.callWithReconnect(spanCtx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		logger.Error("mcp.tool.call.failed", "tool", name, "server", c.cfg.Name, "err", err)
		return nil, fmt.Errorf("mcp: call tool %q: %w", name, err)
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: true})
	logger.Info("mcp.tool.call",
		"tool", name,
		"server", c.cfg.Name,
		"duration_ms", time.Since(start).Milliseconds(),
		"success", true)

	result := &MCPToolResult{
		Content: res["content"],
		IsError: boolVal(res["isError"]),
	}
	if result.Content == nil {
		result.Content = ""
	}
	return result, nil
}

// Name returns the logical server name.
func (c *adapterCore) Name() string { return c.cfg.Name }

// ProtocolVersion returns the protocol version negotiated during the
// initialize handshake, or "" before Connect.
func (c *adapterCore) ProtocolVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocolVersion
}

// doInitializeLocked performs the MCP initialize/initialized handshake. The
// caller must hold c.mu. It sends an initialize request, validates the
// returned protocolVersion against SupportedProtocolVersions, stores the
// negotiated version, and sends notifications/initialized.
func (c *adapterCore) doInitializeLocked(ctx context.Context) error {
	conn := c.conn
	if conn == nil {
		return fmt.Errorf("mcp: connection is not established")
	}
	res, err := conn.Request(ctx, "initialize", map[string]any{
		"protocolVersion": LatestProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "go-cli", "version": "1.0.0"},
	})
	if err != nil {
		return err
	}
	version, _ := res["protocolVersion"].(string)
	if !IsSupportedProtocolVersion(version) {
		return fmt.Errorf("unsupported protocol version: %s", version)
	}
	c.protocolVersion = version
	return conn.Send("notifications/initialized", map[string]any{})
}

// OfficialSDKAdapter is an MCPClient adapter that corresponds to the official
// Go MCP SDK's client transports. It uses a self-contained in-process
// transport (see JSONRPCLineTransport) so no external SDK module is required.
type OfficialSDKAdapter struct {
	*adapterCore
}

var _ MCPClient = (*OfficialSDKAdapter)(nil)

// NewOfficialSDKAdapter returns an OfficialSDKAdapter for the given config.
func NewOfficialSDKAdapter(cfg MCPServerConfig, opts ...AdapterOption) *OfficialSDKAdapter {
	return &OfficialSDKAdapter{adapterCore: newAdapterCore(cfg, opts)}
}

// Mark3labsAdapter is an MCPClient adapter symmetric to OfficialSDKAdapter. It
// corresponds to the mark3labs/mcp-go SDK's client role but is implemented it
// against the same self-contained transport, keeping both adapters
// interchangeable with no external dependency.
type Mark3labsAdapter struct {
	*adapterCore
}

var _ MCPClient = (*Mark3labsAdapter)(nil)

// NewMark3labsAdapter returns a Mark3labsAdapter for the given config.
func NewMark3labsAdapter(cfg MCPServerConfig, opts ...AdapterOption) *Mark3labsAdapter {
	return &Mark3labsAdapter{adapterCore: newAdapterCore(cfg, opts)}
}

// stringOf returns the string value of v, or "" when v is nil or not a string.
func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// boolVal returns the bool value of v, or false otherwise.
func boolVal(v any) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

// mapOf returns v as a map[string]any, or nil.
func mapOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}
