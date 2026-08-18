package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newGRPCPeer starts a MockACPServer in gRPC (JSON-over-HTTP) mode and returns
// a client wired to it.
func newGRPCPeer(t *testing.T) (ACPClient, *MockACPServer) {
	t.Helper()
	mock := NewMockACPServer("srv", ACPTransportGRPC)
	require.NoError(t, mock.Start(context.Background()))
	t.Cleanup(func() { _ = mock.Stop(context.Background()) }) //nolint:errcheck // best-effort cleanup
	client := NewGRPCAdapter(mock.Endpoint(), WithName("grpc-client"))
	return client, mock
}

func TestGRPCAdapterConnectDisconnectLifecycle(t *testing.T) {
	client, mock := newGRPCPeer(t)
	assert.Equal(t, "grpc-client", client.Name())
	assert.Equal(t, "srv", mock.Name())

	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))
	require.NoError(t, client.Connect(ctx)) // idempotent
	require.NoError(t, client.Disconnect(ctx))
	require.NoError(t, client.Disconnect(ctx)) // safe no-op
}

func TestGRPCAdapterSendReceiveRoundTrip(t *testing.T) {
	client, mock := newGRPCPeer(t)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))

	outbound := ACPMessage{
		Type:       TypeMessage,
		SenderID:   "grpc-client",
		ReceiverID: "srv",
		Content:    "hello over http",
		Metadata:   map[string]string{"proto": "json-over-http"},
	}
	require.NoError(t, client.SendMessage(ctx, outbound))

	select {
	case got := <-client.ReceiveMessages():
		assert.Equal(t, TypeResponse, got.Type)
		assert.Equal(t, "echo:hello over http", got.Content)
		assert.Equal(t, "srv", got.SenderID)
		assert.Equal(t, "grpc-client", got.ReceiverID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reply")
	}

	var found ACPMessage
	for _, m := range mock.Received() {
		if m.Type == TypeMessage {
			found = m
		}
	}
	assert.Equal(t, "grpc-client", found.SenderID)
	assert.Equal(t, "srv", found.ReceiverID)
	assert.Equal(t, "hello over http", found.Content)
	assert.Equal(t, "json-over-http", found.Metadata["proto"])
}

func TestGRPCAdapterSendBeforeConnectFails(t *testing.T) {
	mock := NewMockACPServer("srv", ACPTransportGRPC)
	require.NoError(t, mock.Start(context.Background()))
	t.Cleanup(func() { _ = mock.Stop(context.Background()) }) //nolint:errcheck // best-effort cleanup

	client := NewGRPCAdapter(mock.Endpoint())
	err := client.SendMessage(context.Background(), ACPMessage{Type: TypeMessage})
	require.Error(t, err)
	assert.Nil(t, client.ReceiveMessages())
}

func TestGRPCAdapterConnectEmitsSpan(t *testing.T) {
	client, _ := newGRPCPeer(t)
	ctx, exporter, _ := newTraceContext(t, "trace-grpc-connect")
	require.NoError(t, client.Connect(ctx))

	span := waitForSpan(t, exporter, "acp.connect")
	assert.Equal(t, "gRPC", spanAttr(span, "transport"))
	assert.Equal(t, "trace-grpc-connect", span.TraceID)
}

func TestGRPCAdapterSendEmitsSpan(t *testing.T) {
	client, _ := newGRPCPeer(t)
	ctx, exporter, _ := newTraceContext(t, "trace-grpc-send")
	require.NoError(t, client.Connect(ctx))
	require.NoError(t, client.SendMessage(ctx, ACPMessage{
		Type:       TypeMessage,
		SenderID:   "grpc-client",
		ReceiverID: "srv",
		Content:    "spanned",
	}))

	span := waitForSpan(t, exporter, "acp.send")
	assert.Equal(t, "message", spanAttr(span, "message_type"))
	assert.Equal(t, "srv", spanAttr(span, "receiver_id"))
}

func TestGRPCAdapterTraceChainConsistent(t *testing.T) {
	client, _ := newGRPCPeer(t)
	ctx, exporter, rootID := newTraceContext(t, "trace-grpc-chain")
	require.NoError(t, client.Connect(ctx))
	require.NoError(t, client.SendMessage(ctx, ACPMessage{
		Type:       TypeMessage,
		SenderID:   "grpc-client",
		ReceiverID: "srv",
		Content:    "chain",
	}))

	connectSpan := waitForSpan(t, exporter, "acp.connect")
	sendSpan := waitForSpan(t, exporter, "acp.send")
	assert.Equal(t, "trace-grpc-chain", connectSpan.TraceID)
	assert.Equal(t, "trace-grpc-chain", sendSpan.TraceID)
	assert.Equal(t, rootID, connectSpan.ParentSpanID)
	assert.Equal(t, rootID, sendSpan.ParentSpanID)
}

// ---------------------------------------------------------------------------
// gRPC auto-reconnection tests (MD-9)
// ---------------------------------------------------------------------------

// TestGRPCReconnectBackoff verifies the exponential backoff schedule:
// 1s, 2s, 4s, 8s, 16s, 30s (capped).
func TestGRPCReconnectBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second}, // capped
		{6, 30 * time.Second}, // still capped
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, grpcReconnectBackoff(tt.attempt))
	}
}

// controllableGRPCServer is a minimal ACP HTTP server whose availability can
// be toggled at runtime to simulate connection drops and recoveries.
type controllableGRPCServer struct {
	mu        sync.Mutex
	available bool
	pending   []ACPMessage
	received  []ACPMessage
}

func (s *controllableGRPCServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if !s.available {
		s.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	switch r.URL.Path {
	case "/connect", "/disconnect":
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case "/send":
		var msg ACPMessage
		if err := json.NewDecoder(r.Body).Decode(&msg); err == nil {
			s.received = append(s.received, msg)
			if msg.Type == TypeMessage {
				s.pending = append(s.pending, ACPMessage{
					Type:       TypeResponse,
					SenderID:   "srv",
					ReceiverID: msg.SenderID,
					Content:    "echo:" + msg.Content,
					Timestamp:  msg.Timestamp,
				})
			}
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case "/stream":
		out := s.pending
		s.pending = nil
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		for _, msg := range out {
			data, _ := json.Marshal(msg)
			fmt.Fprintf(w, "%s\n", data) //nolint:errcheck // best-effort stream write
		}
	default:
		s.mu.Unlock()
		http.NotFound(w, r)
	}
}

func (s *controllableGRPCServer) setAvailable(v bool) {
	s.mu.Lock()
	s.available = v
	s.mu.Unlock()
}

func (s *controllableGRPCServer) hasReceived(content string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.received {
		if m.Content == content {
			return true
		}
	}
	return false
}

// newControllableGRPCPeer starts a controllableGRPCServer behind an
// httptest.Server and returns the server, its controller, and a client wired
// to it.
func newControllableGRPCPeer(t *testing.T) (*httptest.Server, *controllableGRPCServer, ACPClient) {
	t.Helper()
	ctrl := &controllableGRPCServer{available: true}
	srv := httptest.NewServer(http.HandlerFunc(ctrl.handler))
	t.Cleanup(srv.Close)
	client := NewGRPCAdapter(srv.URL, WithName("grpc-reconnect"))
	return srv, ctrl, client
}

// TestGRPCAdapterReconnectAfterDrop verifies that the adapter automatically
// reconnects with exponential backoff after the server becomes unavailable and
// then resumes normal send/receive operation once the server is back.
func TestGRPCAdapterReconnectAfterDrop(t *testing.T) {
	_, ctrl, client := newControllableGRPCPeer(t)
	adapter := client.(*gRPCAdapter)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))

	// Baseline: send and receive works.
	require.NoError(t, client.SendMessage(ctx, ACPMessage{
		Type: TypeMessage, SenderID: "grpc-reconnect", ReceiverID: "srv",
		Content: "before-drop",
	}))
	select {
	case got := <-client.ReceiveMessages():
		assert.Equal(t, "echo:before-drop", got.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("baseline send/receive timed out")
	}

	// Drop the server.
	ctrl.setAvailable(false)

	// Wait for reconnection to start (readLoop detects 503 on /stream).
	require.Eventually(t, func() bool {
		adapter.reconnectMu.Lock()
		defer adapter.reconnectMu.Unlock()
		return adapter.reconnecting
	}, 2*time.Second, 10*time.Millisecond, "expected reconnection to start")

	// Restore the server before the first reconnection attempt (1s backoff).
	ctrl.setAvailable(true)

	// Wait for reconnection to succeed.
	require.Eventually(t, func() bool {
		adapter.reconnectMu.Lock()
		defer adapter.reconnectMu.Unlock()
		return !adapter.reconnecting
	}, 5*time.Second, 10*time.Millisecond, "expected reconnection to succeed")

	// Verify send/receive works after reconnection.
	require.NoError(t, client.SendMessage(ctx, ACPMessage{
		Type: TypeMessage, SenderID: "grpc-reconnect", ReceiverID: "srv",
		Content: "after-reconnect",
	}))
	select {
	case got := <-client.ReceiveMessages():
		assert.Equal(t, "echo:after-reconnect", got.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reply after reconnection")
	}

	_ = client.Disconnect(ctx) //nolint:errcheck // best-effort cleanup
}

// TestGRPCAdapterQueuesMessagesDuringReconnection verifies that messages sent
// while reconnection is in progress are queued and flushed after the
// connection is re-established.
func TestGRPCAdapterQueuesMessagesDuringReconnection(t *testing.T) {
	_, ctrl, client := newControllableGRPCPeer(t)
	adapter := client.(*gRPCAdapter)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))

	// Drop the server.
	ctrl.setAvailable(false)

	// Wait for reconnection to start.
	require.Eventually(t, func() bool {
		adapter.reconnectMu.Lock()
		defer adapter.reconnectMu.Unlock()
		return adapter.reconnecting
	}, 2*time.Second, 10*time.Millisecond, "expected reconnection to start")

	// Send a message while reconnecting — it should be queued, not sent.
	require.NoError(t, client.SendMessage(ctx, ACPMessage{
		Type: TypeMessage, SenderID: "grpc-reconnect", ReceiverID: "srv",
		Content: "queued-msg",
	}))

	// The message must not have reached the server yet.
	assert.False(t, ctrl.hasReceived("queued-msg"),
		"message should be queued locally, not sent to the server")

	// Restore the server.
	ctrl.setAvailable(true)

	// Wait for reconnection to succeed.
	require.Eventually(t, func() bool {
		adapter.reconnectMu.Lock()
		defer adapter.reconnectMu.Unlock()
		return !adapter.reconnecting
	}, 5*time.Second, 10*time.Millisecond, "expected reconnection to succeed")

	// The queued message should have been flushed to the server.
	require.Eventually(t, func() bool {
		return ctrl.hasReceived("queued-msg")
	}, 2*time.Second, 10*time.Millisecond,
		"queued message should be flushed after reconnection")

	_ = client.Disconnect(ctx) //nolint:errcheck // best-effort cleanup
}

// TestGRPCAdapterReconnectDisconnectCancels verifies that calling Disconnect
// during reconnection terminates the reconnection loop cleanly.
func TestGRPCAdapterReconnectDisconnectCancels(t *testing.T) {
	_, ctrl, client := newControllableGRPCPeer(t)
	adapter := client.(*gRPCAdapter)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))

	// Drop the server to trigger reconnection.
	ctrl.setAvailable(false)
	require.Eventually(t, func() bool {
		adapter.reconnectMu.Lock()
		defer adapter.reconnectMu.Unlock()
		return adapter.reconnecting
	}, 2*time.Second, 10*time.Millisecond, "expected reconnection to start")

	// Disconnect while reconnecting — should not hang.
	done := make(chan struct{})
	go func() {
		_ = client.Disconnect(ctx) //nolint:errcheck // best-effort cleanup
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect hung during reconnection")
	}

	// After Disconnect, reconnecting should be false.
	adapter.reconnectMu.Lock()
	reconnecting := adapter.reconnecting
	adapter.reconnectMu.Unlock()
	assert.False(t, reconnecting, "reconnecting should be false after Disconnect")
}
