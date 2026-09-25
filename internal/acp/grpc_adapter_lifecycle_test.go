package acp

import (
	"bytes"
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

	"github.com/stretchr/testify/require"
)

func TestGRPCAdapterDisconnectClosesCapturedReceiveChannel(t *testing.T) {
	server := startHTTPProfilePeer(t)
	client := NewGRPCAdapter("http://"+server.Addr(), WithName("reader-lifecycle"))
	require.NoError(t, client.Connect(context.Background()))
	inbound := client.ReceiveMessages()
	require.NotNil(t, inbound)
	require.NoError(t, client.Disconnect(context.Background()))
	select {
	case _, ok := <-inbound:
		require.False(t, ok, "the connection worker must own and close its channel")
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect returned before the receive channel closed")
	}
}

func TestGRPCAdapterDisconnectWaitsForStreamBodyClose(t *testing.T) {
	server := startHTTPProfilePeer(t)
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseClose) }) }
	t.Cleanup(release)
	transport := &streamBodyCloseGateTransport{
		base:         http.DefaultTransport,
		closeStarted: closeStarted,
		releaseClose: releaseClose,
	}
	client := NewGRPCAdapter("http://"+server.Addr(), WithName("reader-close-lifecycle"))
	adapter := client.(*gRPCAdapter)
	adapter.client.Transport = transport
	require.NoError(t, client.Connect(context.Background()))
	inbound := client.ReceiveMessages()

	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("stream response body was not closed by the reader")
	}
	disconnectResult := make(chan error, 1)
	go func() { disconnectResult <- client.Disconnect(context.Background()) }()
	require.Eventually(t, func() bool {
		adapter.mu.Lock()
		defer adapter.mu.Unlock()
		return adapter.current != nil && adapter.current.cleanupStarted
	}, 2*time.Second, time.Millisecond, "first Disconnect must own cleanup before the bounded waiter joins")

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelWait()
	require.ErrorIs(t, client.Disconnect(waitCtx), context.DeadlineExceeded,
		"Disconnect must wait until the reader's response body actually closes")
	require.Zero(t, transport.disconnectCalls.Load(), "remote disconnect must wait for reader completion")

	release()
	require.NoError(t, <-disconnectResult)
	select {
	case _, ok := <-inbound:
		require.False(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("receive channel was not closed after stream body cleanup")
	}
	require.EqualValues(t, 1, transport.disconnectCalls.Load())
}

func TestGRPCAdapterReconnectIsExplicitlyUnsupported(t *testing.T) {
	server := startHTTPProfilePeer(t)
	client := NewGRPCAdapter("http://"+server.Addr(), WithName("no-reconnect"))
	require.NoError(t, client.Connect(context.Background()))
	adapter := client.(*gRPCAdapter)
	err := adapter.Reconnect(context.Background())
	require.ErrorIs(t, err, ErrReconnectUnsupported)
	require.NoError(t, client.Disconnect(context.Background()))
}

func TestGRPCAdapterCanceledConnectAdmissionDoesNotPublishGeneration(t *testing.T) {
	server := startHTTPProfilePeer(t)
	transport := &countingHTTPTransport{base: http.DefaultTransport}
	client := NewGRPCAdapter("http://"+server.Addr(), WithName("canceled-admission"))
	adapter := client.(*gRPCAdapter)
	adapter.client.Transport = transport
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, client.Connect(canceledCtx), context.Canceled)
	require.Zero(t, transport.requests.Load(), "an already-canceled Connect must not dispatch HTTP")
	adapter.mu.Lock()
	current := adapter.current
	adapter.mu.Unlock()
	require.Nil(t, current, "an already-canceled Connect must not publish a generation")

	require.NoError(t, client.Connect(context.Background()), "a healthy Connect must be admitted after canceled entry")
	require.Positive(t, transport.requests.Load())
	require.NoError(t, client.Disconnect(context.Background()))
}

func TestGRPCAdapterDisconnectCleanupResultIsStable(t *testing.T) {
	server := startHTTPProfilePeer(t)
	const closeFailureText = "test disconnect cleanup failure"
	closeFailure := errors.New(closeFailureText)
	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseClose) }) }
	gate := &responseCloseGateTransport{
		base:         http.DefaultTransport,
		closeStarted: closeStarted,
		releaseClose: releaseClose,
		closeErr:     closeFailure,
	}
	t.Cleanup(release)
	client := NewGRPCAdapter("http://"+server.Addr(), WithName("cleanup-result"))
	adapter := client.(*gRPCAdapter)
	adapter.client.Transport = gate
	require.NoError(t, client.Connect(context.Background()))

	firstResult := make(chan error, 1)
	go func() { firstResult <- client.Disconnect(context.Background()) }()
	select {
	case <-closeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect response body was not closed")
	}

	waitCtx, cancelWait := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelWait()
	require.ErrorIs(t, client.Disconnect(waitCtx), context.DeadlineExceeded,
		"later callers must wait for the same in-flight cleanup")
	require.EqualValues(t, 1, gate.disconnectRequests.Load(), "cleanup owner sends one remote disconnect")

	release()
	select {
	case err := <-firstResult:
		require.ErrorIs(t, err, closeFailure)
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not publish the response body close error")
	}
	require.ErrorIs(t, client.Disconnect(context.Background()), closeFailure,
		"completed cleanup result must remain stable")
	require.ErrorIs(t, client.Connect(context.Background()), closeFailure,
		"a failed cleanup must fence creation of another session")
	require.EqualValues(t, 1, gate.disconnectRequests.Load(), "later Disconnect must not resend cleanup")
}

func TestGRPCAdapterDisconnectOwnsLateConnectSession(t *testing.T) {
	transport := &lateConnectTransport{
		connectStarted:       make(chan struct{}),
		releaseConnect:       make(chan struct{}),
		disconnectSessionIDs: make(chan string, 1),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(transport.releaseConnect) }) }
	t.Cleanup(release)
	client := NewGRPCAdapter("http://loopback.invalid", WithName("late-connect"))
	adapter := client.(*gRPCAdapter)
	adapter.client.Transport = transport

	connectResult := make(chan error, 1)
	go func() { connectResult <- client.Connect(context.Background()) }()
	select {
	case <-transport.connectStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("connect request did not reach the transport")
	}

	adapter.mu.Lock()
	generation := adapter.current
	adapter.mu.Unlock()
	disconnectResult := make(chan error, 1)
	go func() { disconnectResult <- client.Disconnect(context.Background()) }()
	select {
	case <-generation.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect did not fence the connecting generation")
	}
	select {
	case err := <-disconnectResult:
		t.Fatalf("Disconnect returned before the late connect result was owned: %v", err)
	default:
	}

	release()
	select {
	case err := <-connectResult:
		require.ErrorIs(t, err, errACPCleanupInProgress)
	case <-time.After(2 * time.Second):
		t.Fatal("Connect did not publish its fenced result")
	}
	require.NoError(t, <-disconnectResult)
	select {
	case sessionID := <-transport.disconnectSessionIDs:
		require.Equal(t, "opaque-late-session", sessionID)
	case <-time.After(2 * time.Second):
		t.Fatal("the session created by the late connect was not disconnected")
	}
	require.EqualValues(t, 1, transport.disconnectCalls.Load())
}

func TestGRPCAdapterCallerTimeoutDoesNotForgetLateConnectSession(t *testing.T) {
	transport := &lateConnectTransport{
		connectStarted:       make(chan struct{}),
		releaseConnect:       make(chan struct{}),
		disconnectSessionIDs: make(chan string, 1),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(transport.releaseConnect) }) }
	t.Cleanup(release)
	client := NewGRPCAdapter("http://loopback.invalid", WithName("caller-timeout"))
	adapter := client.(*gRPCAdapter)
	adapter.client.Transport = transport

	connectCtx, cancelConnect := context.WithCancel(context.Background())
	connectResult := make(chan error, 1)
	go func() { connectResult <- client.Connect(connectCtx) }()
	select {
	case <-transport.connectStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("connect request did not reach the transport")
	}
	cancelConnect()
	require.ErrorIs(t, <-connectResult, context.Canceled, "caller timeout ends the wait but not the owned request")

	disconnectResult := make(chan error, 1)
	go func() { disconnectResult <- client.Disconnect(context.Background()) }()
	adapter.mu.Lock()
	generation := adapter.current
	adapter.mu.Unlock()
	select {
	case <-generation.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect did not fence the request after the caller timed out")
	}
	release()
	require.NoError(t, <-disconnectResult)
	select {
	case sessionID := <-transport.disconnectSessionIDs:
		require.Equal(t, "opaque-late-session", sessionID)
	case <-time.After(2 * time.Second):
		t.Fatal("the late session ID was not retained for cleanup")
	}
	require.EqualValues(t, 1, transport.disconnectCalls.Load())
}

func TestGRPCAdapterDisconnectWaitsForActiveSend(t *testing.T) {
	server := startHTTPProfilePeer(t)
	transport := &sendGateTransport{
		base:        http.DefaultTransport,
		sendStarted: make(chan struct{}),
		releaseSend: make(chan struct{}),
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(transport.releaseSend) }) }
	t.Cleanup(release)
	client := NewGRPCAdapter("http://"+server.Addr(), WithName("send-cleanup"))
	adapter := client.(*gRPCAdapter)
	adapter.client.Transport = transport
	require.NoError(t, client.Connect(context.Background()))

	sendResult := make(chan error, 1)
	go func() {
		sendResult <- client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, Content: "in flight"})
	}()
	select {
	case <-transport.sendStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("send did not reach the controlled transport")
	}

	adapter.mu.Lock()
	generation := adapter.current
	adapter.mu.Unlock()
	disconnectResult := make(chan error, 1)
	go func() { disconnectResult <- client.Disconnect(context.Background()) }()
	select {
	case <-generation.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Disconnect did not cancel the active generation")
	}
	require.Zero(t, transport.disconnectCalls.Load(), "remote disconnect must wait for the active send response body")

	release()
	select {
	case <-sendResult:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight send did not complete after its response was released")
	}
	require.NoError(t, <-disconnectResult)
	require.EqualValues(t, 1, transport.disconnectCalls.Load())
}

func TestGRPCAdapterConnectErrorPreservesCauseWithoutLeakingEndpointOrToken(t *testing.T) {
	const token = "private-bearer-value"
	const endpointSecret = "private-query-value"
	transportErr := errors.New("transport cause")
	var requestCount atomic.Int32
	client := NewGRPCAdapter("http://127.0.0.1:8123", WithHTTPBearerToken(token))
	adapter := client.(*gRPCAdapter)
	adapter.client.Transport = roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		requestCount.Add(1)
		return nil, fmt.Errorf("failed URL https://user:pass@example.invalid/acp?key=%s token=%s: %w", endpointSecret, token, transportErr)
	})

	err := client.Connect(context.Background())
	require.ErrorIs(t, err, transportErr)
	require.ErrorIs(t, err, ErrHTTPConnectOutcomeUnknown)
	require.NotContains(t, err.Error(), token)
	require.NotContains(t, err.Error(), endpointSecret)
	require.NotContains(t, err.Error(), "user:pass")
	cleanupErr := client.Disconnect(context.Background())
	require.ErrorIs(t, cleanupErr, ErrHTTPConnectOutcomeUnknown)
	require.ErrorIs(t, cleanupErr, transportErr)
	require.NotContains(t, cleanupErr.Error(), token)
	require.ErrorIs(t, client.Connect(context.Background()), ErrHTTPConnectOutcomeUnknown,
		"unknown remote creation must fence another connect attempt")
	require.ErrorIs(t, client.Disconnect(context.Background()), ErrHTTPConnectOutcomeUnknown,
		"later Disconnect returns the same unknown cleanup result without replay")
	require.EqualValues(t, 1, requestCount.Load(), "unknown creation must not issue a second connect or guessed disconnect")
}

func TestHTTPACPStreamReadPanicClosesResponseAndReturnsFixedError(t *testing.T) {
	body := &panicReadBody{}
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/x-ndjson"}},
		Body:          body,
		ContentLength: -1,
	}
	_, retryable, err := readHTTPACPStreamResponse(context.Background(), make(chan ACPMessage), response, "opaque-session")
	require.ErrorIs(t, err, errHTTPBodyReadPanic)
	require.False(t, retryable, "a contained callback panic is terminal rather than retried")
	require.True(t, body.closed.Load(), "the response owner must close after Read panics")
	require.NotContains(t, err.Error(), "secret panic text")
}

func TestHTTPACPStreamKeepsKnownDenialAndUnknownSessionErrorsDistinct(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantErr    error
	}{
		{name: "unauthorized", statusCode: http.StatusUnauthorized, wantErr: errHTTPUnauthorized},
		{name: "forbidden", statusCode: http.StatusForbidden, wantErr: errHTTPForbidden},
		{name: "unknown session", statusCode: http.StatusNotFound, wantErr: errHTTPNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := &http.Response{
				StatusCode:    tt.statusCode,
				Body:          io.NopCloser(strings.NewReader("private response body")),
				ContentLength: -1,
			}
			_, retryable, err := readHTTPACPStreamResponse(context.Background(), make(chan ACPMessage), response, "opaque-session")
			require.ErrorIs(t, err, tt.wantErr)
			require.False(t, retryable, "identity and permission statuses terminate the current generation")
			require.NotContains(t, err.Error(), "private response body")
		})
	}
}

func TestGRPCAdapterUncertainConnectResponsesStayFenced(t *testing.T) {
	readFailure := errors.New("body read failed: token-and-query-secret")
	tests := []struct {
		name       string
		statusCode int
		body       string
		readErr    error
	}{
		{name: "server failure", statusCode: http.StatusInternalServerError, body: `{"version":"go-cli-acp-json/1","session_id":"opaque-maybe-created"}`},
		{name: "unexpected success status", statusCode: http.StatusCreated, body: `{"version":"go-cli-acp-json/1","session_id":"opaque-maybe-created"}`},
		{name: "malformed response", statusCode: http.StatusOK, body: `{"version":`},
		{name: "read failure", statusCode: http.StatusOK, body: `{"version":"go-cli-acp-json/1"}`, readErr: readFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &connectOutcomeTransport{
				statusCode: tt.statusCode,
				body:       &trackedResponseBody{body: io.NopCloser(strings.NewReader(tt.body)), readErr: tt.readErr},
			}
			client := NewGRPCAdapter("http://private-outcome.invalid", WithName("unknown-outcome"))
			adapter := client.(*gRPCAdapter)
			adapter.client.Transport = transport

			err := client.Connect(context.Background())
			require.ErrorIs(t, err, ErrHTTPConnectOutcomeUnknown)
			if tt.readErr != nil {
				require.ErrorIs(t, err, tt.readErr)
				require.NotContains(t, err.Error(), "token-and-query-secret")
			}
			require.True(t, transport.body.closed.Load(), "connect response body must close before the worker completes")
			cleanupErr := client.Disconnect(context.Background())
			require.ErrorIs(t, cleanupErr, ErrHTTPConnectOutcomeUnknown)
			require.ErrorIs(t, client.Connect(context.Background()), ErrHTTPConnectOutcomeUnknown)
			require.EqualValues(t, 1, transport.connectRequests.Load())
			require.Zero(t, transport.disconnectRequests.Load(), "the unknown opaque ID cannot be guessed for cleanup")
		})
	}
}

func TestHTTPResponseClosePanicIsContainedAndSanitized(t *testing.T) {
	const sensitive = "token-and-query-secret"
	closer := &panicCloseBody{message: sensitive}
	err := closeHTTPResponseBody(closer)
	require.ErrorIs(t, err, errHTTPBodyClosePanic)
	require.True(t, closer.called.Load())
	require.NotContains(t, err.Error(), sensitive)
}

func TestHTTPResponseCloseErrorPreservesCauseWithoutLeaking(t *testing.T) {
	const sensitive = "token-and-query-secret"
	closeCause := errors.New("close failed: " + sensitive)
	err := closeHTTPResponseBody(closeErrorBody{err: closeCause})
	require.ErrorIs(t, err, closeCause)
	require.NotContains(t, err.Error(), sensitive)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type countingHTTPTransport struct {
	base     http.RoundTripper
	requests atomic.Int32
}

func (t *countingHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.requests.Add(1)
	return t.base.RoundTrip(request)
}

type connectOutcomeTransport struct {
	statusCode         int
	body               *trackedResponseBody
	connectRequests    atomic.Int32
	disconnectRequests atomic.Int32
}

func (t *connectOutcomeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	switch request.URL.Path {
	case "/connect":
		t.connectRequests.Add(1)
		return &http.Response{
			StatusCode:    t.statusCode,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          t.body,
			ContentLength: -1,
			Request:       request,
		}, nil
	case "/disconnect":
		t.disconnectRequests.Add(1)
		return &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(strings.NewReader("")),
			ContentLength: 0,
			Request:       request,
		}, nil
	case "/stream":
		<-request.Context().Done()
		return nil, request.Context().Err()
	default:
		return nil, fmt.Errorf("unexpected outcome test route %s", request.URL.Path)
	}
}

type trackedResponseBody struct {
	body    io.ReadCloser
	readErr error
	closed  atomic.Bool
}

func (b *trackedResponseBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	return b.body.Read(p)
}

func (b *trackedResponseBody) Close() error {
	b.closed.Store(true)
	return b.body.Close()
}

type lateConnectTransport struct {
	connectStarted       chan struct{}
	releaseConnect       chan struct{}
	disconnectSessionIDs chan string
	disconnectCalls      atomic.Int32
}

type sendGateTransport struct {
	base            http.RoundTripper
	sendStarted     chan struct{}
	releaseSend     chan struct{}
	disconnectCalls atomic.Int32
}

type streamBodyCloseGateTransport struct {
	base            http.RoundTripper
	closeStarted    chan struct{}
	releaseClose    chan struct{}
	streamSeen      atomic.Bool
	disconnectCalls atomic.Int32
}

func (t *streamBodyCloseGateTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return response, err
	}
	if request.URL.Path == "/disconnect" {
		t.disconnectCalls.Add(1)
	}
	if request.URL.Path == "/stream" && t.streamSeen.CompareAndSwap(false, true) {
		response.Body = &gatedCloseBody{
			ReadCloser: response.Body,
			started:    t.closeStarted,
			release:    t.releaseClose,
		}
	}
	return response, nil
}

func (t *sendGateTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path == "/disconnect" {
		t.disconnectCalls.Add(1)
		return t.base.RoundTrip(request)
	}
	response, err := t.base.RoundTrip(request)
	if err != nil || request.URL.Path != "/send" {
		return response, err
	}
	close(t.sendStarted)
	<-t.releaseSend
	return response, nil
}

func (t *lateConnectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	switch request.URL.Path {
	case "/connect":
		close(t.connectStarted)
		<-t.releaseConnect
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(`{"version":"go-cli-acp-json/1","session_id":"opaque-late-session"}`)),
			ContentLength: -1,
			Request:       request,
		}, nil
	case "/stream":
		<-request.Context().Done()
		return nil, request.Context().Err()
	case "/disconnect":
		data, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var envelope httpACPEnvelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			return nil, err
		}
		t.disconnectCalls.Add(1)
		t.disconnectSessionIDs <- envelope.SessionID
		return &http.Response{
			StatusCode:    http.StatusOK,
			Body:          io.NopCloser(bytes.NewReader(nil)),
			ContentLength: 0,
			Request:       request,
		}, nil
	default:
		return nil, fmt.Errorf("unexpected test route %s", request.URL.Path)
	}
}

type panicReadBody struct {
	closed atomic.Bool
}

func (b *panicReadBody) Read([]byte) (int, error) { panic("secret panic text") }

func (b *panicReadBody) Close() error {
	b.closed.Store(true)
	return nil
}

type panicCloseBody struct {
	message string
	called  atomic.Bool
}

func (b *panicCloseBody) Close() error {
	b.called.Store(true)
	panic(b.message)
}

type closeErrorBody struct{ err error }

func (b closeErrorBody) Close() error { return b.err }

type responseCloseGateTransport struct {
	base               http.RoundTripper
	closeStarted       chan struct{}
	releaseClose       <-chan struct{}
	closeErr           error
	disconnectRequests atomic.Int32
}

func (t *responseCloseGateTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(request)
	if err != nil || request.URL.Path != "/disconnect" {
		return response, err
	}
	t.disconnectRequests.Add(1)
	response.Body = &gatedCloseBody{
		ReadCloser: response.Body,
		started:    t.closeStarted,
		release:    t.releaseClose,
		closeErr:   t.closeErr,
	}
	return response, nil
}

type gatedCloseBody struct {
	io.ReadCloser
	started  chan struct{}
	release  <-chan struct{}
	closeErr error
	once     sync.Once
}

func (b *gatedCloseBody) Close() error {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return errors.Join(b.closeErr, b.ReadCloser.Close())
}
