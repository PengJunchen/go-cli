package mcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPAdapterConnectCompletionWaitsForOwnedLogging(t *testing.T) {
	previousLogger := slog.Default()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLog := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseLog()
		slog.SetDefault(previousLogger)
	})
	var blockedOnce sync.Once
	slog.SetDefault(slog.New(blockingConnectHandler{
		started: started,
		release: release,
		once:    &blockedOnce,
	}))

	var initializeCalls atomic.Int32
	var deleteCalls atomic.Int32
	var toolCalls atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deleteCalls.Add(1)
			return lifecycleResponse(req, http.StatusMethodNotAllowed, nil, ""), nil
		}
		method, _, _, err := decodeHTTPMCPRequest(req)
		if err != nil {
			return nil, err
		}
		switch method {
		case "initialize":
			initializeCalls.Add(1)
			return lifecycleResponse(req, http.StatusOK, map[string]any{"protocolVersion": streamableHTTPProtocolVersion}, "session-log-wait"), nil
		case "notifications/initialized":
			return lifecycleResponse(req, http.StatusAccepted, nil, ""), nil
		case "tools/list":
			toolCalls.Add(1)
			return lifecycleResponse(req, http.StatusOK, map[string]any{"tools": []any{}}, ""), nil
		default:
			return lifecycleResponse(req, http.StatusBadRequest, nil, ""), nil
		}
	})
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       "http://mcp.test/mcp",
	}, &http.Client{Transport: transport})
	connectDone := make(chan error, 1)
	go func() { connectDone <- adapter.Connect(context.Background()) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Connect success logger did not start")
	}

	var earlyConnectErr error
	connectReturnedEarly := false
	select {
	case err := <-connectDone:
		earlyConnectErr = err
		connectReturnedEarly = true
		t.Errorf("Connect returned before its logging callback exited: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	assert.Empty(t, adapter.ProtocolVersion(), "the protocol must not be visible before Connect's owner finishes")
	_, listErr := adapter.ListTools(context.Background())
	require.Error(t, listErr)
	assert.Contains(t, listErr.Error(), "requires an initialized connection")
	assert.Zero(t, toolCalls.Load(), "requests must not enter while Connect is still owned")

	disconnectCtx, cancelDisconnect := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelDisconnect()
	disconnectDone := make(chan error, 1)
	go func() { disconnectDone <- adapter.Disconnect(disconnectCtx) }()
	var disconnectErr error
	select {
	case disconnectErr = <-disconnectDone:
	case <-time.After(250 * time.Millisecond):
		t.Error("Disconnect waiter did not honor its own deadline")
	}
	require.Error(t, disconnectErr)
	assert.True(t, errors.Is(disconnectErr, context.DeadlineExceeded))
	assert.True(t, errors.Is(disconnectErr, ErrHTTPOperationPending))
	assert.Zero(t, deleteCalls.Load(), "close owner waits for Connect's actual completion")

	connectWaitCtx, cancelConnectWait := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelConnectWait()
	connectWaitDone := make(chan error, 1)
	go func() { connectWaitDone <- adapter.Connect(connectWaitCtx) }()
	var connectWaitErr error
	select {
	case connectWaitErr = <-connectWaitDone:
	case <-time.After(250 * time.Millisecond):
		t.Error("repeated Connect waiter did not honor its deadline")
	}
	require.Error(t, connectWaitErr)
	assert.True(t, errors.Is(connectWaitErr, ErrHTTPOperationPending))
	assert.EqualValues(t, 1, initializeCalls.Load(), "a pending connection cannot be overwritten by a new generation")

	releaseLog()
	if !connectReturnedEarly {
		select {
		case err := <-connectDone:
			require.Error(t, err, "Disconnect fenced publication while Connect's logger was blocked")
		case <-time.After(time.Second):
			t.Fatal("Connect owner did not exit after logging was released")
		}
	} else if earlyConnectErr == nil {
		t.Error("Connect reported success before its logging callback exited")
	}
	require.NoError(t, adapter.Disconnect(context.Background()))
	assert.EqualValues(t, 1, deleteCalls.Load(), "the close owner deletes the captured session once")

	require.NoError(t, adapter.Connect(context.Background()), "successful teardown permits a new explicit generation")
	assert.Equal(t, streamableHTTPProtocolVersion, adapter.ProtocolVersion())
	require.NoError(t, adapter.Disconnect(context.Background()))
	assert.EqualValues(t, 2, initializeCalls.Load())

	slog.SetDefault(previousLogger)
}

type blockingConnectHandler struct {
	started chan struct{}
	release <-chan struct{}
	once    *sync.Once
}

func (h blockingConnectHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h blockingConnectHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "mcp.http.connect" {
		h.once.Do(func() {
			close(h.started)
			<-h.release
		})
	}
	return nil
}

func (h blockingConnectHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h blockingConnectHandler) WithGroup(string) slog.Handler { return h }

const lifecyclePanicMarker = "lifecycle-owner-panic-marker"

func TestHTTPAdapterLifecycleOwnerPanicIsContained(t *testing.T) {
	for _, mode := range []string{"roundtripper", "connect_log", "initialize_body_close", "delete_body_close"} {
		t.Run(mode, func(t *testing.T) {
			command := exec.Command(os.Args[0], "-test.run=^TestHTTPAdapterLifecyclePanicChild$")
			command.Env = append(os.Environ(), "MCP_HTTP_LIFECYCLE_PANIC_CHILD="+mode)
			output, err := command.CombinedOutput()
			require.NoError(t, err, "child lifecycle panic must be contained; output: %s", output)
			assert.NotContains(t, string(output), lifecyclePanicMarker)
		})
	}
}

func TestHTTPAdapterLifecyclePanicChild(t *testing.T) {
	mode := os.Getenv("MCP_HTTP_LIFECYCLE_PANIC_CHILD")
	if mode == "" {
		return
	}
	var deleteCalls atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if mode == "roundtripper" {
			panic(lifecyclePanicMarker)
		}
		if req.Method == http.MethodDelete {
			deleteCalls.Add(1)
			if mode != "delete_body_close" {
				return lifecycleResponse(req, http.StatusMethodNotAllowed, nil, ""), nil
			}
			return &http.Response{
				Status:     "405 Method Not Allowed",
				StatusCode: http.StatusMethodNotAllowed,
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header:     make(http.Header),
				Body:       panicCloseReadCloser{Reader: strings.NewReader("")},
				Request:    req,
			}, nil
		}
		method, _, _, err := decodeHTTPMCPRequest(req)
		if err != nil {
			return nil, err
		}
		switch method {
		case "initialize":
			resp := lifecycleResponse(req, http.StatusOK, map[string]any{"protocolVersion": streamableHTTPProtocolVersion}, "session-panic-close")
			if mode == "initialize_body_close" {
				body, readErr := io.ReadAll(resp.Body)
				if readErr != nil {
					return nil, readErr
				}
				resp.Body = panicCloseReadCloser{Reader: strings.NewReader(string(body))}
			}
			return resp, nil
		case "notifications/initialized":
			return lifecycleResponse(req, http.StatusAccepted, nil, ""), nil
		default:
			return lifecycleResponse(req, http.StatusBadRequest, nil, ""), nil
		}
	})
	previousLogger := slog.Default()
	if mode == "connect_log" || mode == "initialize_body_close" {
		slog.SetDefault(slog.New(panicConnectHandler{}))
		defer slog.SetDefault(previousLogger)
	}
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       "http://mcp.test/mcp",
	}, &http.Client{Transport: transport})

	if mode == "roundtripper" {
		err := adapter.Connect(context.Background())
		require.Error(t, err)
		assert.NotContains(t, err.Error(), lifecyclePanicMarker)
		assert.Empty(t, adapter.ProtocolVersion())
		disconnectErr := adapter.Disconnect(context.Background())
		require.Error(t, disconnectErr, "without a known session ID teardown cannot be claimed")
		assert.NotContains(t, disconnectErr.Error(), lifecyclePanicMarker)
		assert.EqualError(t, adapter.Disconnect(context.Background()), disconnectErr.Error())
		return
	}
	if mode == "connect_log" || mode == "initialize_body_close" {
		err := adapter.Connect(context.Background())
		require.Error(t, err)
		assert.NotContains(t, err.Error(), lifecyclePanicMarker)
		assert.Empty(t, adapter.ProtocolVersion())
		require.NoError(t, adapter.Disconnect(context.Background()), "session identity learned before the callback panic is still deleted")
		assert.EqualValues(t, 1, deleteCalls.Load())
		return
	}
	require.NoError(t, adapter.Connect(context.Background()))
	err := adapter.Disconnect(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), lifecyclePanicMarker)
	assert.Contains(t, err.Error(), "unknown")
	assert.EqualError(t, adapter.Disconnect(context.Background()), err.Error(), "close panic remains a stable terminal result")
	assert.EqualError(t, adapter.Connect(context.Background()), err.Error(), "close panic keeps reconnect fail closed")
}

type panicCloseReadCloser struct {
	io.Reader
}

func (panicCloseReadCloser) Close() error { panic(lifecyclePanicMarker) }

type panicConnectHandler struct{}

func (panicConnectHandler) Enabled(context.Context, slog.Level) bool { return true }

func (panicConnectHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "mcp.http.connect" {
		panic(lifecyclePanicMarker)
	}
	return nil
}

func (panicConnectHandler) WithAttrs([]slog.Attr) slog.Handler { return panicConnectHandler{} }

func (panicConnectHandler) WithGroup(string) slog.Handler { return panicConnectHandler{} }
