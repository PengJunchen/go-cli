package mcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
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

	protocolVersion string
	token           string // OAuth/bearer token obtained after authentication
	oauthState      string // CSRF state parameter for the current OAuth flow
}

var _ MCPClient = (*HTTPClientAdapter)(nil)

// defaultHTTPTimeout bounds individual HTTP requests. Network fetch
// operations (e.g. web search) can legitimately take several seconds, but
// without a cap a hung server would block the agent loop forever.
const defaultHTTPTimeout = 60 * time.Second

// maxHTTPResponseSize caps the size of an HTTP response body read by the
// adapter. This prevents a malicious or buggy server from exhausting memory
// by streaming an arbitrarily large response.
const maxHTTPResponseSize = 10 * 1024 * 1024 // 10 MB

// readLimitedBody reads at most maxHTTPResponseSize bytes from r. If the body
// exceeds the limit, an error is returned so the caller can abort instead of
// allocating unbounded memory.
func readLimitedBody(r io.Reader) ([]byte, error) {
	limited := io.LimitReader(r, maxHTTPResponseSize+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxHTTPResponseSize {
		return nil, fmt.Errorf("mcp: response body exceeds %d byte limit", maxHTTPResponseSize)
	}
	return raw, nil
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
// is nil, a loopback-allowing SSRF-safe client is used so that OAuth token
// exchange and all other requests are protected against internal-IP targeting.
func NewHTTPClientAdapterWithClient(cfg MCPServerConfig, client *http.Client) *HTTPClientAdapter {
	if client == nil {
		client = tools.NewSSRFSafeHTTPClientAllowLoopback(defaultHTTPTimeout)
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
	a.applyAuthHeaders(req)

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

	if err := a.doInitializeLocked(ctx); err != nil {
		a.connected = false
		return fmt.Errorf("mcp: initialize handshake: %w", err)
	}

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
	endpoint := a.endpoint
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

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("mcp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	a.applyAuthHeaders(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	raw, err := readLimitedBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("mcp: read response: %w", err)
	}
	return parseHTTPResponse(raw)
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

// parseHTTPResponse parses the raw HTTP response body into a JSON-RPC result
// map. It handles both plain JSON and SSE-formatted responses (where the
// JSON-RPC payload is wrapped in data: lines).
func parseHTTPResponse(raw []byte) (map[string]any, error) {
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

// ProtocolVersion returns the protocol version negotiated during the
// initialize handshake, or "" before Connect.
func (a *HTTPClientAdapter) ProtocolVersion() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.protocolVersion
}

// applyAuthHeaders sets custom headers and the Bearer token Authorization
// header on the given request. The Bearer token from config takes precedence
// over any OAuth-obtained token.
func (a *HTTPClientAdapter) applyAuthHeaders(req *http.Request) {
	for k, v := range a.cfg.Headers {
		req.Header.Set(k, v)
	}
	token := a.cfg.BearerToken
	if token == "" && a.token != "" {
		token = a.token
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// doPost sends a JSON-RPC POST request and returns the raw HTTP response. The
// caller is responsible for closing the response body. The caller must hold
// a.mu (this method is called from doInitializeLocked which runs under the
// Connect lock).
func (a *HTTPClientAdapter) doPost(ctx context.Context, method string, params map[string]any) (*http.Response, error) {
	id := a.nextID
	a.nextID++
	endpoint := a.endpoint

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("mcp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	a.applyAuthHeaders(req)

	return a.client.Do(req)
}

// doInitializeLocked performs the MCP initialize/initialized handshake over
// HTTP. The caller must hold a.mu. If the server responds with 401 and an
// OAuthConfig is set, it triggers the OAuth 2.1 authorization code flow and
// retries the initialize request with the obtained token.
func (a *HTTPClientAdapter) doInitializeLocked(ctx context.Context) error {
	initParams := map[string]any{
		"protocolVersion": LatestProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "go-cli", "version": "1.0.0"},
	}

	resp, err := a.doPost(ctx, "initialize", initParams)
	if err != nil {
		return fmt.Errorf("mcp: initialize request: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()

		if a.cfg.OAuthConfig != nil {
			if err := a.doOAuthFlow(ctx, wwwAuth); err != nil {
				return fmt.Errorf("mcp: oauth flow: %w", err)
			}
			resp, err = a.doPost(ctx, "initialize", initParams)
			if err != nil {
				return fmt.Errorf("mcp: initialize retry: %w", err)
			}
		} else {
			return fmt.Errorf("mcp: authentication required (401 Unauthorized)")
		}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := readLimitedBody(resp.Body)
	if err != nil {
		return fmt.Errorf("mcp: read response: %w", err)
	}
	result, err := parseHTTPResponse(raw)
	if err != nil {
		return err
	}

	version, _ := result["protocolVersion"].(string)
	if !IsSupportedProtocolVersion(version) {
		return fmt.Errorf("unsupported protocol version: %s", version)
	}
	a.protocolVersion = version

	// Send notifications/initialized (best-effort).
	if notifResp, nErr := a.doPost(ctx, "notifications/initialized", map[string]any{}); nErr == nil {
		_ = notifResp.Body.Close()
	}

	slog.Info("mcp.http.initialized", "server", a.cfg.Name, "protocolVersion", version)
	return nil
}

// generateOAuthState generates a cryptographically random 32-byte hex-encoded
// string used as the OAuth state parameter to prevent CSRF attacks on the
// authorization code flow.
func generateOAuthState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// doOAuthFlow performs the OAuth 2.1 authorization code flow: starts a local
// callback server, opens the user's browser to the authorization URL, receives
// the authorization code, and exchanges it for an access token. The token is
// stored in a.token and used for subsequent requests. A random state parameter
// is included in the authorization URL and verified on callback to prevent
// CSRF attacks.
func (a *HTTPClientAdapter) doOAuthFlow(ctx context.Context, _ string) error {
	oauth := a.cfg.OAuthConfig
	if oauth == nil {
		return fmt.Errorf("mcp: no OAuth config")
	}

	// Generate and store the CSRF state parameter.
	state, err := generateOAuthState()
	if err != nil {
		return fmt.Errorf("mcp: generate oauth state: %w", err)
	}
	a.oauthState = state

	// Start a local callback server to receive the authorization code.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("mcp: start callback listener: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port //nolint:errcheck // listener is always *net.TCPAddr
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	codeCh := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		callbackState := r.URL.Query().Get("state")
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)                  //nolint:errcheck
			_, _ = w.Write([]byte("Missing authorization code.")) //nolint:errcheck
			return
		}
		// Verify the CSRF state parameter to prevent cross-site request
		// forgery attacks on the authorization code flow.
		if callbackState == "" || callbackState != state {
			w.WriteHeader(http.StatusBadRequest)               //nolint:errcheck
			_, _ = w.Write([]byte("Invalid state parameter.")) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusOK)                                                 //nolint:errcheck
		_, _ = w.Write([]byte("Authorization complete. You may close this window.")) //nolint:errcheck
		select {
		case codeCh <- code:
		default:
		}
	})}
	go func() { _ = srv.Serve(listener) }()
	defer func() { _ = srv.Shutdown(ctx) }()

	// Build the authorization URL.
	authURL, err := url.Parse(oauth.AuthorizationURL)
	if err != nil {
		return fmt.Errorf("mcp: parse authorization URL: %w", err)
	}
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", oauth.ClientID)
	q.Set("redirect_uri", redirectURL)
	q.Set("state", state)
	if len(oauth.Scopes) > 0 {
		q.Set("scope", strings.Join(oauth.Scopes, " "))
	}
	authURL.RawQuery = q.Encode()

	slog.Info("mcp.oauth.authorize", "url", authURL.String(), "server", a.cfg.Name)
	fmt.Fprintf(os.Stderr, "\nMCP server requires authentication. Open this URL to authorize:\n  %s\n\n", authURL.String()) //nolint:errcheck

	// Wait for the callback.
	select {
	case code := <-codeCh:
		return a.exchangeCodeForToken(ctx, oauth, code, redirectURL)
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("mcp: oauth flow timed out waiting for authorization")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// exchangeCodeForToken exchanges the authorization code for an access token
// via the token endpoint.
func (a *HTTPClientAdapter) exchangeCodeForToken(ctx context.Context, oauth *OAuthConfig, code, redirectURL string) error {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURL)
	form.Set("client_id", oauth.ClientID)
	if oauth.ClientSecret != "" {
		form.Set("client_secret", oauth.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauth.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("mcp: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := readLimitedBody(resp.Body)
		return fmt.Errorf("mcp: token exchange failed (status %d): %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxHTTPResponseSize)).Decode(&tokenResp); err != nil {
		return fmt.Errorf("mcp: decode token response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return fmt.Errorf("mcp: token response missing access_token")
	}
	a.token = tokenResp.AccessToken
	slog.Info("mcp.oauth.token_obtained", "server", a.cfg.Name)
	return nil
}
