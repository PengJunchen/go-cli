package mcp

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// extractSSEData unit tests
// ---------------------------------------------------------------------------

func TestExtractSSEData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single data line",
			input: "id:1\nevent:message\ndata:{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n",
			want:  "{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}",
		},
		{
			name:  "multiple data lines concatenated",
			input: "data:{\"a\":1,\ndata:\"b\":2}\n\n",
			want:  "{\"a\":1,\"b\":2}",
		},
		{
			name:  "empty data lines skipped",
			input: "data:\ndata:{\"ok\":true}\ndata:\n\n",
			want:  "{\"ok\":true}",
		},
		{
			name:  "CRLF line endings",
			input: "id:1\r\nevent:message\r\ndata:{\"jsonrpc\":\"2.0\"}\r\n\r\n",
			want:  "{\"jsonrpc\":\"2.0\"}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, string(extractSSEData([]byte(tc.input))))
		})
	}
}

func TestExtractSSEDataPlainJSONPassedThrough(t *testing.T) {
	t.Parallel()
	// Plain JSON contains no "data:" lines, so the fallback returns the raw
	// input unchanged. In production request() never calls extractSSEData for
	// this case (it short-circuits when raw[0] == '{').
	in := []byte(`{"jsonrpc":"2.0","id":0,"result":{"tools":[{"name":"echo"}]}}`)
	assert.Equal(t, string(in), string(extractSSEData(in)))
}

// ---------------------------------------------------------------------------
// truncate unit tests
// ---------------------------------------------------------------------------

func TestTruncate(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "echo", truncate("echo", 100), "short strings are returned as-is")
	assert.Equal(t, "hello...", truncate("hello world", 5), "long strings are truncated with ellipsis")
	assert.Equal(t, "exact", truncate("exact", 5), "strings at the limit are returned as-is")
	assert.Equal(t, "", truncate("", 5), "empty strings are returned as-is")
}

// ---------------------------------------------------------------------------
// httptest helpers for HTTPClientAdapter integration tests
// ---------------------------------------------------------------------------

// newHTTPMCPServer starts an httptest server speaking JSON-RPC over HTTP.
// GET (the Connect handshake) always returns getStatus with an empty body;
// POST responds to tools/list and tools/call, either as plain JSON or wrapped
// in an SSE frame depending on sse.
func newHTTPMCPServer(t *testing.T, getStatus int, sse bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(getStatus)
		case http.MethodPost:
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if unmarshalErr := json.Unmarshal(body, &req); unmarshalErr != nil {
				http.Error(w, unmarshalErr.Error(), http.StatusBadRequest)
				return
			}

			var result map[string]any
			switch req.Method {
			case "initialize":
				result = map[string]any{
					"protocolVersion": LatestProtocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
				}
			case "notifications/initialized":
				result = map[string]any{}
			case "tools/list":
				result = map[string]any{
					"tools": []map[string]any{
						{"name": "echo", "description": "echoes input back"},
					},
				}
			case "tools/call":
				result = map[string]any{"content": "hello"}
			default:
				http.Error(w, "unsupported method", http.StatusBadRequest)
				return
			}

			frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
			b, err := json.Marshal(frame)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			if sse {
				w.Header().Set("Content-Type", "text/event-stream")
				if _, err := fmt.Fprintf(w, "id:1\nevent:message\ndata:%s\n\n", b); err != nil {
					return
				}
			} else {
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write(b); err != nil {
					return
				}
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newHTTPTestAdapter(srv *httptest.Server) *HTTPClientAdapter {
	return NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       srv.URL,
	})
}

// ---------------------------------------------------------------------------
// HTTPClientAdapter integration tests
// ---------------------------------------------------------------------------

func TestHTTPAdapterConnectStreamableHTTP(t *testing.T) {
	t.Parallel()
	// A streamable HTTP server never emits the SSE "event: endpoint" frame on
	// the initial GET; Connect must still succeed and fall back to the URL as
	// the direct POST endpoint.
	srv := newHTTPMCPServer(t, http.StatusOK, false)
	adapter := newHTTPTestAdapter(srv)

	require.NoError(t, adapter.Connect(context.Background()))
	assert.Equal(t, srv.URL, adapter.endpoint)
	assert.True(t, adapter.connected)
}

func TestHTTPAdapterConnectGETReturns400(t *testing.T) {
	t.Parallel()
	// Streamable HTTP servers may reject the GET handshake outright; Connect
	// must still succeed and use the URL for direct POST.
	srv := newHTTPMCPServer(t, http.StatusBadRequest, false)
	adapter := newHTTPTestAdapter(srv)

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))
	assert.Equal(t, srv.URL, adapter.endpoint)

	// The direct-POST fallback must be fully functional.
	tools, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0].Name)
}

func TestHTTPAdapterListTools(t *testing.T) {
	t.Parallel()
	srv := newHTTPMCPServer(t, http.StatusOK, false)
	adapter := newHTTPTestAdapter(srv)

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))

	tools, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0].Name)
	assert.Equal(t, "echoes input back", tools[0].Description)
}

func TestHTTPAdapterListToolsSSE(t *testing.T) {
	t.Parallel()
	// The server wraps the JSON-RPC payload in an SSE frame:
	//   id:1 / event:message / data:{...}
	srv := newHTTPMCPServer(t, http.StatusOK, true)
	adapter := newHTTPTestAdapter(srv)

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))

	tools, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0].Name)
	assert.Equal(t, "echoes input back", tools[0].Description)
}

func TestHTTPAdapterCallTool(t *testing.T) {
	t.Parallel()
	srv := newHTTPMCPServer(t, http.StatusOK, false)
	adapter := newHTTPTestAdapter(srv)

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))

	result, err := adapter.CallTool(ctx, "echo", map[string]any{"msg": "hi"})
	require.NoError(t, err)
	assert.Equal(t, "hello", result.Content)
	assert.False(t, result.IsError)
}

func TestHTTPAdapterCallToolSSE(t *testing.T) {
	t.Parallel()
	srv := newHTTPMCPServer(t, http.StatusOK, true)
	adapter := newHTTPTestAdapter(srv)

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))

	result, err := adapter.CallTool(ctx, "echo", map[string]any{"msg": "hi"})
	require.NoError(t, err)
	assert.Equal(t, "hello", result.Content)
	assert.False(t, result.IsError)
}

// ---------------------------------------------------------------------------
// OAuth CSRF state parameter tests
// ---------------------------------------------------------------------------

// TestGenerateOAuthState verifies that generateOAuthState returns a
// 64-character hex-encoded string (32 bytes of cryptographic randomness).
func TestGenerateOAuthState(t *testing.T) {
	t.Parallel()
	state, err := generateOAuthState()
	require.NoError(t, err)
	assert.Len(t, state, 64, "state should be 32 bytes hex-encoded (64 chars)")
	decoded, err := hex.DecodeString(state)
	require.NoError(t, err, "state should be valid hex")
	assert.Len(t, decoded, 32, "decoded state should be 32 bytes")
}

// TestGenerateOAuthStateUniqueness verifies that successive calls to
// generateOAuthState produce different values.
func TestGenerateOAuthStateUniqueness(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		s, err := generateOAuthState()
		require.NoError(t, err)
		assert.False(t, seen[s], "state %q already generated", s)
		seen[s] = true
	}
}

// TestOAuthFlowStateParameter verifies that:
//  1. The state parameter is present in the authorization URL and matches the
//     value stored on the adapter.
//  2. A callback with a missing state parameter is rejected (HTTP 400).
//  3. A callback with a mismatched state parameter is rejected (HTTP 400).
//  4. A callback with the correct state parameter succeeds (HTTP 200) and the
//     OAuth flow completes with a valid access token.
//
// This test does NOT run in parallel because it temporarily replaces
// os.Stderr to capture the printed authorization URL.
func TestOAuthFlowStateParameter(t *testing.T) {
	// Token endpoint that returns a valid access token.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": "token-xyz",
			"token_type":   "Bearer",
		})
	}))
	t.Cleanup(tokenSrv.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name: "oauth-srv",
		OAuthConfig: &OAuthConfig{
			AuthorizationURL: "https://auth.example.com/authorize",
			TokenURL:         tokenSrv.URL,
			ClientID:         "test-client",
		},
	})

	// Capture stderr to extract the printed authorization URL.
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- adapter.doOAuthFlow(ctx, "")
	}()

	// Read the authorization URL from the captured stderr.
	scanner := bufio.NewScanner(r)
	var authURLStr string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "http") {
			authURLStr = line
			break
		}
	}
	os.Stderr = oldStderr
	_ = w.Close()

	require.NotEmpty(t, authURLStr, "authorization URL should have been printed to stderr")

	authURL, err := url.Parse(authURLStr)
	require.NoError(t, err)

	// Verify the state parameter is present in the authorization URL.
	state := authURL.Query().Get("state")
	require.NotEmpty(t, state, "state parameter must be present in the authorization URL")
	assert.Equal(t, adapter.oauthState, state, "stored state should match URL state")

	// Verify the redirect_uri (callback URL) is present.
	redirectURI := authURL.Query().Get("redirect_uri")
	require.NotEmpty(t, redirectURI, "redirect_uri must be present in the authorization URL")

	// Callback with missing state parameter must be rejected.
	resp, err := http.Get(redirectURI + "?code=test-code")
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	_ = resp.Body.Close()

	// Callback with mismatched state parameter must be rejected.
	resp, err = http.Get(redirectURI + "?code=test-code&state=wrong-state")
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	_ = resp.Body.Close()

	// Callback with the correct state parameter must succeed.
	resp, err = http.Get(redirectURI + "?code=test-code&state=" + url.QueryEscape(state))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	// The OAuth flow should complete successfully with the token.
	err = <-errCh
	require.NoError(t, err)
	assert.Equal(t, "token-xyz", adapter.token)
}
