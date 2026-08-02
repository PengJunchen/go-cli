package acp

import (
	"context"
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
