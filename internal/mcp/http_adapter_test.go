package mcp

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
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

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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

func TestHTTPAdapterRejectsCrossOriginSSEEndpointBeforeCredentials(t *testing.T) {
	t.Parallel()
	headerCanary, err := generateOAuthState()
	require.NoError(t, err)
	bearerCanary, err := generateOAuthState()
	require.NoError(t, err)

	foreignRequests := make(chan http.Header, 1)
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(foreign.Close)

	configuredHeaders := make(chan http.Header, 1)
	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case configuredHeaders <- r.Header.Clone():
		default:
		}
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", foreign.URL)
	}))
	t.Cleanup(configured.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:        "srv",
		Transport:   MCPTransportSSE,
		URL:         configured.URL,
		Headers:     map[string]string{"X-MCP-Canary": headerCanary},
		BearerToken: bearerCanary,
	})
	err = adapter.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "origin")

	select {
	case got := <-configuredHeaders:
		assert.Equal(t, headerCanary, got.Get("X-MCP-Canary"))
		assert.Equal(t, "Bearer "+bearerCanary, got.Get("Authorization"))
	default:
		t.Fatal("configured MCP endpoint did not receive the handshake request")
	}
	select {
	case got := <-foreignRequests:
		t.Fatalf("foreign MCP endpoint received request with canary header %q and authorization %q", got.Get("X-MCP-Canary"), got.Get("Authorization"))
	default:
	}
}

func TestHTTPAdapterRejectsCrossOriginSSEEndpointWithOAuthToken(t *testing.T) {
	t.Parallel()
	headerCanary, err := generateOAuthState()
	require.NoError(t, err)
	oauthCanary, err := generateOAuthState()
	require.NoError(t, err)

	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(foreign.Close)

	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", foreign.URL)
	}))
	t.Cleanup(configured.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportSSE,
		URL:       configured.URL,
		Headers:   map[string]string{"X-MCP-Canary": headerCanary},
		OAuthConfig: &OAuthConfig{
			AuthorizationURL: "http://auth.example/authorize",
			TokenURL:         "http://auth.example/token",
		},
	})
	adapter.token = oauthCanary

	err = adapter.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "origin")
	assert.Zero(t, foreignRequests.Load(), "foreign MCP endpoint must receive no OAuth or header credentials")
}

func TestHTTPAdapterRejectsCrossOriginRedirectBeforeCredentials(t *testing.T) {
	t.Parallel()
	headerCanary, err := generateOAuthState()
	require.NoError(t, err)
	bearerCanary, err := generateOAuthState()
	require.NoError(t, err)

	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(foreign.Close)

	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(configured.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:        "srv",
		Transport:   MCPTransportSSE,
		URL:         configured.URL,
		Headers:     map[string]string{"X-MCP-Canary": headerCanary},
		BearerToken: bearerCanary,
	})
	err = adapter.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirect")
	assert.Zero(t, foreignRequests.Load(), "foreign redirect target must receive no request")
}

func TestHTTPAdapterAllowsSameOriginRedirectWithCredentials(t *testing.T) {
	t.Parallel()
	headerCanary, err := generateOAuthState()
	require.NoError(t, err)
	bearerCanary, err := generateOAuthState()
	require.NoError(t, err)

	var redirectedRequests atomic.Int32
	var missingCredentials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path != "/final" {
			http.NotFound(w, r)
			return
		}
		redirectedRequests.Add(1)
		if r.Header.Get("X-MCP-Canary") != headerCanary || r.Header.Get("Authorization") != "Bearer "+bearerCanary {
			missingCredentials.Add(1)
		}
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}

		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result": map[string]any{
				"protocolVersion": LatestProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "redirect-test", "version": "1.0"},
			},
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode redirected MCP response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:        "srv",
		Transport:   MCPTransportSSE,
		URL:         srv.URL + "/start",
		Headers:     map[string]string{"X-MCP-Canary": headerCanary},
		BearerToken: bearerCanary,
	})
	require.NoError(t, adapter.Connect(context.Background()))
	assert.GreaterOrEqual(t, redirectedRequests.Load(), int32(3), "GET, initialize, and initialized notification should reach the same-origin redirect target")
	assert.Zero(t, missingCredentials.Load(), "same-origin redirects must preserve configured credentials")
}

func TestHTTPAdapterCallToolLostResponseHasUnknownOutcome(t *testing.T) {
	t.Parallel()

	var sideEffects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Method == "tools/call" {
			sideEffects.Add(1)
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "HTTP hijacking unavailable", http.StatusInternalServerError)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}

		result := map[string]any{}
		if req.Method == "initialize" {
			result["protocolVersion"] = LatestProtocolVersion
			result["capabilities"] = map[string]any{}
			result["serverInfo"] = map[string]any{"name": "test", "version": "1.0"}
		}
		frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(frame); err != nil {
			t.Errorf("encode MCP response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       srv.URL,
	})
	require.NoError(t, adapter.Connect(context.Background()))

	result, err := adapter.CallTool(context.Background(), "write", map[string]any{"path": "file"})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "outcome unknown")
	assert.Contains(t, err.Error(), "may have executed")
	assertToolOutcomeUnknown(t, err)
	assert.EqualValues(t, 1, sideEffects.Load(), "the remote side effect completed once before its response was lost")
}

func TestHTTPAdapterCallToolUntrustedResponsesHaveUnknownOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		statusCode int
		response   func(id int64) []byte
	}{
		{
			name:       "malformed JSON",
			statusCode: http.StatusOK,
			response:   func(int64) []byte { return []byte("not-json") },
		},
		{
			name:       "wrong response ID",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"failed"}}`, id+1))
			},
		},
		{
			name:       "missing response ID",
			statusCode: http.StatusOK,
			response: func(int64) []byte {
				return []byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"failed"}}`)
			},
		},
		{
			name:       "null response ID",
			statusCode: http.StatusOK,
			response: func(int64) []byte {
				return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"failed"}}`)
			},
		},
		{
			name:       "duplicate response ID",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				// The final value matches the request ID, but the earlier value
				// makes the envelope ambiguous and must not be accepted.
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"id":%d,"result":{"content":[]}}`, id+1, id))
			},
		},
		{
			name:       "case alias response ID",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"ID":%d,"result":{"content":[]}}`, id+1, id))
			},
		},
		{
			name:       "case alias JSON-RPC version",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"1.0","JSONRPC":"2.0","id":%d,"result":{"content":[]}}`, id))
			},
		},
		{
			name:       "case alias result",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":null,"Result":{"content":[]}}`, id))
			},
		},
		{
			name:       "case alias error",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":null,"Error":{"code":-32000,"message":"failed"}}`, id))
			},
		},
		{
			name:       "duplicate result",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"content":[]},"result":{"content":[]}}`, id))
			},
		},
		{
			name:       "duplicate error",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"failed"},"error":{"code":-32000,"message":"failed again"}}`, id))
			},
		},
		{
			name:       "duplicate error code",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32001,"code":-32000,"message":"failed"}}`, id))
			},
		},
		{
			name:       "duplicate error message",
			statusCode: http.StatusOK,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"first","message":"failed"}}`, id))
			},
		},
		{
			name:       "error HTTP status",
			statusCode: http.StatusServiceUnavailable,
			response: func(id int64) []byte {
				return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":-32000,"message":"failed"}}`, id))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var sideEffects atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusOK)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				var req struct {
					ID     int64  `json:"id"`
					Method string `json:"method"`
				}
				if err := json.Unmarshal(body, &req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				if req.Method == "tools/call" {
					sideEffects.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.statusCode)
					_, _ = w.Write(tt.response(req.ID))
					return
				}

				result := map[string]any{}
				if req.Method == "initialize" {
					result["protocolVersion"] = LatestProtocolVersion
					result["capabilities"] = map[string]any{}
					result["serverInfo"] = map[string]any{"name": "test", "version": "1.0"}
				}
				frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(frame); err != nil {
					t.Errorf("encode MCP response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			adapter := NewHTTPClientAdapter(MCPServerConfig{
				Name:      "srv",
				Transport: MCPTransportStreamableHTTP,
				URL:       srv.URL,
			})
			require.NoError(t, adapter.Connect(context.Background()))

			result, err := adapter.CallTool(context.Background(), "write", map[string]any{"path": "file"})
			require.Error(t, err)
			assert.Nil(t, result)
			assert.Contains(t, err.Error(), "outcome unknown")
			assert.Contains(t, err.Error(), "may have executed")
			assertToolOutcomeUnknown(t, err)
			assert.EqualValues(t, 1, sideEffects.Load(), "the action must be counted once before its response is rejected")
		})
	}
}

func TestHTTPAdapterCallToolMatchingRPCErrorIsDefinite(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		frame := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if req.Method == "initialize" {
			frame["result"] = map[string]any{
				"protocolVersion": LatestProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
			}
		} else if req.Method == "tools/call" {
			frame["error"] = map[string]any{"code": -32000, "message": "tool rejected"}
		} else {
			frame["result"] = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(frame); err != nil {
			t.Errorf("encode MCP response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	adapter := newHTTPTestAdapter(srv)
	require.NoError(t, adapter.Connect(context.Background()))

	result, err := adapter.CallTool(context.Background(), "write", nil)
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "rpc error")
	assert.NotContains(t, err.Error(), "outcome unknown")
	var outcome OutcomeUnknown
	assert.False(t, errors.As(err, &outcome), "a trusted RPC error is not classified as unknown")
}

func TestHTTPAdapterCallToolCustomRoundTripperFailureIsUnknownAfterDispatch(t *testing.T) {
	t.Parallel()

	var sideEffects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result := map[string]any{}
		switch req.Method {
		case "initialize":
			result["protocolVersion"] = LatestProtocolVersion
			result["capabilities"] = map[string]any{}
			result["serverInfo"] = map[string]any{"name": "test", "version": "1.0"}
		case "notifications/initialized":
		case "tools/call":
			sideEffects.Add(1)
			result["content"] = "completed"
		}
		frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(frame); err != nil {
			t.Errorf("encode MCP response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if req.Method == http.MethodPost && req.GetBody != nil {
			body, err := req.GetBody()
			if err == nil {
				raw, readErr := io.ReadAll(body)
				_ = body.Close()
				if readErr == nil && strings.Contains(string(raw), `"method":"tools/call"`) {
					_ = resp.Body.Close()
					return nil, fmt.Errorf("connection reset by peer after dispatch")
				}
			}
		}
		return resp, nil
	})
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       srv.URL,
	}, &http.Client{Transport: transport})
	require.NoError(t, adapter.Connect(context.Background()))

	result, err := adapter.CallTool(context.Background(), "write", map[string]any{"path": "file"})
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "outcome unknown")
	assert.Contains(t, err.Error(), "may have executed")
	assertToolOutcomeUnknown(t, err)
	assert.EqualValues(t, 1, sideEffects.Load(), "the server executed the request before the RoundTripper failed")
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

// ---------------------------------------------------------------------------
// HTTP response size limit tests (HI-8)
// ---------------------------------------------------------------------------

// TestHTTPAdapterResponseSizeLimit verifies that responses exceeding the 10MB
// limit are rejected with an error instead of consuming unbounded memory.
func TestHTTPAdapterResponseSizeLimit(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
				"serverInfo":      map[string]any{"name": "test", "version": "1.0"},
			}
		case "notifications/initialized":
			result = map[string]any{}
		case "tools/call":
			// Return a response larger than 10MB to trigger the size limit.
			big := strings.Repeat("x", 11*1024*1024)
			result = map[string]any{"content": big}
		default:
			http.Error(w, "unsupported", http.StatusBadRequest)
			return
		}

		frame := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(frame)
	}))
	t.Cleanup(srv.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       srv.URL,
	})

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))

	_, err := adapter.CallTool(ctx, "echo", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

// TestReadLimitedBody verifies the readLimitedBody helper enforces the size
// limit correctly.
func TestReadLimitedBody(t *testing.T) {
	t.Parallel()

	// Under the limit: returns the data.
	small := strings.NewReader("hello")
	raw, err := readLimitedBody(small)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(raw))

	// Over the limit: returns an error.
	big := strings.NewReader(strings.Repeat("x", maxHTTPResponseSize+100))
	_, err = readLimitedBody(big)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")

	// Exactly at the limit: succeeds.
	exact := strings.NewReader(strings.Repeat("x", maxHTTPResponseSize))
	raw, err = readLimitedBody(exact)
	require.NoError(t, err)
	assert.Len(t, raw, maxHTTPResponseSize)
}

// ---------------------------------------------------------------------------
// SSRF protection tests (SEC-4 / SEC-8)
// ---------------------------------------------------------------------------

// TestMCPAdapter_SSRSafeClient verifies that the MCP HTTPClientAdapter uses an
// SSRF-safe HTTP client: requests to internal IP ranges are blocked at dial
// time (AC-2), and DNS-rebinding attacks are caught by the dial-time Control
// even when a listener is reachable on a loopback address (AC-4).
func TestMCPAdapter_SSRSafeClient(t *testing.T) {
	t.Parallel()

	// --- Internal IP ranges are blocked ---
	// Connect swallows GET failures (it falls back to direct POST), so the
	// SSRF block surfaces on the first POST (ListTools).
	for _, endpoint := range []string{
		"http://10.0.0.1/mcp",        // RFC 1918
		"http://169.254.169.254/mcp", // cloud metadata / link-local
		"http://192.168.1.1/mcp",     // RFC 1918
		"http://172.16.0.1/mcp",      // RFC 1918
	} {
		adapter := NewHTTPClientAdapter(MCPServerConfig{
			Name:      "internal",
			Transport: MCPTransportStreamableHTTP,
			URL:       endpoint,
		})
		_ = adapter.Connect(context.Background())
		_, err := adapter.ListTools(context.Background())
		require.Error(t, err, "endpoint %q should be blocked", endpoint)
		assert.ErrorIs(t, err, tools.ErrPrivateIP, "endpoint %q", endpoint)
	}

	// --- DNS rebinding: dial-time Control blocks even a reachable listener ---
	// A listener on 127.0.0.1 simulates the destination a rebinding attack
	// would redirect to at dial time. NewSSRFSafeHTTPClient (non-loopback)
	// refuses the connection before it is established.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
	}()

	addr := ln.Addr().String()
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name: "rebinding",
		URL:  "http://" + addr + "/mcp",
	}, tools.NewSSRFSafeHTTPClient(2*time.Second))
	_ = adapter.Connect(context.Background())
	_, err = adapter.ListTools(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, tools.ErrPrivateIP)
}

// TestMCPOAuth_TokenEndpointSSRF verifies that the OAuth token exchange uses
// the SSRF-safe client: when TokenURL points to an internal IP, the request is
// blocked at dial time and the error wraps tools.ErrPrivateIP.
func TestMCPOAuth_TokenEndpointSSRF(t *testing.T) {
	t.Parallel()

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name: "oauth-ssrf",
		OAuthConfig: &OAuthConfig{
			AuthorizationURL: "https://auth.example.com/authorize",
			TokenURL:         "http://10.0.0.1/token", // internal IP
			ClientID:         "test-client",
		},
	})

	err := adapter.exchangeCodeForToken(
		context.Background(),
		adapter.cfg.OAuthConfig,
		"fake-code",
		"http://127.0.0.1/callback",
	)
	require.Error(t, err)
	assert.ErrorIs(t, err, tools.ErrPrivateIP)
}
