package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// HTTPClientAdapter is an MCPClient that communicates with an MCP server over
// the MCP 2025-06-18 Streamable HTTP transport.
type HTTPClientAdapter struct {
	cfg       MCPServerConfig
	client    *http.Client
	endpoint  string // validated Streamable HTTP MCP endpoint
	sessionID string
	lifecycle sync.RWMutex
	mu        sync.Mutex
	connected bool
	nextID    int64

	protocolVersion string
}

var _ MCPClient = (*HTTPClientAdapter)(nil)

var errCrossOriginRedirect = errors.New("mcp: cross-origin redirect rejected")

// ErrUnsupportedOAuth reports that this HTTP adapter does not implement the
// authorization flow configured for an MCP server. Callers may still provide
// an already-issued BearerToken.
var ErrUnsupportedOAuth = errors.New("mcp: OAuth configuration is unsupported by the Streamable HTTP adapter")

// ErrUnsupportedHTTPTransport reports that the configured HTTP transport is
// the deprecated HTTP+SSE profile rather than Streamable HTTP.
var ErrUnsupportedHTTPTransport = errors.New("mcp: only the Streamable HTTP transport is supported")

// HTTPProtocolError describes an invalid Streamable HTTP response or
// lifecycle transition without including remote response bodies.
type HTTPProtocolError struct {
	message string
	cause   error
}

func (e *HTTPProtocolError) Error() string { return "mcp: " + e.message }

func (e *HTTPProtocolError) Unwrap() error { return e.cause }

func httpProtocolError(format string, args ...any) error {
	return &HTTPProtocolError{message: fmt.Sprintf(format, args...)}
}

func httpProtocolErrorCause(message string, cause error) error {
	return &HTTPProtocolError{message: message, cause: cause}
}

const streamableHTTPProtocolVersion = "2025-06-18"

var errSSEResponseMissing = errors.New("mcp: SSE stream ended without the matching JSON-RPC response")

// defaultHTTPTimeout bounds individual HTTP requests. Network fetch
// operations (e.g. web search) can legitimately take several seconds, but
// without a cap a hung server would block the agent loop forever.
const defaultHTTPTimeout = 60 * time.Second

// maxHTTPResponseSize caps the size of an HTTP response body read by the
// adapter. This prevents a malicious or buggy server from exhausting memory
// by streaming an arbitrarily large response.
const maxHTTPResponseSize = 10 * 1024 * 1024 // 10 MB

var errHTTPResponseTooLarge = fmt.Errorf("mcp: response body exceeds %d byte limit", maxHTTPResponseSize)

type responseByteBudget struct {
	remaining int64
}

func newResponseByteBudget(limit int64) *responseByteBudget {
	return &responseByteBudget{remaining: limit}
}

func (b *responseByteBudget) wrap(source io.Reader) io.Reader {
	return responseBudgetReader{source: source, budget: b}
}

type responseBudgetReader struct {
	source io.Reader
	budget *responseByteBudget
}

func (r responseBudgetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := r.budget.remaining
	maxProbe := remaining + 1
	if int64(len(p)) > maxProbe {
		p = p[:maxProbe]
	}
	n, err := r.source.Read(p)
	if int64(n) > remaining {
		r.budget.remaining = 0
		return 0, errHTTPResponseTooLarge
	}
	r.budget.remaining -= int64(n)
	return n, err
}

// readLimitedBody reads at most maxHTTPResponseSize bytes from r. If the body
// exceeds the limit, an error is returned so the caller can abort instead of
// allocating unbounded memory.
func readLimitedBody(r io.Reader) ([]byte, error) {
	return readLimitedBodyWithBudget(r, newResponseByteBudget(maxHTTPResponseSize))
}

func readLimitedBodyWithBudget(r io.Reader, budget *responseByteBudget) ([]byte, error) {
	raw, err := io.ReadAll(budget.wrap(r))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func sameHTTPOrigin(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	leftScheme := strings.ToLower(left.Scheme)
	rightScheme := strings.ToLower(right.Scheme)
	if leftScheme != rightScheme || (leftScheme != "http" && leftScheme != "https") {
		return false
	}
	if !strings.EqualFold(left.Hostname(), right.Hostname()) {
		return false
	}
	return effectiveHTTPPort(left) == effectiveHTTPPort(right)
}

func effectiveHTTPPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		if value, err := strconv.Atoi(port); err == nil {
			return strconv.Itoa(value)
		}
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func resolveSameOriginEndpoint(configuredURL, endpoint string) (string, error) {
	base, err := url.Parse(configuredURL)
	if err != nil {
		return "", httpProtocolError("configured HTTP endpoint is invalid")
	}
	baseScheme := strings.ToLower(base.Scheme)
	if base.Host == "" || (baseScheme != "http" && baseScheme != "https") {
		return "", fmt.Errorf("mcp: configured endpoint must be an absolute HTTP URL")
	}
	ref, err := url.Parse(endpoint)
	if err != nil {
		return "", httpProtocolError("HTTP endpoint is invalid")
	}
	resolved := base.ResolveReference(ref)
	if !sameHTTPOrigin(base, resolved) {
		return "", fmt.Errorf("mcp: endpoint must remain on the configured origin %q", base.Host)
	}
	return resolved.String(), nil
}

// doHTTP preserves the configured client's transport and options while
// refusing redirects that would move a request to a different origin.
func (a *HTTPClientAdapter) doHTTP(req *http.Request) (*http.Response, error) {
	client := *a.client
	checkRedirect := client.CheckRedirect
	initialURL := *req.URL
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !sameHTTPOrigin(&initialURL, next.URL) {
			return errCrossOriginRedirect
		}
		if checkRedirect != nil {
			if err := checkRedirect(next, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		// The caller's CheckRedirect callback can mutate next.URL after the
		// first origin check, so validate the actual target immediately before
		// net/http sends the redirected request.
		if !sameHTTPOrigin(&initialURL, next.URL) {
			return errCrossOriginRedirect
		}
		return nil
	}
	resp, err := client.Do(req)
	if errors.Is(err, errCrossOriginRedirect) {
		return nil, errCrossOriginRedirect
	}
	return resp, err
}

// NewHTTPClientAdapter returns an HTTPClientAdapter for the given config. The
// underlying *http.Client is SSRF-safe: it blocks private, link-local, and
// cloud-metadata IP ranges at dial time (defending against DNS rebinding)
// while permitting loopback so local MCP servers keep working.
func NewHTTPClientAdapter(cfg MCPServerConfig) *HTTPClientAdapter {
	return &HTTPClientAdapter{
		cfg:    cfg,
		client: tools.NewSSRFSafeHTTPClientAllowLoopback(defaultHTTPTimeout),
	}
}

// NewHTTPClientAdapterWithClient returns an HTTPClientAdapter using the given
// *http.Client, allowing callers to override transport or timeouts. When client
// is nil, a loopback-allowing SSRF-safe client protects all requests against
// internal-IP targeting.
func NewHTTPClientAdapterWithClient(cfg MCPServerConfig, client *http.Client) *HTTPClientAdapter {
	if client == nil {
		client = tools.NewSSRFSafeHTTPClientAllowLoopback(defaultHTTPTimeout)
	}
	return &HTTPClientAdapter{cfg: cfg, client: client}
}

// Connect performs the 2025-06-18 initialize/initialized lifecycle over the
// configured Streamable HTTP endpoint. It never probes or falls back to the
// deprecated HTTP+SSE transport.
func (a *HTTPClientAdapter) Connect(ctx context.Context) error {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.connected {
		return nil
	}
	a.clearConnectionLocked()
	if a.cfg.OAuthConfig != nil {
		return ErrUnsupportedOAuth
	}
	if a.cfg.Transport != MCPTransportStreamableHTTP {
		return ErrUnsupportedHTTPTransport
	}

	endpoint, err := resolveSameOriginEndpoint(a.cfg.URL, a.cfg.URL)
	if err != nil {
		return err
	}
	a.endpoint = endpoint
	if err := a.doInitializeLocked(ctx); err != nil {
		a.clearConnectionLocked()
		return fmt.Errorf("mcp: initialize handshake: %w", err)
	}
	a.connected = true
	slog.Info("mcp.http.connect", "server", a.cfg.Name)
	return nil
}

func (a *HTTPClientAdapter) clearConnectionLocked() {
	a.connected = false
	a.endpoint = ""
	a.sessionID = ""
	a.protocolVersion = ""
}

// Disconnect closes the local connection and asks a session-aware server to
// delete the remote session. A 405 response is permitted by the transport and
// does not prove that any in-flight remote action stopped.
func (a *HTTPClientAdapter) Disconnect(ctx context.Context) error {
	a.lifecycle.Lock()
	defer a.lifecycle.Unlock()

	a.mu.Lock()
	if !a.connected {
		a.clearConnectionLocked()
		a.mu.Unlock()
		return nil
	}
	endpoint, version, sessionID := a.endpoint, a.protocolVersion, a.sessionID
	a.clearConnectionLocked()
	a.mu.Unlock()
	if sessionID == "" {
		return nil
	}

	req, err := a.newHTTPMessageRequest(ctx, http.MethodDelete, endpoint, nil, version, sessionID, "", "")
	if err != nil {
		return fmt.Errorf("mcp: build session DELETE: %w", err)
	}
	resp, err := a.doHTTP(req)
	if err != nil {
		return fmt.Errorf("mcp: delete session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck
	if resp.StatusCode == http.StatusMethodNotAllowed {
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return httpProtocolError("session DELETE returned HTTP status %d", resp.StatusCode)
	}
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
		tracing.Attribute{Key: "transport", Value: "streamable-http"},
	)
	defer span.End()

	res, err := a.request(spanCtx, "tools/call", map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: call tool %q: %w", name, toolCallOutcomeError(name, err))
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
	a.lifecycle.RLock()
	defer a.lifecycle.RUnlock()

	a.mu.Lock()
	if !a.connected {
		a.mu.Unlock()
		return nil, httpProtocolError("request %q requires an initialized connection", method)
	}
	id := a.nextID
	a.nextID++
	endpoint, version, sessionID := a.endpoint, a.protocolVersion, a.sessionID
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	req, err := a.newHTTPMessageRequest(ctx, http.MethodPost, endpoint, body, version, sessionID, "application/json, text/event-stream", "")
	if err != nil {
		return nil, fmt.Errorf("mcp: build request: %w", err)
	}
	resp, err := a.doHTTP(req)
	if err != nil {
		return nil, newRequestBoundaryError(fmt.Errorf("mcp: do request: %w", err), true, false)
	}
	responseBudget := newResponseByteBudget(maxHTTPResponseSize)
	result, cursor, resumable, responseErr := a.readRequestResponse(resp, id, sessionID, responseBudget)
	_ = resp.Body.Close()
	if responseErr != nil && resumable && cursor != "" && ctx.Err() == nil {
		result, responseErr = a.resumeRequestResponse(ctx, endpoint, version, sessionID, id, cursor, responseBudget)
	}
	if responseErr != nil {
		return nil, markRequestDispatched(responseErr)
	}
	return result, nil
}

// truncate returns s trimmed to at most n characters with an ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (a *HTTPClientAdapter) newHTTPMessageRequest(ctx context.Context, method, endpoint string, body []byte, version, sessionID, accept, lastEventID string) (*http.Request, error) {
	endpoint, err := resolveSameOriginEndpoint(a.cfg.URL, endpoint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	a.applyAuthHeaders(req)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	req.Header.Del("MCP-Protocol-Version")
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	req.Header.Del("Mcp-Session-Id")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	req.Header.Del("Last-Event-ID")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	return req, nil
}

func (a *HTTPClientAdapter) readRequestResponse(resp *http.Response, id int64, sessionID string, budget *responseByteBudget) (map[string]any, string, bool, error) {
	if resp.StatusCode != http.StatusOK {
		return nil, "", false, httpProtocolError("JSON-RPC POST returned HTTP status %d", resp.StatusCode)
	}
	if err := validateResponseSessionID(resp, sessionID); err != nil {
		return nil, "", false, err
	}
	return a.readRPCResponse(resp, id, budget)
}

func (a *HTTPClientAdapter) readRPCResponse(resp *http.Response, id int64, budget *responseByteBudget) (map[string]any, string, bool, error) {
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", false, httpProtocolError("invalid response Content-Type")
	}
	switch strings.ToLower(mediaType) {
	case "application/json":
		raw, readErr := readLimitedBodyWithBudget(resp.Body, budget)
		if readErr != nil {
			if errors.Is(readErr, errHTTPResponseTooLarge) {
				return nil, "", false, httpProtocolError("JSON-RPC response body exceeds size limit")
			}
			return nil, "", false, httpProtocolErrorCause("reading JSON-RPC response failed", readErr)
		}
		result, parseErr := parseJSONRPCResponse(raw, id)
		return result, "", false, parseErr
	case "text/event-stream":
		result, cursor, streamErr := readSSEResponseWithBudget(resp.Body, id, budget)
		if streamErr == nil {
			return result, cursor, false, nil
		}
		return nil, cursor, canResumeSSE(streamErr) && cursor != "", streamErr
	default:
		return nil, "", false, httpProtocolError("unsupported response Content-Type %q", mediaType)
	}
}

func validateResponseSessionID(resp *http.Response, current string) error {
	values, present := resp.Header[http.CanonicalHeaderKey("Mcp-Session-Id")]
	if !present {
		return nil
	}
	if len(values) != 1 || !validSessionID(values[0]) || values[0] != current {
		return httpProtocolError("server changed or returned an invalid MCP session ID")
	}
	return nil
}

func validSessionID(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func validLastEventID(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func canResumeSSE(err error) bool {
	if errors.Is(err, errSSEResponseMissing) {
		return true
	}
	var boundaryErr *requestBoundaryError
	if errors.As(err, &boundaryErr) {
		return false
	}
	var protocolErr *HTTPProtocolError
	if errors.As(err, &protocolErr) {
		return false
	}
	var rpcErr *rpcResponseError
	return !errors.As(err, &rpcErr)
}

func readSSEResponse(body io.Reader, expectedID int64) (map[string]any, string, error) {
	return readSSEResponseWithBudget(body, expectedID, newResponseByteBudget(maxHTTPResponseSize))
}

func readSSEResponseWithBudget(body io.Reader, expectedID int64, budget *responseByteBudget) (map[string]any, string, error) {
	scanner := bufio.NewScanner(budget.wrap(body))
	scanner.Buffer(make([]byte, 64*1024), maxHTTPResponseSize+1)
	var dataLines []string
	eventID := ""
	eventIDSet := false
	cursor := ""

	dispatch := func() (map[string]any, bool, error) {
		if eventIDSet {
			cursor = ""
			if validLastEventID(eventID) {
				cursor = eventID
			}
		}
		if len(dataLines) == 0 {
			dataLines = nil
			eventID = ""
			eventIDSet = false
			return nil, false, nil
		}
		data := []byte(strings.Join(dataLines, "\n"))
		dataLines = nil
		eventID = ""
		eventIDSet = false
		result, isResponse, err := parseSSEMessage(data, expectedID)
		return result, isResponse, err
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			result, isResponse, err := dispatch()
			if err != nil {
				return nil, cursor, err
			}
			if isResponse {
				return result, cursor, nil
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, hasColon := strings.Cut(line, ":")
		if !hasColon {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				eventID = value
				eventIDSet = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, errHTTPResponseTooLarge) {
			return nil, cursor, httpProtocolErrorCause("SSE response body exceeds size limit", err)
		}
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, cursor, httpProtocolError("SSE event line exceeds response limit")
		}
		return nil, cursor, err
	}
	return nil, cursor, errSSEResponseMissing
}

func parseSSEMessage(raw []byte, expectedID int64) (map[string]any, bool, error) {
	if err := validateJSONRPCObjectFields(raw, "SSE JSON-RPC message", "jsonrpc", "id", "method", "params", "result", "error"); err != nil {
		return nil, false, newRequestBoundaryError(fmt.Errorf("mcp: invalid SSE JSON-RPC message: %w", err), true, false)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope == nil {
		return nil, false, newRequestBoundaryError(fmt.Errorf("mcp: invalid SSE JSON-RPC message"), true, false)
	}
	var jsonrpc string
	if err := json.Unmarshal(envelope["jsonrpc"], &jsonrpc); err != nil || jsonrpc != "2.0" {
		return nil, false, newRequestBoundaryError(fmt.Errorf("mcp: invalid SSE JSON-RPC version"), true, false)
	}
	if methodRaw, hasMethod := envelope["method"]; hasMethod {
		if _, hasResult := envelope["result"]; hasResult {
			return nil, false, httpProtocolError("SSE JSON-RPC message has both method and result")
		}
		if _, hasError := envelope["error"]; hasError {
			return nil, false, httpProtocolError("SSE JSON-RPC message has both method and error")
		}
		var method string
		if err := json.Unmarshal(methodRaw, &method); err != nil || method == "" {
			return nil, false, httpProtocolError("SSE JSON-RPC method is invalid")
		}
		if _, hasID := envelope["id"]; hasID {
			return nil, false, httpProtocolError("server request in SSE stream is unsupported")
		}
		return nil, false, nil // A valid server notification precedes the response.
	}
	result, err := parseJSONRPCResponse(raw, expectedID)
	if err != nil {
		return nil, false, err
	}
	return result, true, nil
}

func (a *HTTPClientAdapter) resumeRequestResponse(ctx context.Context, endpoint, version, sessionID string, id int64, cursor string, budget *responseByteBudget) (map[string]any, error) {
	if !validLastEventID(cursor) {
		return nil, httpProtocolError("cannot resume SSE stream with an invalid event ID")
	}
	req, err := a.newHTTPMessageRequest(ctx, http.MethodGet, endpoint, nil, version, sessionID, "text/event-stream", cursor)
	if err != nil {
		return nil, err
	}
	resp, err := a.doHTTP(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: resume SSE stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, httpProtocolError("SSE resume GET returned HTTP status %d", resp.StatusCode)
	}
	if err := validateResponseSessionID(resp, sessionID); err != nil {
		return nil, err
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "text/event-stream") {
		return nil, httpProtocolError("SSE resume GET returned invalid Content-Type")
	}
	result, _, err := readSSEResponseWithBudget(resp.Body, id, budget)
	return result, err
}

func markRequestDispatched(err error) error {
	if err == nil || requestMayHaveBeenDispatched(err) {
		return err
	}
	var rpcErr *rpcResponseError
	if errors.As(err, &rpcErr) {
		return err
	}
	return newRequestBoundaryError(err, true, false)
}

// ProtocolVersion returns the protocol version negotiated during the
// initialize handshake, or "" before Connect.
func (a *HTTPClientAdapter) ProtocolVersion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.protocolVersion
}

// applyAuthHeaders applies only caller-provided headers and a preconfigured
// bearer token. OAuth negotiation is rejected before the adapter connects.
func (a *HTTPClientAdapter) applyAuthHeaders(req *http.Request) {
	for k, v := range a.cfg.Headers {
		req.Header.Set(k, v)
	}
	if a.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.cfg.BearerToken)
	}
}

// doInitializeLocked performs the 2025-06-18 initialize/initialized exchange.
// The caller must hold a.mu.
func (a *HTTPClientAdapter) doInitializeLocked(ctx context.Context) error {
	initParams := map[string]any{
		"protocolVersion": streamableHTTPProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "go-cli", "version": "1.0.0"},
	}
	id := a.nextID
	a.nextID++
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "initialize",
		"params":  initParams,
	})
	if err != nil {
		return fmt.Errorf("mcp: marshal initialize request: %w", err)
	}
	req, err := a.newHTTPMessageRequest(ctx, http.MethodPost, a.endpoint, body, "", "", "application/json, text/event-stream", "")
	if err != nil {
		return fmt.Errorf("mcp: build initialize request: %w", err)
	}
	resp, err := a.doHTTP(req)
	if err != nil {
		return fmt.Errorf("mcp: initialize request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return httpProtocolError("authentication required (HTTP 401)")
		}
		return httpProtocolError("initialize POST returned HTTP status %d", resp.StatusCode)
	}
	result, _, _, err := a.readRPCResponse(resp, id, newResponseByteBudget(maxHTTPResponseSize))
	if err != nil {
		return fmt.Errorf("mcp: invalid initialize response: %w", err)
	}
	sessionID, err := initializeSessionID(resp)
	if err != nil {
		return err
	}
	version, _ := result["protocolVersion"].(string)
	if version != streamableHTTPProtocolVersion {
		return httpProtocolError("server negotiated unsupported protocol version %q", version)
	}
	a.protocolVersion = version
	a.sessionID = sessionID

	notification, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if err != nil {
		return fmt.Errorf("mcp: marshal initialized notification: %w", err)
	}
	notifReq, err := a.newHTTPMessageRequest(ctx, http.MethodPost, a.endpoint, notification, version, sessionID, "application/json, text/event-stream", "")
	if err != nil {
		return fmt.Errorf("mcp: build initialized notification: %w", err)
	}
	notifResp, err := a.doHTTP(notifReq)
	if err != nil {
		return fmt.Errorf("mcp: send initialized notification: %w", err)
	}
	defer func() { _ = notifResp.Body.Close() }()
	if notifResp.StatusCode != http.StatusAccepted {
		return httpProtocolError("initialized notification returned HTTP status %d, want 202", notifResp.StatusCode)
	}
	if err := validateResponseSessionID(notifResp, sessionID); err != nil {
		return err
	}
	bodyBytes, err := readLimitedBody(notifResp.Body)
	if err != nil {
		return fmt.Errorf("mcp: read initialized notification acknowledgement: %w", err)
	}
	if len(bodyBytes) != 0 {
		return httpProtocolError("initialized notification acknowledgement must have an empty body")
	}

	slog.Info("mcp.http.initialized", "server", a.cfg.Name, "protocolVersion", version)
	return nil
}

func initializeSessionID(resp *http.Response) (string, error) {
	values, present := resp.Header[http.CanonicalHeaderKey("Mcp-Session-Id")]
	if !present {
		return "", nil
	}
	if len(values) != 1 || !validSessionID(values[0]) {
		return "", httpProtocolError("initialize response contained an invalid MCP session ID")
	}
	return values[0], nil
}
