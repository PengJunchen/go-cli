package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lifecycleResponse(req *http.Request, status int, result any, sessionID string) *http.Response {
	header := make(http.Header)
	body := ""
	if result != nil {
		requestBody, err := req.GetBody()
		if err != nil {
			panic(err)
		}
		requestCopy := req.Clone(req.Context())
		requestCopy.Body = requestBody
		_, id, _, err := decodeHTTPMCPRequest(requestCopy)
		if err != nil {
			panic(err)
		}
		frame := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
		encoded, err := json.Marshal(frame)
		if err != nil {
			panic(err)
		}
		body = string(encoded)
		header.Set("Content-Type", "application/json")
	}
	if sessionID != "" {
		header.Set("Mcp-Session-Id", sessionID)
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func TestHTTPAdapterDisconnectWaitIsCancelableAndCloseIsJoined(t *testing.T) {
	t.Parallel()

	listStarted := make(chan struct{})
	listContext := make(chan context.Context, 1)
	releaseList := make(chan struct{})
	var releaseOnce sync.Once
	allowListReturn := func() { releaseOnce.Do(func() { close(releaseList) }) }
	t.Cleanup(allowListReturn)
	var deleteCalls atomic.Int32
	var roundTrips atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deleteCalls.Add(1)
			return lifecycleResponse(req, http.StatusMethodNotAllowed, nil, ""), nil
		}
		if req.Method != http.MethodPost {
			return lifecycleResponse(req, http.StatusMethodNotAllowed, nil, ""), nil
		}
		method, _, _, err := decodeHTTPMCPRequest(req)
		if err != nil {
			return nil, err
		}
		roundTrips.Add(1)
		switch method {
		case "initialize":
			return lifecycleResponse(req, http.StatusOK, map[string]any{"protocolVersion": streamableHTTPProtocolVersion}, "session-close-wait"), nil
		case "notifications/initialized":
			return lifecycleResponse(req, http.StatusAccepted, nil, ""), nil
		case "tools/list":
			listContext <- req.Context()
			close(listStarted)
			<-releaseList // Deliberately ignore request-context cancellation.
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
	require.NoError(t, adapter.Connect(context.Background()))

	listDone := make(chan error, 1)
	go func() {
		_, err := adapter.ListTools(context.Background())
		listDone <- err
	}()
	select {
	case <-listStarted:
	case <-time.After(time.Second):
		t.Fatal("tools/list RoundTrip did not start")
	}
	activeListCtx := <-listContext

	firstCtx, cancelFirst := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelFirst()
	firstDisconnect := make(chan error, 1)
	go func() { firstDisconnect <- adapter.Disconnect(firstCtx) }()
	var firstErr error
	select {
	case firstErr = <-firstDisconnect:
	case <-time.After(250 * time.Millisecond):
		t.Error("Disconnect did not return when its waiter context expired")
	}
	if firstErr != nil {
		assert.Contains(t, firstErr.Error(), "pending")
		assert.True(t, errors.Is(firstErr, context.DeadlineExceeded))
	}
	firstCloseDone := requireHTTPOperationDone(t, firstErr)
	select {
	case <-firstCloseDone:
		t.Fatal("pending disconnect completion closed while tools/list still held the connection generation")
	default:
	}
	select {
	case <-activeListCtx.Done():
		assert.ErrorIs(t, activeListCtx.Err(), context.Canceled)
	case <-time.After(250 * time.Millisecond):
		t.Error("Disconnect did not cancel the local active request context")
	}
	assert.Zero(t, deleteCalls.Load(), "DELETE must wait until the uncooperative active request actually returns")

	listCtx, cancelList := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelList()
	blockedList := make(chan error, 1)
	go func() {
		_, err := adapter.ListTools(listCtx)
		blockedList <- err
	}()
	select {
	case err := <-blockedList:
		require.Error(t, err, "the close fence must reject new requests")
	case <-time.After(100 * time.Millisecond):
		t.Error("new request was not rejected while close remained pending")
	}

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelSecond()
	secondDisconnect := make(chan error, 1)
	go func() { secondDisconnect <- adapter.Disconnect(secondCtx) }()
	var secondErr error
	select {
	case secondErr = <-secondDisconnect:
	case <-time.After(250 * time.Millisecond):
		t.Error("joining Disconnect did not return when its waiter context expired")
	}
	require.Error(t, secondErr)
	assert.Contains(t, secondErr.Error(), "pending")
	assert.True(t, errors.Is(secondErr, context.DeadlineExceeded))
	secondCloseDone := requireHTTPOperationDone(t, secondErr)
	assert.True(t, firstCloseDone == secondCloseDone, "all close waiters observe the same generation operation")
	assert.Zero(t, deleteCalls.Load(), "joining a pending close must not start a second DELETE")

	allowListReturn()
	select {
	case <-listDone:
	case <-time.After(time.Second):
		t.Fatal("active tools/list did not return after release")
	}
	if firstErr == nil {
		// The old implementation blocks past the waiter deadline. Its result is
		// consumed here so the regression fails without leaving the test hung.
		select {
		case <-firstDisconnect:
		case <-time.After(time.Second):
			t.Fatal("legacy Disconnect did not finish after the active request returned")
		}
	}
	require.NoError(t, adapter.Disconnect(context.Background()), "later waiters join and read the single close result")
	select {
	case <-firstCloseDone:
	default:
		t.Fatal("disconnect completion remained open after the generation finished closing")
	}
	assert.EqualValues(t, 1, deleteCalls.Load())
	assert.EqualValues(t, 3, roundTrips.Load(), "initialize, initialized, and the original tools/list are the only POSTs")
}

func TestHTTPAdapterCanceledConnectRemainsOwnedUntilActualReturn(t *testing.T) {
	t.Parallel()

	initStarted := make(chan struct{})
	releaseInit := make(chan struct{})
	var releaseOnce sync.Once
	allowInitReturn := func() { releaseOnce.Do(func() { close(releaseInit) }) }
	t.Cleanup(allowInitReturn)
	var initializeCalls atomic.Int32
	var deleteCalls atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deleteCalls.Add(1)
			return lifecycleResponse(req, http.StatusMethodNotAllowed, nil, ""), nil
		}
		if req.Method != http.MethodPost {
			return lifecycleResponse(req, http.StatusMethodNotAllowed, nil, ""), nil
		}
		method, _, _, err := decodeHTTPMCPRequest(req)
		if err != nil {
			return nil, err
		}
		switch method {
		case "initialize":
			call := initializeCalls.Add(1)
			if call == 1 {
				close(initStarted)
				<-releaseInit // Deliberately ignore the canceled connect context.
			}
			return lifecycleResponse(req, http.StatusOK, map[string]any{"protocolVersion": streamableHTTPProtocolVersion}, fmt.Sprintf("session-connect-%d", call)), nil
		case "notifications/initialized":
			return lifecycleResponse(req, http.StatusAccepted, nil, ""), nil
		default:
			return lifecycleResponse(req, http.StatusBadRequest, nil, ""), nil
		}
	})
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       "http://mcp.test/mcp",
	}, &http.Client{Transport: transport})

	connectCtx, cancelConnect := context.WithCancel(context.Background())
	connectDone := make(chan error, 1)
	go func() { connectDone <- adapter.Connect(connectCtx) }()
	select {
	case <-initStarted:
	case <-time.After(time.Second):
		t.Fatal("initialize RoundTrip did not start")
	}
	cancelConnect()
	var connectErr error
	select {
	case connectErr = <-connectDone:
	case <-time.After(250 * time.Millisecond):
		t.Error("Connect did not return when its owner context was canceled")
	}
	if connectErr != nil {
		assert.Contains(t, connectErr.Error(), "pending")
		assert.True(t, errors.Is(connectErr, context.Canceled))
	}
	connectOperationDone := requireHTTPOperationDone(t, connectErr)
	select {
	case <-connectOperationDone:
		t.Fatal("connect completion closed while initialize remained blocked")
	default:
	}
	assert.Empty(t, adapter.ProtocolVersion(), "an actual handshake still in progress cannot publish connection state")

	firstWaitCtx, cancelFirstWait := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelFirstWait()
	firstDisconnect := make(chan error, 1)
	go func() { firstDisconnect <- adapter.Disconnect(firstWaitCtx) }()
	var firstWaitErr error
	select {
	case firstWaitErr = <-firstDisconnect:
	case <-time.After(250 * time.Millisecond):
		t.Error("Disconnect did not return when its waiter context expired")
	}
	require.Error(t, firstWaitErr)
	assert.Contains(t, firstWaitErr.Error(), "pending")
	assert.True(t, errors.Is(firstWaitErr, context.DeadlineExceeded))
	assert.Zero(t, deleteCalls.Load(), "close owner must wait for actual initialize return")

	joinCtx, cancelJoin := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelJoin()
	joinConnect := make(chan error, 1)
	go func() { joinConnect <- adapter.Connect(joinCtx) }()
	var joinErr error
	select {
	case joinErr = <-joinConnect:
	case <-time.After(250 * time.Millisecond):
		t.Error("Connect did not return when its waiter context expired")
	}
	require.Error(t, joinErr)
	assert.Contains(t, joinErr.Error(), "pending", "Connect cannot replace a pending close operation")
	assert.EqualValues(t, 1, initializeCalls.Load(), "waiting for close must not start a new generation")

	allowInitReturn()
	require.NoError(t, adapter.Disconnect(context.Background()), "the close waiter joins the single owner")
	select {
	case <-connectOperationDone:
	default:
		t.Fatal("connect operation completion remained open after initialize returned")
	}
	assert.EqualValues(t, 1, deleteCalls.Load(), "the late session is deleted exactly once")
	assert.Empty(t, adapter.ProtocolVersion(), "the late handshake response must never publish after the close fence")

	require.NoError(t, adapter.Connect(context.Background()), "a successful close permits an explicit new generation")
	assert.Equal(t, streamableHTTPProtocolVersion, adapter.ProtocolVersion())
	require.NoError(t, adapter.Disconnect(context.Background()))
	assert.EqualValues(t, 2, deleteCalls.Load())
}

func TestHTTPPendingConnectCompletionBelongsToItsGeneration(t *testing.T) {
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	var releaseFirstOnce sync.Once
	var releaseSecondOnce sync.Once
	defer releaseFirstOnce.Do(func() { close(releaseFirst) })
	defer releaseSecondOnce.Do(func() { close(releaseSecond) })
	var initializeCalls atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			return lifecycleResponse(req, http.StatusMethodNotAllowed, nil, ""), nil
		}
		method, _, _, err := decodeHTTPMCPRequest(req)
		if err != nil {
			return nil, err
		}
		switch method {
		case "initialize":
			call := initializeCalls.Add(1)
			if call == 1 {
				close(firstStarted)
				<-releaseFirst
			} else if call == 2 {
				close(secondStarted)
				<-releaseSecond
			}
			return lifecycleResponse(req, http.StatusOK, map[string]any{"protocolVersion": streamableHTTPProtocolVersion}, fmt.Sprintf("generation-%d", call)), nil
		case "notifications/initialized":
			return lifecycleResponse(req, http.StatusAccepted, nil, ""), nil
		default:
			return lifecycleResponse(req, http.StatusBadRequest, nil, ""), nil
		}
	})
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name: "srv", Transport: MCPTransportStreamableHTTP, URL: "http://mcp.test/mcp",
	}, &http.Client{Transport: transport})

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstConnect := make(chan error, 1)
	go func() { firstConnect <- adapter.Connect(firstCtx) }()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first initialize did not start")
	}
	cancelFirst()
	firstErr := <-firstConnect
	firstDone := requireHTTPOperationDone(t, firstErr)
	select {
	case <-firstDone:
		t.Fatal("first generation completed before its blocked initialize returned")
	default:
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), 20*time.Millisecond)
	closeErr := adapter.Disconnect(closeCtx)
	cancelClose()
	require.ErrorIs(t, closeErr, ErrHTTPOperationPending)
	releaseFirstOnce.Do(func() { close(releaseFirst) })
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first generation completion did not close after initialize returned")
	}
	require.NoError(t, adapter.Disconnect(context.Background()))

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondConnect := make(chan error, 1)
	go func() { secondConnect <- adapter.Connect(secondCtx) }()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second initialize did not start")
	}
	cancelSecond()
	secondErr := <-secondConnect
	secondDone := requireHTTPOperationDone(t, secondErr)
	assert.False(t, firstDone == secondDone, "each pending connect identifies only its own generation")
	select {
	case <-firstDone:
	default:
		t.Fatal("the completed first generation became incomplete after reconnect")
	}
	select {
	case <-secondDone:
		t.Fatal("the new generation's completion was incorrectly inherited from the old one")
	default:
	}

	releaseSecondOnce.Do(func() { close(releaseSecond) })
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second generation completion did not close after initialize returned")
	}
	require.Error(t, secondErr, "the canceled second connect cannot publish success")
	require.NoError(t, adapter.Disconnect(context.Background()))
}

func TestHTTPAdapterFailedDeleteIsStoredAndBlocksReconnect(t *testing.T) {
	t.Parallel()

	var initializeCalls atomic.Int32
	var deleteCalls atomic.Int32
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodDelete {
			deleteCalls.Add(1)
			return lifecycleResponse(req, http.StatusInternalServerError, nil, ""), nil
		}
		method, _, _, err := decodeHTTPMCPRequest(req)
		if err != nil {
			return nil, err
		}
		switch method {
		case "initialize":
			initializeCalls.Add(1)
			return lifecycleResponse(req, http.StatusOK, map[string]any{"protocolVersion": streamableHTTPProtocolVersion}, "session-delete-fails"), nil
		case "notifications/initialized":
			return lifecycleResponse(req, http.StatusAccepted, nil, ""), nil
		default:
			return lifecycleResponse(req, http.StatusBadRequest, nil, ""), nil
		}
	})
	adapter := NewHTTPClientAdapterWithClient(MCPServerConfig{
		Name:      "srv",
		Transport: MCPTransportStreamableHTTP,
		URL:       "http://mcp.test/mcp",
	}, &http.Client{Transport: transport})
	require.NoError(t, adapter.Connect(context.Background()))

	deleteErr := adapter.Disconnect(context.Background())
	require.Error(t, deleteErr)
	assert.Contains(t, deleteErr.Error(), "HTTP status 500")
	assert.Empty(t, adapter.ProtocolVersion())
	assert.EqualError(t, adapter.Disconnect(context.Background()), deleteErr.Error(), "Disconnect returns the stored terminal result")
	assert.EqualError(t, adapter.Connect(context.Background()), deleteErr.Error(), "failed remote teardown keeps this adapter fail closed")
	assert.EqualValues(t, 1, initializeCalls.Load(), "failed teardown must not be overwritten by a new generation")
	assert.EqualValues(t, 1, deleteCalls.Load(), "the close owner makes exactly one DELETE attempt")
}
