package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// ---------------------------------------------------------------------------
// Streamable HTTP SSE envelope tests
// ---------------------------------------------------------------------------

func TestReadSSEResponseParsesEachEvent(t *testing.T) {
	t.Parallel()
	input := "id:progress-1\nevent:message\ndata:{\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n" +
		"id:response-2\nevent:message\ndata:{\"jsonrpc\":\"2.0\",\ndata:\"id\":7,\"result\":{\"ok\":true}}\n\n"
	result, cursor, err := readSSEResponse(strings.NewReader(input), 7)
	require.NoError(t, err)
	assert.Equal(t, "response-2", cursor)
	assert.Equal(t, true, result["ok"])
}

func TestSameHTTPOriginNormalizesHostAndEffectivePort(t *testing.T) {
	t.Parallel()
	parse := func(raw string) *url.URL {
		t.Helper()
		parsed, err := url.Parse(raw)
		require.NoError(t, err)
		return parsed
	}

	assert.True(t, sameHTTPOrigin(parse("http://example.test"), parse("HTTP://EXAMPLE.TEST:080/path")))
	assert.True(t, sameHTTPOrigin(parse("https://example.test"), parse("https://example.test:443/path")))
	assert.False(t, sameHTTPOrigin(parse("http://example.test"), parse("https://example.test")))
	assert.False(t, sameHTTPOrigin(parse("http://example.test"), parse("http://example.test:81")))
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

// newHTTPMCPServer starts an httptest peer speaking the 2025-06-18 Streamable
// HTTP lifecycle. GET is offered only for explicit resume requests.
func newHTTPMCPServer(t *testing.T, sse bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodPost:
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
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
			if req.Method == "notifications/initialized" {
				if len(req.ID) != 0 {
					t.Errorf("initialized notification unexpectedly has id %s", req.ID)
				}
				w.WriteHeader(http.StatusAccepted)
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

func newHTTPTestCanary(t *testing.T) string {
	t.Helper()
	value := make([]byte, 32)
	_, err := rand.Read(value)
	require.NoError(t, err)
	return hex.EncodeToString(value)
}

func newHTTPTestAdapter(srv *httptest.Server) *HTTPClientAdapter {
	return NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       srv.URL,
	})
}

func writeHTTPMCPJSONRPCResponse(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	frame := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	_ = json.NewEncoder(w).Encode(frame)
}

func decodeHTTPMCPRequest(r *http.Request) (string, json.RawMessage, json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
		return "", nil, nil, err
	}
	var method string
	if err := json.Unmarshal(envelope["method"], &method); err != nil {
		return "", nil, nil, err
	}
	return method, envelope["id"], envelope["params"], nil
}

func TestMCP20250618StreamableHTTPHeaders(t *testing.T) {
	t.Parallel()

	const sessionID = "session-test-20250618"
	var seenMu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			t.Errorf("unexpected unsolicited GET %s", r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Method == http.MethodDelete {
			seenMu.Lock()
			seen = append(seen, "DELETE")
			seenMu.Unlock()
			assert.Equal(t, LatestProtocolVersion, r.Header.Get("MCP-Protocol-Version"))
			assert.Equal(t, sessionID, r.Header.Get("Mcp-Session-Id"))
			w.WriteHeader(http.StatusMethodNotAllowed) // A server may decline remote teardown.
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.Contains(t, r.Header.Get("Accept"), "application/json")
		assert.Contains(t, r.Header.Get("Accept"), "text/event-stream")
		method, id, paramsRaw, err := decodeHTTPMCPRequest(r)
		if err != nil {
			t.Errorf("decode POST request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		seenMu.Lock()
		seen = append(seen, method)
		seenMu.Unlock()
		switch method {
		case "initialize":
			assert.Empty(t, r.Header.Get("MCP-Protocol-Version"), "initialize has no negotiated protocol header yet")
			assert.Empty(t, r.Header.Get("Mcp-Session-Id"), "initialize has no session header yet")
			var params map[string]json.RawMessage
			_ = json.Unmarshal(paramsRaw, &params)
			var requestedVersion string
			_ = json.Unmarshal(params["protocolVersion"], &requestedVersion)
			assert.Equal(t, "2025-06-18", requestedVersion)
			w.Header().Set("Mcp-Session-Id", sessionID)
			writeHTTPMCPJSONRPCResponse(w, id, map[string]any{
				"protocolVersion": LatestProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "wire-test", "version": "1"},
			})
		case "notifications/initialized":
			assert.Empty(t, id, "initialized must be a JSON-RPC notification without id")
			assert.Equal(t, LatestProtocolVersion, r.Header.Get("MCP-Protocol-Version"))
			assert.Equal(t, sessionID, r.Header.Get("Mcp-Session-Id"))
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			assert.NotEmpty(t, id)
			assert.Equal(t, LatestProtocolVersion, r.Header.Get("MCP-Protocol-Version"))
			assert.Equal(t, sessionID, r.Header.Get("Mcp-Session-Id"))
			writeHTTPMCPJSONRPCResponse(w, id, map[string]any{"tools": []any{}})
		default:
			t.Errorf("unexpected JSON-RPC method %q", method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	adapter := newHTTPTestAdapter(srv)
	// Stale config values must not override negotiation-owned protocol headers.
	adapter.cfg.Headers = map[string]string{
		"MCP-Protocol-Version": "old-version",
		"Mcp-Session-Id":       "old-session",
	}
	require.NoError(t, adapter.Connect(context.Background()))
	assert.Equal(t, LatestProtocolVersion, adapter.ProtocolVersion())
	_, err := adapter.ListTools(context.Background())
	require.NoError(t, err)
	require.NoError(t, adapter.Disconnect(context.Background()))
	seenMu.Lock()
	defer seenMu.Unlock()
	assert.Equal(t, []string{"initialize", "notifications/initialized", "tools/list", "DELETE"}, seen)
}

func TestMCP20250618SSEFramesAndResume(t *testing.T) {
	t.Parallel()

	const sessionID = "session-sse-resume"
	var toolCalls atomic.Int32
	var resumeRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			resumeRequests.Add(1)
			assert.Equal(t, "text/event-stream", r.Header.Get("Accept"))
			assert.Equal(t, "cursor-call-2", r.Header.Get("Last-Event-ID"))
			assert.Equal(t, LatestProtocolVersion, r.Header.Get("MCP-Protocol-Version"))
			assert.Equal(t, sessionID, r.Header.Get("Mcp-Session-Id"))
			var id json.RawMessage
			id = json.RawMessage("2")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "id: replayed\nevent: message\ndata:{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"content\":\"resumed\"}}\n\n", id)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		method, id, _, err := decodeHTTPMCPRequest(r)
		if err != nil {
			t.Errorf("decode POST request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", sessionID)
			writeHTTPMCPJSONRPCResponse(w, id, map[string]any{
				"protocolVersion": LatestProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "wire-test", "version": "1"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			assert.Equal(t, LatestProtocolVersion, r.Header.Get("MCP-Protocol-Version"))
			assert.Equal(t, sessionID, r.Header.Get("Mcp-Session-Id"))
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			_, _ = io.WriteString(w, "id: progress-1\nevent: message\ndata:{\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1}}\n\n")
			response := fmt.Sprintf("{\"jsonrpc\":\"2.0\",\n\"id\":%s,\"result\":{\"tools\":[{\"name\":\"echo\"}]}}", id)
			lines := strings.Split(response, "\n")
			_, _ = fmt.Fprintf(w, "id: list-response\nevent: message\ndata:%s\ndata:%s\n\n", lines[0], lines[1])
		case "tools/call":
			toolCalls.Add(1)
			payload := "id: cursor-call-1\nevent: message\ndata:{\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":2}}\n\n" +
				"id: cursor-call-2\nevent: message\ndata:{\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":3}}\n\n"
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("HTTP hijacking unavailable for interruption fixture")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack response stream: %v", err)
				return
			}
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n%x\r\n%s\r\n", len(payload), payload)
			_ = conn.Close() // Deliberately omit the terminal zero chunk.
			// The POST stream is interrupted after ID-bearing events but before
			// its response. The client may resume, but must not replay POST.
		default:
			t.Errorf("unexpected JSON-RPC method %q", method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	adapter := newHTTPTestAdapter(srv)
	require.NoError(t, adapter.Connect(context.Background()))
	tools, err := adapter.ListTools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0].Name)

	result, err := adapter.CallTool(context.Background(), "echo", map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, "resumed", result.Content)
	assert.EqualValues(t, 1, toolCalls.Load(), "resumption must not replay tools/call")
	assert.EqualValues(t, 1, resumeRequests.Load())
}

func TestMCP20250618RejectsOAuthBeforeNetwork(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       srv.URL,
		OAuthConfig: &OAuthConfig{
			AuthorizationURL: "https://auth.invalid/authorize",
			TokenURL:         "https://auth.invalid/token",
		},
	})
	err := adapter.Connect(context.Background())
	require.Error(t, err)
	assert.Zero(t, requests.Load(), "unsupported OAuth must be rejected before network access")
}

func TestMCP20250618RejectsVersionContentTypeAndInvalidSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		protocol       string
		contentType    string
		sessionID      string
		wantContentErr bool
	}{
		{name: "version mismatch", protocol: "2025-03-26", contentType: "application/json"},
		{name: "unsupported content type", protocol: streamableHTTPProtocolVersion, contentType: "text/plain", wantContentErr: true},
		{name: "invalid session id", protocol: streamableHTTPProtocolVersion, contentType: "application/json", sessionID: "bad session"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var initialized atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("unexpected method %q", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				method, id, _, err := decodeHTTPMCPRequest(r)
				if err != nil {
					t.Errorf("decode MCP request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if method == "notifications/initialized" {
					initialized.Add(1)
					w.WriteHeader(http.StatusAccepted)
					return
				}
				if tt.sessionID != "" {
					w.Header().Set("Mcp-Session-Id", tt.sessionID)
				}
				w.Header().Set("Content-Type", tt.contentType)
				frame := map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"protocolVersion": tt.protocol,
					"capabilities":    map[string]any{},
				}}
				_ = json.NewEncoder(w).Encode(frame)
			}))
			t.Cleanup(srv.Close)

			adapter := newHTTPTestAdapter(srv)
			err := adapter.Connect(context.Background())
			require.Error(t, err)
			var protocolErr *HTTPProtocolError
			assert.ErrorAs(t, err, &protocolErr)
			if tt.wantContentErr {
				assert.Contains(t, err.Error(), "Content-Type")
			}
			assert.Zero(t, initialized.Load(), "failed initialization must not send notifications/initialized")
			assert.Empty(t, adapter.ProtocolVersion())
		})
	}
}

func TestMCP20250618RejectsSessionChangeAndMalformedSSEWithoutResume(t *testing.T) {
	t.Run("session ID cannot change after initialize", func(t *testing.T) {
		const firstSession = "session-initial"
		const changedSession = "session-changed"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("unexpected method %q", r.Method)
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			method, id, _, err := decodeHTTPMCPRequest(r)
			if err != nil {
				t.Errorf("decode MCP request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch method {
			case "initialize":
				w.Header().Set("Mcp-Session-Id", firstSession)
				writeHTTPMCPJSONRPCResponse(w, id, map[string]any{"protocolVersion": streamableHTTPProtocolVersion})
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			case "tools/list":
				assert.Equal(t, firstSession, r.Header.Get("Mcp-Session-Id"))
				w.Header().Set("Mcp-Session-Id", changedSession)
				writeHTTPMCPJSONRPCResponse(w, id, map[string]any{"tools": []any{}})
			default:
				t.Errorf("unexpected method %q", method)
			}
		}))
		t.Cleanup(srv.Close)
		adapter := newHTTPTestAdapter(srv)
		require.NoError(t, adapter.Connect(context.Background()))
		_, err := adapter.ListTools(context.Background())
		require.Error(t, err)
		var protocolErr *HTTPProtocolError
		assert.ErrorAs(t, err, &protocolErr)
	})

	t.Run("malformed SSE with cursor does not resume", func(t *testing.T) {
		var resumes atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				resumes.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				return
			}
			method, id, _, err := decodeHTTPMCPRequest(r)
			if err != nil {
				t.Errorf("decode MCP request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch method {
			case "initialize":
				writeHTTPMCPJSONRPCResponse(w, id, map[string]any{"protocolVersion": streamableHTTPProtocolVersion})
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			case "tools/list":
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "id: valid-cursor\ndata:{\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
				_, _ = io.WriteString(w, "id: malformed-event\ndata:not-json\n\n")
			default:
				t.Errorf("unexpected method %q", method)
			}
		}))
		t.Cleanup(srv.Close)
		adapter := newHTTPTestAdapter(srv)
		require.NoError(t, adapter.Connect(context.Background()))
		_, err := adapter.ListTools(context.Background())
		require.Error(t, err)
		assert.Zero(t, resumes.Load(), "protocol failure must not resume a stream")
	})
}

func TestMCP20250618RedirectCallbackCannotChangeOrigin(t *testing.T) {
	t.Parallel()
	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignRequests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(foreign.Close)
	foreignURL, err := url.Parse(foreign.URL)
	require.NoError(t, err)

	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/redirect-target", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(configured.Close)
	client := &http.Client{CheckRedirect: func(next *http.Request, _ []*http.Request) error {
		*next.URL = *foreignURL
		return nil
	}}
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       configured.URL + "/start",
	}, client)
	err = adapter.Connect(context.Background())
	require.Error(t, err)
	assert.Zero(t, foreignRequests.Load(), "post-callback origin validation must run before sending redirected credentials/request")
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
	// Initialization starts with a direct POST to the configured endpoint.
	srv := newHTTPMCPServer(t, false)
	adapter := newHTTPTestAdapter(srv)

	require.NoError(t, adapter.Connect(context.Background()))
	assert.Equal(t, srv.URL, adapter.current.endpoint)
	assert.True(t, adapter.current.connected)
}

func TestHTTPAdapterRejectsLegacySSETransportBeforeNetwork(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportSSE,
		URL:       srv.URL,
	})
	err := adapter.Connect(context.Background())
	require.ErrorIs(t, err, ErrUnsupportedHTTPTransport)
	assert.Zero(t, requests.Load(), "legacy endpoint probing is not part of this transport")
}

func TestHTTPAdapterRejectsCrossOriginRedirectBeforeCredentials(t *testing.T) {
	t.Parallel()
	headerCanary := newHTTPTestCanary(t)
	bearerCanary := newHTTPTestCanary(t)

	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(foreign.Close)

	configured := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(configured.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:        "srv",
		Transport:   MCPTransportStreamableHTTP,
		URL:         configured.URL,
		Headers:     map[string]string{"X-MCP-Canary": headerCanary},
		BearerToken: bearerCanary,
	})
	err := adapter.Connect(context.Background())
	require.ErrorIs(t, err, errCrossOriginRedirect)
	assert.NotContains(t, err.Error(), headerCanary)
	assert.NotContains(t, err.Error(), bearerCanary)
	assert.Zero(t, foreignRequests.Load(), "foreign redirect target must receive no request")
}

func TestHTTPAdapterAllowsSameOriginRedirectWithCredentials(t *testing.T) {
	t.Parallel()
	headerCanary := newHTTPTestCanary(t)
	bearerCanary := newHTTPTestCanary(t)

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
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode redirected MCP response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:        "srv",
		Transport:   MCPTransportStreamableHTTP,
		URL:         srv.URL + "/start",
		Headers:     map[string]string{"X-MCP-Canary": headerCanary},
		BearerToken: bearerCanary,
	})
	require.NoError(t, adapter.Connect(context.Background()))
	assert.GreaterOrEqual(t, redirectedRequests.Load(), int32(2), "initialize and initialized notification should reach the same-origin redirect target")
	assert.Zero(t, missingCredentials.Load(), "same-origin redirects must preserve configured credentials")
}

func TestMCPToolCallLostResponseIsUnknownAndNotRetried(t *testing.T) {
	t.Parallel()

	var sideEffects atomic.Int32
	var cancellationNotifications atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected HTTP method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
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
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if req.Method == "notifications/cancelled" {
			cancellationNotifications.Add(1)
			w.WriteHeader(http.StatusAccepted)
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
	assert.Zero(t, cancellationNotifications.Load(), "an unexpected transport disconnect must not notify remote cancellation")
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
				if r.Method != http.MethodPost {
					t.Errorf("unexpected HTTP method %q", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
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
				if req.Method == "notifications/initialized" {
					w.WriteHeader(http.StatusAccepted)
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
		if r.Method != http.MethodPost {
			t.Errorf("unexpected HTTP method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
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
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
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
		if r.Method != http.MethodPost {
			t.Errorf("unexpected HTTP method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
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
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := map[string]any{}
		switch req.Method {
		case "initialize":
			result["protocolVersion"] = LatestProtocolVersion
			result["capabilities"] = map[string]any{}
			result["serverInfo"] = map[string]any{"name": "test", "version": "1.0"}
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

func TestHTTPAdapterConnectAndListToolsDoNotRequireGET(t *testing.T) {
	t.Parallel()
	// Streamable HTTP initialization begins with POST. GET is optional and is
	// reserved here for resuming an ID-bearing interrupted POST stream.
	srv := newHTTPMCPServer(t, false)
	adapter := newHTTPTestAdapter(srv)

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))
	assert.Equal(t, srv.URL, adapter.current.endpoint)

	// Normal request/response traffic uses the same MCP endpoint directly.
	tools, err := adapter.ListTools(ctx)
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0].Name)
}

func TestHTTPAdapterListTools(t *testing.T) {
	t.Parallel()
	srv := newHTTPMCPServer(t, false)
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
	srv := newHTTPMCPServer(t, true)
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
	srv := newHTTPMCPServer(t, false)
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
	srv := newHTTPMCPServer(t, true)
	adapter := newHTTPTestAdapter(srv)

	ctx := context.Background()
	require.NoError(t, adapter.Connect(ctx))

	result, err := adapter.CallTool(ctx, "echo", map[string]any{"msg": "hi"})
	require.NoError(t, err)
	assert.Equal(t, "hello", result.Content)
	assert.False(t, result.IsError)
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
		if req.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
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
	// Initialize is the first POST and is rejected by the SSRF-safe dialer.
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
		err := adapter.Connect(context.Background())
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
		Name:      "rebinding",
		Transport: MCPTransportStreamableHTTP,
		URL:       "http://" + addr + "/mcp",
	}, tools.NewSSRFSafeHTTPClient(2*time.Second))
	err = adapter.Connect(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, tools.ErrPrivateIP)
}
