package mcp

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const responseReadPanicMarker = "review-body-read-panic"

type trackedPanicReadBody struct {
	closeStarted chan struct{}
	releaseClose <-chan struct{}
	closeCalls   atomic.Int32
}

func (*trackedPanicReadBody) Read([]byte) (int, error) { panic(responseReadPanicMarker) }

func (b *trackedPanicReadBody) Close() error {
	b.closeCalls.Add(1)
	close(b.closeStarted)
	<-b.releaseClose
	return nil
}

func TestHTTPAdapterJoinsResponseBodyCloseAfterReadPanic(t *testing.T) {
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseClose) }) }
	t.Cleanup(release)
	var deleteCalls atomic.Int32
	body := &trackedPanicReadBody{closeStarted: closeStarted, releaseClose: releaseClose}
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
			return lifecycleResponse(req, http.StatusOK, map[string]any{"protocolVersion": streamableHTTPProtocolVersion}, "body-panic-session"), nil
		case "notifications/initialized":
			return lifecycleResponse(req, http.StatusAccepted, nil, ""), nil
		case "tools/list":
			return &http.Response{
				Status: "200 OK", StatusCode: http.StatusOK, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
				Header: http.Header{"Content-Type": []string{"application/json"}}, Body: body, Request: req,
			}, nil
		default:
			return lifecycleResponse(req, http.StatusBadRequest, nil, ""), nil
		}
	})
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name: "srv", Transport: MCPTransportStreamableHTTP, URL: "http://mcp.test/mcp",
	}, &http.Client{Transport: transport})
	require.NoError(t, adapter.Connect(context.Background()))

	requestDone := make(chan any, 1)
	go func() {
		defer func() { requestDone <- recover() }()
		_, _ = adapter.ListTools(context.Background())
	}()
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		release()
		t.Fatal("response body was not closed while unwinding a read panic")
	}

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelWait()
	disconnectDone := make(chan error, 1)
	go func() { disconnectDone <- adapter.Disconnect(waitCtx) }()
	var waitErr error
	select {
	case waitErr = <-disconnectDone:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Disconnect waiter did not honor its deadline")
	}
	require.ErrorIs(t, waitErr, context.DeadlineExceeded)
	require.ErrorIs(t, waitErr, ErrHTTPOperationPending)
	assert.Zero(t, deleteCalls.Load(), "DELETE must wait until response-body Close returns")
	assert.EqualValues(t, 1, body.closeCalls.Load())

	release()
	select {
	case recovered := <-requestDone:
		assert.Equal(t, responseReadPanicMarker, recovered, "the body panic remains visible to the synchronous caller")
	case <-time.After(time.Second):
		t.Fatal("request caller did not unwind after body Close was released")
	}
	require.NoError(t, adapter.Disconnect(context.Background()))
	assert.EqualValues(t, 1, deleteCalls.Load(), "the single close owner deletes after body Close is joined")
	assert.EqualValues(t, 1, body.closeCalls.Load(), "the response is closed once even though resume/cleanup paths share it")
	assert.True(t, strings.Contains(waitErr.Error(), "pending"))
}
