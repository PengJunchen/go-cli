package mcp

import (
	"context"
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
