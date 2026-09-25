package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

func TestMCP20250618ResumeEnforcesAggregateResponseBudget(t *testing.T) {
	t.Parallel()

	const eventDataSize = 6 * 1024 * 1024
	largeData := strings.Repeat("x", eventDataSize)
	var toolCalls atomic.Int32
	var resumes atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			resumes.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			progress := fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"value":%q}}`, largeData)
			_, _ = fmt.Fprintf(w, "id: resume-progress\ndata:%s\n\n", progress)
			_, _ = io.WriteString(w, "id: resumed-response\ndata:{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[]}}\n\n")
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected HTTP method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		method, id, _, err := decodeHTTPMCPRequest(r)
		if err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch method {
		case "initialize":
			writeHTTPMCPJSONRPCResponse(w, id, map[string]any{"protocolVersion": streamableHTTPProtocolVersion})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			toolCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			progress := fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"value":%q}}`, largeData)
			_, _ = fmt.Fprintf(w, "id: initial-progress\ndata:%s\n\n", progress)
		default:
			t.Errorf("unexpected JSON-RPC method %q", method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	adapter := newHTTPTestAdapter(srv)
	require.NoError(t, adapter.Connect(context.Background()))
	_, err := adapter.ListTools(context.Background())
	require.Error(t, err, "the combined POST and resume GET response must exceed the per-request cap")
	assert.EqualValues(t, 1, toolCalls.Load(), "the interrupted operation must not replay its POST")
	assert.EqualValues(t, 1, resumes.Load())
}

func TestHTTPConnectCanceledBeforeDispatchDoesNotPoisonNextConnect(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("unexpected HTTP method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		method, id, _, err := decodeHTTPMCPRequest(r)
		if err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch method {
		case "initialize":
			writeHTTPMCPJSONRPCResponse(w, id, map[string]any{"protocolVersion": streamableHTTPProtocolVersion})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected JSON-RPC method %q", method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	adapter := newHTTPTestAdapter(srv)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	firstErr := adapter.Connect(canceled)
	require.ErrorIs(t, firstErr, context.Canceled)
	assert.Zero(t, requests.Load(), "a canceled Connect must not dispatch an initialize request")

	require.NoError(t, adapter.Connect(context.Background()), "a pre-dispatch cancellation must not publish a failed generation")
	assert.EqualValues(t, 2, requests.Load(), "the healthy Connect must complete both handshake requests")
	require.NoError(t, adapter.Disconnect(context.Background()))
}

func TestWaitForConnectDoesNotTreatCleanupCancelAsCallerCancel(t *testing.T) {
	operationCtx, cancelOperation := context.WithCancel(context.Background())
	op := &httpConnectOperation{done: make(chan struct{}), cancel: cancelOperation}
	op.cancel()
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(20 * time.Millisecond)
		close(op.done)
	}()

	require.ErrorIs(t, operationCtx.Err(), context.Canceled)
	err := waitForConnect(context.Background(), op, httpConnectWaitPolicy{lifecycleCtx: context.Background(), owner: true})
	<-released
	require.NoError(t, err, "normal operation cleanup must not wake the waiter before done is published")
}

func TestReadSSEResponseCountsRawCRLFBytes(t *testing.T) {
	t.Parallel()

	idLine := "id:cursor\r\n"
	dataPrefix := `data:{"jsonrpc":"2.0","id":7,"result":{}`
	spaceCount := maxHTTPResponseSize + 3 - len(idLine) - len(dataPrefix) - 1 - 2 - 2
	require.Greater(t, spaceCount, 0)
	input := idLine + dataPrefix + strings.Repeat(" ", spaceCount) + "}\r\n\r\n"
	require.EqualValues(t, maxHTTPResponseSize+3, len(input))

	_, _, err := readSSEResponse(strings.NewReader(input), 7)
	require.Error(t, err, "CRLF bytes must count toward the raw response budget")
}

func TestReadSSEResponseDiscardsUnterminatedFinalEvent(t *testing.T) {
	t.Parallel()

	input := "id:response\ndata:{\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}"
	_, _, err := readSSEResponse(strings.NewReader(input), 7)
	require.ErrorIs(t, err, errSSEResponseMissing, "an event without its terminating blank line is not dispatched at EOF")
}

func TestReadSSEResponseSupportsCRAndLeadingBOM(t *testing.T) {
	t.Parallel()

	input := "\uFEFFid:cursor\rdata:{\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}\r\r"
	result, cursor, err := readSSEResponse(strings.NewReader(input), 7)
	require.NoError(t, err)
	assert.Equal(t, "cursor", cursor)
	assert.Equal(t, true, result["ok"])
}

func TestValidLastEventIDRespectsHTTPFieldValueSafety(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "UTF-8 obs-text", value: "cursor-雪", valid: true},
		{name: "internal whitespace", value: "cursor with space", valid: true},
		{name: "empty", value: "", valid: false},
		{name: "control", value: "cursor\x01", valid: false},
		{name: "NUL", value: "cursor\x00", valid: false},
		{name: "leading whitespace", value: " cursor", valid: false},
		{name: "trailing whitespace", value: "cursor ", valid: false},
		{name: "invalid UTF-8", value: string([]byte{'c', 0xff}), valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.valid, validLastEventID(test.value))
		})
	}
}

func TestMCP20250618ResumeAcceptsUTF8EventID(t *testing.T) {
	t.Parallel()

	const cursor = "resume-雪"
	lastEventID := make(chan string, 1)
	var toolsListIDMu sync.Mutex
	var toolsListID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			lastEventID <- r.Header.Get("Last-Event-ID")
			w.Header().Set("Content-Type", "text/event-stream")
			toolsListIDMu.Lock()
			id := toolsListID
			toolsListIDMu.Unlock()
			frame, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      json.RawMessage(id),
				"result":  map[string]any{"tools": []any{}},
			})
			if err != nil {
				t.Errorf("marshal resumed response: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = fmt.Fprintf(w, "data:%s\n\n", frame)
			return
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected HTTP method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		method, id, _, err := decodeHTTPMCPRequest(r)
		if err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch method {
		case "initialize":
			writeHTTPMCPJSONRPCResponse(w, id, map[string]any{"protocolVersion": streamableHTTPProtocolVersion})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			toolsListIDMu.Lock()
			toolsListID = string(id)
			toolsListIDMu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "\uFEFFid:%s\rdata:{\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\r\r", cursor)
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
	assert.Empty(t, tools)
	select {
	case actual := <-lastEventID:
		assert.Equal(t, cursor, actual)
	case <-time.After(time.Second):
		t.Fatal("server did not receive the UTF-8 Last-Event-ID resume header")
	}
}

func TestMCPJSONBodyCancellationPreservesCause(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, id, _, err := decodeHTTPMCPRequest(r)
		if err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch method {
		case "initialize":
			writeHTTPMCPJSONRPCResponse(w, id, map[string]any{"protocolVersion": streamableHTTPProtocolVersion})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			writeHTTPMCPJSONRPCResponse(w, id, map[string]any{"tools": []any{}})
		default:
			t.Errorf("unexpected JSON-RPC method %q", method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	readStarted := make(chan struct{})
	var roundTrips atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		if req.Method == http.MethodPost && roundTrips.Add(1) == 3 {
			resp.Body = &contextBlockingReadCloser{ReadCloser: resp.Body, ctx: req.Context(), started: readStarted}
		}
		return resp, nil
	})}

	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       srv.URL,
	}, client)
	require.NoError(t, adapter.Connect(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := adapter.ListTools(ctx)
		errCh <- err
	}()

	select {
	case <-readStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not begin reading the JSON response body")
	}
	cancel()
	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled), "error chain must retain the caller cancellation")
	case <-time.After(2 * time.Second):
		t.Fatal("request did not return after its context was canceled")
	}
}

type contextBlockingReadCloser struct {
	io.ReadCloser
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (r *contextBlockingReadCloser) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func TestHTTPAdapterCrossOriginErrorRedactsURLUserinfo(t *testing.T) {
	t.Parallel()

	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignRequests.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(foreign.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Redirect(w, r, foreign.URL, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(origin.Close)

	password := newHTTPTestCanary(t)
	endpoint, err := url.Parse(origin.URL)
	require.NoError(t, err)
	endpoint.Path = "/mcp"
	endpoint.User = url.UserPassword(newHTTPTestCanary(t), password)
	adapter := NewHTTPClientAdapter(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       endpoint.String(),
	})
	err = adapter.Connect(context.Background())
	require.ErrorIs(t, err, errCrossOriginRedirect)
	assert.NotContains(t, err.Error(), password, "redirect diagnostics must not expose URL userinfo")
	assert.Zero(t, foreignRequests.Load())
}

func TestHTTPAdapterRedirectCallbackErrorRedactsURLSecrets(t *testing.T) {
	t.Parallel()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			http.Redirect(w, r, r.URL.Path+"?redirect=1", http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(origin.Close)

	username := newHTTPTestCanary(t)
	password := newHTTPTestCanary(t)
	querySecret := newHTTPTestCanary(t)
	callbackSecret := newHTTPTestCanary(t)
	endpoint, err := url.Parse(origin.URL)
	require.NoError(t, err)
	endpoint.Path = "/mcp"
	endpoint.User = url.UserPassword(username, password)
	endpoint.RawQuery = "api_key=" + url.QueryEscape(querySecret)
	callbackFailure := errors.New("redirect callback failure sentinel")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("callback diagnostic %s: %w", callbackSecret, callbackFailure)
	}}
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       endpoint.String(),
	}, client)

	err = adapter.Connect(context.Background())
	require.ErrorIs(t, err, callbackFailure)
	for _, secret := range []string{username, password, querySecret, callbackSecret} {
		assert.NotContains(t, err.Error(), secret)
	}
}

func TestHTTPAdapterRoundTripErrorRedactsURLSecrets(t *testing.T) {
	t.Parallel()

	username := newHTTPTestCanary(t)
	password := newHTTPTestCanary(t)
	querySecret := newHTTPTestCanary(t)
	endpoint, err := url.Parse("http://mcp.test/mcp")
	require.NoError(t, err)
	endpoint.User = url.UserPassword(username, password)
	endpoint.RawQuery = "api_key=" + url.QueryEscape(querySecret)
	roundTripFailure := errors.New("round trip failure sentinel")
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, roundTripFailure
	})}
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       endpoint.String(),
	}, client)

	err = adapter.Connect(context.Background())
	require.ErrorIs(t, err, roundTripFailure)
	for _, secret := range []string{username, password, querySecret} {
		assert.NotContains(t, err.Error(), secret)
	}
}
