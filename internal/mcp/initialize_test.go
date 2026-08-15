package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// AC-1: After Connect, send initialize request first and validate
//       returned protocolVersion
// ---------------------------------------------------------------------------

// TestInitializeHandshakeStdio verifies that after Connect the negotiated
// protocol version is available via ProtocolVersion().
func TestInitializeHandshakeStdio(t *testing.T) {
	t.Parallel()
	serverConn, conn := fakeMCPConn(t)
	defer closeConn(serverConn)

	adapter := NewOfficialSDKAdapter(
		MCPServerConfig{Name: "srv", Transport: MCPTransportStdio},
		WithConnection(conn),
	)
	require.NoError(t, adapter.Connect(context.Background()))
	assert.Equal(t, LatestProtocolVersion, adapter.ProtocolVersion())
}

// TestInitializeRejectsUnsupportedVersion verifies that Connect fails when
// the server returns a protocol version the client does not support.
func TestInitializeRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()
	serverConn, clientConn := net.Pipe()
	defer closeConn(serverConn)

	go func() {
		sc := bufio.NewScanner(serverConn)
		for sc.Scan() {
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			var result map[string]any
			switch req.Method {
			case "initialize":
				result = map[string]any{
					"protocolVersion": "1999-01-01",
					"capabilities":    map[string]any{},
				}
			default:
				result = map[string]any{}
			}
			frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
			b, _ := json.Marshal(frame)
			fmt.Fprintf(serverConn, "%s\n", b)
		}
		closeConn(serverConn)
	}()

	conn := NewJSONRPCLineTransport(clientConn, clientConn, clientConn.Close)
	adapter := NewOfficialSDKAdapter(
		MCPServerConfig{Name: "srv", Transport: MCPTransportStdio},
		WithConnection(conn),
	)
	err := adapter.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported protocol version")
}

// TestInitializeSentFirst verifies that initialize is the first method sent
// during Connect and that notifications/initialized follows it.
func TestInitializeSentFirst(t *testing.T) {
	t.Parallel()
	serverConn, clientConn := net.Pipe()
	defer closeConn(serverConn)

	var mu sync.Mutex
	var methods []string

	go func() {
		sc := bufio.NewScanner(serverConn)
		for sc.Scan() {
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}

			mu.Lock()
			methods = append(methods, req.Method)
			mu.Unlock()

			// Skip JSON-RPC notifications (no id field) — they must not receive a response.
			var raw map[string]json.RawMessage
			if json.Unmarshal(sc.Bytes(), &raw) == nil {
				if _, hasID := raw["id"]; !hasID {
					continue
				}
			}

			var result map[string]any
			switch req.Method {
			case "initialize":
				result = map[string]any{
					"protocolVersion": LatestProtocolVersion,
					"capabilities":    map[string]any{},
				}
			default:
				result = map[string]any{}
			}
			frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
			b, _ := json.Marshal(frame)
			fmt.Fprintf(serverConn, "%s\n", b)
		}
		closeConn(serverConn)
	}()

	conn := NewJSONRPCLineTransport(clientConn, clientConn, clientConn.Close)
	adapter := NewOfficialSDKAdapter(
		MCPServerConfig{Name: "srv", Transport: MCPTransportStdio},
		WithConnection(conn),
	)
	require.NoError(t, adapter.Connect(context.Background()))

	// Trigger another request to verify the full method order.
	_, _ = adapter.ListTools(context.Background())

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(methods), 3, "expected initialize, notifications/initialized, and tools/list")
	assert.Equal(t, "initialize", methods[0], "initialize must be sent first")
	assert.Equal(t, "notifications/initialized", methods[1], "notifications/initialized must follow initialize")
	assert.Equal(t, "tools/list", methods[2])
}

// TestHTTPInitializeHandshake verifies the HTTP transport also performs the
// initialize handshake and exposes the negotiated protocol version.
func TestHTTPInitializeHandshake(t *testing.T) {
	t.Parallel()
	srv := newHTTPMCPServer(t, http.StatusOK, false)
	adapter := newHTTPTestAdapter(srv)
	require.NoError(t, adapter.Connect(context.Background()))
	assert.Equal(t, LatestProtocolVersion, adapter.ProtocolVersion())
}

// ---------------------------------------------------------------------------
// AC-2: Support 2024-11-05 / 2025-03-26 / 2025-06-18 multi-version
//       negotiation
// ---------------------------------------------------------------------------

// TestIsSupportedProtocolVersion is a unit test for the version helper.
func TestIsSupportedProtocolVersion(t *testing.T) {
	t.Parallel()
	for _, v := range SupportedProtocolVersions {
		assert.True(t, IsSupportedProtocolVersion(v), "version %s should be supported", v)
	}
	assert.False(t, IsSupportedProtocolVersion("1999-01-01"))
	assert.False(t, IsSupportedProtocolVersion(""))
}

// TestInitializeMultiVersionNegotiation verifies that the client accepts each
// of the supported protocol versions returned by the server.
func TestInitializeMultiVersionNegotiation(t *testing.T) {
	t.Parallel()
	for _, version := range SupportedProtocolVersions {
		version := version
		t.Run(version, func(t *testing.T) {
			t.Parallel()

			serverConn, clientConn := net.Pipe()
			defer closeConn(serverConn)

			go func() {
				sc := bufio.NewScanner(serverConn)
				for sc.Scan() {
					var req struct {
						ID     int64  `json:"id"`
						Method string `json:"method"`
					}
					if json.Unmarshal(sc.Bytes(), &req) != nil {
						continue
					}

					// Skip JSON-RPC notifications (no id field) — they must not receive a response.
					var raw map[string]json.RawMessage
					if json.Unmarshal(sc.Bytes(), &raw) == nil {
						if _, hasID := raw["id"]; !hasID {
							continue
						}
					}

					var result map[string]any
					switch req.Method {
					case "initialize":
						result = map[string]any{
							"protocolVersion": version,
							"capabilities":    map[string]any{},
						}
					default:
						result = map[string]any{}
					}
					frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
					b, _ := json.Marshal(frame)
					fmt.Fprintf(serverConn, "%s\n", b)
				}
				closeConn(serverConn)
			}()

			conn := NewJSONRPCLineTransport(clientConn, clientConn, clientConn.Close)
			adapter := NewOfficialSDKAdapter(
				MCPServerConfig{Name: "srv", Transport: MCPTransportStdio},
				WithConnection(conn),
			)
			require.NoError(t, adapter.Connect(context.Background()))
			assert.Equal(t, version, adapter.ProtocolVersion())
		})
	}
}

// ---------------------------------------------------------------------------
// AC-3: HTTP transport supports Authorization header / Bearer token
// ---------------------------------------------------------------------------

// httpMCPServerWithAuthCheck starts an httptest MCP server that records the
// Authorization header and custom headers from POST requests. The returned
// cleanup function closes the server.
func httpMCPServerWithAuthCheck(t *testing.T, mu *sync.Mutex, seenAuth *string, seenHeaders map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			if seenAuth != nil && *seenAuth == "" {
				*seenAuth = r.Header.Get("Authorization")
			}
			if seenHeaders != nil {
				for k := range seenHeaders {
					seenHeaders[k] = r.Header.Get(k)
				}
			}
			mu.Unlock()
		}

		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}

		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		var result map[string]any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": LatestProtocolVersion,
				"capabilities":    map[string]any{},
			}
		default:
			result = map[string]any{}
		}
		frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		b, _ := json.Marshal(frame)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHTTPBearerTokenSent verifies that the BearerToken from config is sent
// as the Authorization header on HTTP POST requests.
func TestHTTPBearerTokenSent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seenAuth string

	srv := httpMCPServerWithAuthCheck(t, &mu, &seenAuth, nil)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:        "srv",
		Transport:   MCPTransportStreamableHTTP,
		URL:         srv.URL,
		BearerToken: "my-secret-token",
	})
	require.NoError(t, adapter.Connect(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "Bearer my-secret-token", seenAuth)
}

// TestHTTPCustomHeadersSent verifies that custom headers from the config are
// sent on HTTP requests.
func TestHTTPCustomHeadersSent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	seenHeaders := map[string]string{
		"X-Custom-1": "",
		"X-Custom-2": "",
	}

	srv := httpMCPServerWithAuthCheck(t, &mu, nil, seenHeaders)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       srv.URL,
		Headers: map[string]string{
			"X-Custom-1": "value1",
			"X-Custom-2": "value2",
		},
	})
	require.NoError(t, adapter.Connect(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "value1", seenHeaders["X-Custom-1"])
	assert.Equal(t, "value2", seenHeaders["X-Custom-2"])
}

// ---------------------------------------------------------------------------
// AC-4: 401 + WWW-Authenticate triggers OAuth 2.1 authorization code flow
// ---------------------------------------------------------------------------

// TestHTTP401WithoutOAuthReturnsError verifies that a 401 response without an
// OAuthConfig results in an authentication error.
func TestHTTP401WithoutOAuthReturnsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       srv.URL,
	})
	err := adapter.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authentication required")
}

// TestHTTPOAuthFlow verifies the full OAuth 2.1 authorization code flow:
// 401 → start callback server → authorize → exchange code → retry with token.
func TestHTTPOAuthFlow(t *testing.T) {
	// NOT t.Parallel() — captures os.Stderr to extract the authorization URL.

	// Mock token endpoint.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"test-token","token_type":"Bearer"}`))
	}))
	t.Cleanup(tokenSrv.Close)

	// Mock authorization endpoint: redirects to the callback URL with a code.
	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectURI := r.URL.Query().Get("redirect_uri")
		if redirectURI == "" {
			http.Error(w, "missing redirect_uri", http.StatusBadRequest)
			return
		}
		// Forward the state parameter to satisfy CSRF verification on the callback.
		state := r.URL.Query().Get("state")
		redirectURL := redirectURI + "?code=fake-auth-code"
		if state != "" {
			redirectURL += "&state=" + url.QueryEscape(state)
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
	}))
	t.Cleanup(authSrv.Close)

	// Mock MCP server: first initialize returns 401, second succeeds with token.
	var initCount int32
	mcpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}

		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)

		if req.Method == "initialize" {
			count := atomic.AddInt32(&initCount, 1)
			if count == 1 {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// Second attempt: verify the Bearer token is present.
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			result := map[string]any{
				"protocolVersion": LatestProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
			}
			frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
			b, _ := json.Marshal(frame)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
			return
		}

		// Handle notifications/initialized and other methods.
		frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}}
		b, _ := json.Marshal(frame)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(mcpSrv.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       mcpSrv.URL,
		OAuthConfig: &OAuthConfig{
			AuthorizationURL: authSrv.URL,
			TokenURL:         tokenSrv.URL,
			ClientID:         "test-client",
		},
	})

	// Capture stderr to extract the authorization URL printed by doOAuthFlow.
	oldStderr := os.Stderr
	rPipe, wPipe, _ := os.Pipe()
	os.Stderr = wPipe
	t.Cleanup(func() {
		os.Stderr = oldStderr
		_ = wPipe.Close()
		_ = rPipe.Close()
	})

	// Goroutine simulates the browser: reads the auth URL from stderr and
	// follows it, causing the auth server to redirect to the local callback.
	go func() {
		scanner := bufio.NewScanner(rPipe)
		for scanner.Scan() {
			line := scanner.Text()
			if idx := strings.Index(line, "http://"); idx >= 0 {
				authURL := strings.TrimSpace(line[idx:])
				resp, err := http.Get(authURL)
				if err == nil {
					_ = resp.Body.Close()
				}
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, adapter.Connect(ctx))
	assert.Equal(t, "test-token", adapter.token)
	assert.Equal(t, LatestProtocolVersion, adapter.ProtocolVersion())
}
