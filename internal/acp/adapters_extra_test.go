package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// stubClient is a controllable ACPClient for testing ACPMiddleware relay and
// failure behavior without a live peer.
type stubClient struct {
	name      string
	recv      chan ACPMessage
	sendErr   error
	mu        sync.Mutex
	sent      []ACPMessage
	connected bool
}

func newStubClient() *stubClient {
	return &stubClient{name: "stub", recv: make(chan ACPMessage, 8), connected: true}
}

func (s *stubClient) Connect(ctx context.Context) error {
	s.mu.Lock()
	s.connected = true
	s.mu.Unlock()
	return nil
}
func (s *stubClient) Disconnect(ctx context.Context) error { return nil }
func (s *stubClient) SendMessage(_ context.Context, m ACPMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sendErr != nil {
		return s.sendErr
	}
	s.sent = append(s.sent, m)
	return nil
}
func (s *stubClient) ReceiveMessages() <-chan ACPMessage { return s.recv }
func (s *stubClient) Name() string                       { return s.name }
func (s *stubClient) sentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}
func (s *stubClient) lastSent() ACPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		return ACPMessage{}
	}
	return s.sent[len(s.sent)-1]
}

// TestACPMiddlewareRelaysResponse validates that after the AgentFunc runs, a
// TypeResponse is relayed back through the client with swapped sender/receiver
// and the produced text.
func TestACPMiddlewareRelaysResponse(t *testing.T) {
	client := newStubClient()
	m := NewACPMiddleware("acp-bridge", client)
	next := func(_ context.Context, in extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: "result:" + in.Message}, nil
	}

	out, err := m.WrapAgent(next)(context.Background(), extension.AgentInput{
		Data: ACPMessage{Type: TypeMessage, SenderID: "peer", ReceiverID: "me", Content: "hi"},
	})
	require.NoError(t, err)
	assert.Equal(t, "result:hi", out.Text)
	require.Equal(t, 1, client.sentCount())

	reply := client.lastSent()
	assert.Equal(t, TypeResponse, reply.Type)
	assert.Equal(t, "peer", reply.ReceiverID)
	assert.Equal(t, "me", reply.SenderID)
	assert.Equal(t, "result:hi", reply.Content)
	assert.False(t, reply.Timestamp.IsZero())
}

// TestACPMiddlewareRelaySendError propagates a client SendMessage failure.
func TestACPMiddlewareRelaySendError(t *testing.T) {
	client := newStubClient()
	client.sendErr = errors.New("peer went away")
	m := NewACPMiddleware("acp-bridge", client)
	next := func(_ context.Context, in extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: "ok"}, nil
	}

	_, err := m.WrapAgent(next)(context.Background(), extension.AgentInput{
		Data: ACPMessage{Type: TypeMessage, SenderID: "peer", Content: "hi"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "peer went away")
}

// TestACPMiddlewareForwardsAgentError verifies an error from the wrapped
// AgentFunc is returned unchanged and no reply is relayed.
func TestACPMiddlewareForwardsAgentError(t *testing.T) {
	client := newStubClient()
	m := NewACPMiddleware("acp-bridge", client)
	next := func(_ context.Context, _ extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{}, errors.New("agent failed")
	}

	_, err := m.WrapAgent(next)(context.Background(), extension.AgentInput{
		Data: ACPMessage{Type: TypeMessage, SenderID: "p", Content: "x"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "agent failed")
	assert.Equal(t, 0, client.sentCount(), "no reply should be relayed when the agent fails")
}

// TestACPMiddlewarePassesThroughNonMessageType verifies that an ACPMessage
// whose Type is not TypeMessage is passed through unchanged (no conversion).
func TestACPMiddlewarePassesThroughNonMessageType(t *testing.T) {
	client := newStubClient()
	m := NewACPMiddleware("acp-bridge", client)

	var in extension.AgentInput
	next := func(_ context.Context, i extension.AgentInput) (extension.AgentOutput, error) {
		in = i
		return extension.AgentOutput{Text: "passthrough"}, nil
	}
	origInput := extension.AgentInput{
		Message: "raw-surface",
		Data:    ACPMessage{Type: TypeAck, SenderID: "p", Content: "meta-content"},
	}
	out, err := m.WrapAgent(next)(context.Background(), origInput)
	require.NoError(t, err)
	assert.Equal(t, "passthrough", out.Text)
	// The AgentFunc must receive the original input untouched.
	assert.Equal(t, origInput, in, "non-message ACP data must pass through unchanged")
	assert.Equal(t, 0, client.sentCount(), "no reply should be relayed for a pass-through")
}

// TestResolveNameAppliesOptions verifies option resolution order.
func TestResolveNameAppliesOptions(t *testing.T) {
	assert.Equal(t, "default", resolveName("default", nil))
	assert.Equal(t, "custom", resolveName("default", []Option{WithName("custom")}))
	// Last option wins.
	assert.Equal(t, "custom2", resolveName("default", []Option{WithName("custom1"), WithName("custom2")}))
}

// TestDebugMessageDoesNotPanic exercises the debug logging helper.
func TestDebugMessageDoesNotPanic(t *testing.T) {
	msg := ACPMessage{Type: TypeMessage, SenderID: "a", ReceiverID: "b"}
	assert.NotPanics(t, func() {
		debugMessage(context.Background(), "acp.receive", msg)
	})
}

// TestGRPCAdapterConnectToFailingEndpoint verifies connect surfaces the remote
// error when the peer rejects it.
func TestGRPCAdapterConnectToFailingEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := NewGRPCAdapter(srv.URL, WithName("grpc-fail"))
	err := client.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 503")
	assert.Nil(t, client.ReceiveMessages(), "not connected after a failed connect")
}

// TestGRPCAdapterDisconnectStopsReceive verifies that after Disconnect the
// inbound channel is no longer exposed and sending fails.
func TestGRPCAdapterDisconnectStopsReceive(t *testing.T) {
	client, _ := newGRPCPeer(t)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))
	require.NotNil(t, client.ReceiveMessages(), "must expose inbound while connected")

	require.NoError(t, client.Disconnect(ctx))
	assert.Nil(t, client.ReceiveMessages(), "no inbound after disconnect")

	err := client.SendMessage(ctx, ACPMessage{Type: TypeMessage, ReceiverID: "srv"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

// TestGRPCAdapterConcurrentConnect verifies Connect is safe under concurrent
// callers (the mock tolerates an already-started server).
func TestGRPCAdapterConcurrentConnect(t *testing.T) {
	client, _ := newGRPCPeer(t)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = client.Connect(ctx)
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i], "concurrent connect must succeed")
	}
	require.NoError(t, client.Disconnect(ctx))
}

// TestGRPCAdapterConnectContextCancellation verifies a canceled context fails
// the connect HTTP call.
func TestGRPCAdapterConnectContextCancellation(t *testing.T) {
	// Use a slow server so the canceled request aborts in transit.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewGRPCAdapter(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := client.Connect(ctx)
	require.Error(t, err, "a canceled connect context must fail")
}

// TestStdioAdapterDisconnectWritesFrame verifies Disconnect emits a disconnect
// frame to the peer before tearing down.
func TestStdioAdapterDisconnectWritesFrame(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		closeIgnored(serverR)
		closeIgnored(clientW)
		closeIgnored(serverW)
		closeIgnored(clientR)
	})

	client := NewStdioAdapter(clientR, clientW)
	frames := make(chan ACPMessage, 8)
	go func() {
		for {
			line, err := readLineLen(serverR)
			if err != nil {
				close(frames)
				return
			}
			var m ACPMessage
			if json.Unmarshal(line, &m) == nil {
				frames <- m
			}
		}
	}()

	require.NoError(t, client.Connect(context.Background()))
	require.NoError(t, client.Disconnect(context.Background()))

	// Collect frames until a disconnect is observed or we time out.
	deadline := time.After(2 * time.Second)
	sawDisconnect := false
Loop:
	for {
		select {
		case m, open := <-frames:
			if !open {
				break Loop
			}
			if m.Type == TypeDisconnect {
				sawDisconnect = true
				break Loop
			}
		case <-deadline:
			break Loop
		}
	}
	assert.True(t, sawDisconnect, "expected a disconnect frame to be written")
}

// TestACPMessageMetadataOmitted verifies Metadata is omitted from JSON when nil
// but present when set.
func TestACPMessageMetadataOmitted(t *testing.T) {
	noMeta := ACPMessage{Type: TypeMessage, SenderID: "a", ReceiverID: "b", Content: "x"}
	data, err := json.Marshal(noMeta)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "metadata", "nil metadata must be omitted from the wire")

	withMeta := noMeta
	withMeta.Metadata = map[string]string{"k": "v"}
	data, err = json.Marshal(withMeta)
	require.NoError(t, err)
	assert.Contains(t, string(data), "metadata")
}

// TestGRPCAdapterSendRouteReturnsError verifies a non-2xx /send response is
// surfaced and no reply is expected.
func TestGRPCAdapterSendRouteReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/connect" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	client := NewGRPCAdapter(srv.URL)
	require.NoError(t, client.Connect(context.Background()))
	err := client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage, ReceiverID: "srv"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 400")
}

// TestGRPCAdapterUnreachableEndpoint verifies dialing an unused port fails the
// operation without hanging the caller.
func TestGRPCAdapterUnreachableEndpoint(t *testing.T) {
	// Grab an ephemeral port, then free it so connections are refused.
	ln := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadAddr := ln.URL
	ln.Close()

	client := NewGRPCAdapter(deadAddr)
	start := time.Now()
	err := client.Connect(context.Background())
	require.Error(t, err)
	require.Less(t, time.Since(start), 2*time.Second, "connect to a refused port must not hang")
}

// TestStdioAdapterMalformedInputSkipped verifies the receiver skips non-JSON
// lines while still delivering well-formed messages.
func TestStdioAdapterMalformedInputSkipped(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		closeIgnored(serverR)
		closeIgnored(clientW)
		closeIgnored(serverW)
		closeIgnored(clientR)
	})

	client := NewStdioAdapter(clientR, clientW, WithName("client"))

	// Drain the connect frame the adapter writes on clientW (peer reads
	// serverR). Must be ready before Connect, whose pipe write blocks until the
	// other end is read.
	var connectLine []byte
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		connectLine, _ = readLineLen(serverR) //nolint:errcheck
	}()

	require.NoError(t, client.Connect(context.Background()))

	// Write a malformed line followed by a valid message to the adapter's input
	// (peer writes serverW, which feeds clientR).
	_, err := serverW.Write([]byte("this is not json\n"))
	require.NoError(t, err)
	valid := ACPMessage{Type: TypeMessage, SenderID: "peer", ReceiverID: "client", Content: "valid"}
	payload, err := json.Marshal(valid)
	require.NoError(t, err)
	_, err = serverW.Write(append(payload, '\n'))
	require.NoError(t, err)

	select {
	case got := <-client.ReceiveMessages():
		assert.Equal(t, "valid", got.Content)
		assert.Equal(t, "peer", got.SenderID)
	case <-time.After(2 * time.Second):
		t.Fatal("expected the valid message to be delivered despite a prior malformed line")
	}
	<-readDone
	require.NotEmpty(t, connectLine, "expected a connect frame on startup")
}

// readLineLen reads one newline-delimited line from an io.Reader.
func readLineLen(r io.Reader) ([]byte, error) {
	var buf []byte
	tmp := make([]byte, 1)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if tmp[0] == '\n' {
				return buf, nil
			}
		}
		if err != nil {
			return buf, err
		}
	}
}

// TestStdioAdapterConnectWriteFailure verifies a failing writer surfaces the
// connect error and leaves the adapter disconnected.
func TestStdioAdapterConnectWriteFailure(t *testing.T) {
	client := NewStdioAdapter(nil, errorWriter{})
	err := client.Connect(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write")
	assert.Nil(t, client.ReceiveMessages())
}

// errorWriter always returns an error on Write.
type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write blocked") }

// TestStdioAdapterConcurrentSendReceiver verifies concurrent SendMessage calls
// do not race while the receiver goroutine is decoding inbound frames.
func TestStdioAdapterConcurrentSendReceiver(t *testing.T) {
	client, _ := newStdioPeer(t)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))

	// Drain inbound replies so the mock peer is never blocked writing back,
	// which would otherwise stall the concurrent sends.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		recv := client.ReceiveMessages()
		for range recv {
		}
	}()

	var wg sync.WaitGroup
	const senders = 4
	const perSender = 20
	wg.Add(senders)
	for i := 0; i < senders; i++ {
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perSender; j++ {
				require.NoError(t, client.SendMessage(ctx, ACPMessage{
					Type: TypeMessage, SenderID: "client", ReceiverID: "srv",
					Content: "c",
				}))
			}
		}(i)
	}
	wg.Wait()
	require.NoError(t, client.Disconnect(ctx))
	<-drainDone
}
