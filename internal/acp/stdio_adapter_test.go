package acp

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// newStdioPeer wires a StdioAdapter to a MockACPServer over two in-memory pipe
// pairs so the two sides can exchange newline-delimited JSON. It registers a
// cleanup that closes every pipe end so no receiver goroutine leaks.
func newStdioPeer(t *testing.T) (ACPClient, *MockACPServer) {
	t.Helper()
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()

	mock := NewMockACPServer("srv", ACPTransportStdio)
	mock.SetIO(serverR, serverW, nil)
	require.NoError(t, mock.Start(context.Background()))

	client := NewStdioAdapter(clientR, clientW, WithName("client"))

	t.Cleanup(func() {
		closeIgnored(serverR)
		closeIgnored(clientW)
		closeIgnored(serverW)
		closeIgnored(clientR)
	})
	return client, mock
}

func TestStdioAdapterConnectDisconnectLifecycle(t *testing.T) {
	client, mock := newStdioPeer(t)
	assert.Equal(t, "client", client.Name())
	assert.Equal(t, "srv", mock.Name())

	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))
	// A second connect is idempotent.
	require.NoError(t, client.Connect(ctx))
	require.NoError(t, client.Disconnect(ctx))
	// Disconnecting again is a safe no-op.
	require.NoError(t, client.Disconnect(ctx))
}

func TestStdioAdapterSendReceiveRoundTrip(t *testing.T) {
	client, mock := newStdioPeer(t)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))

	outbound := ACPMessage{
		Type:       TypeMessage,
		SenderID:   "client",
		ReceiverID: "srv",
		Content:    "hello from stdio",
		Metadata:   map[string]string{"k": "v"},
	}
	require.NoError(t, client.SendMessage(ctx, outbound))

	select {
	case got := <-client.ReceiveMessages():
		assert.Equal(t, TypeResponse, got.Type)
		assert.Equal(t, "echo:hello from stdio", got.Content)
		assert.Equal(t, "srv", got.SenderID)
		assert.Equal(t, "client", got.ReceiverID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reply")
	}

	// The mock must have recorded the sent message (plus the connect frame).
	var found ACPMessage
	for _, m := range mock.Received() {
		if m.Type == TypeMessage {
			found = m
		}
	}
	assert.Equal(t, "client", found.SenderID)
	assert.Equal(t, "srv", found.ReceiverID)
	assert.Equal(t, "hello from stdio", found.Content)
	assert.Equal(t, "v", found.Metadata["k"])
}

func TestStdioAdapterSendBeforeConnectFails(t *testing.T) {
	client := NewStdioAdapter(nil, nil)
	ctx := context.Background()
	err := client.SendMessage(ctx, ACPMessage{Type: TypeMessage})
	require.Error(t, err)
	// Not connected: ReceiveMessages is nil.
	assert.Nil(t, client.ReceiveMessages())
}

func TestStdioAdapterSendAfterDisconnectFails(t *testing.T) {
	client, _ := newStdioPeer(t)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))
	require.NoError(t, client.Disconnect(ctx))

	err := client.SendMessage(ctx, ACPMessage{Type: TypeMessage, SenderID: "client", ReceiverID: "srv"})
	require.Error(t, err)
	assert.Nil(t, client.ReceiveMessages())
}

func TestStdioAdapterConnectEmitsSpan(t *testing.T) {
	client, _ := newStdioPeer(t)
	ctx, exporter, _ := newTraceContext(t, "trace-stdio-connect")
	require.NoError(t, client.Connect(ctx))

	span := waitForSpan(t, exporter, "acp.connect")
	assert.Equal(t, "Stdio", spanAttr(span, "transport"))
	assert.Equal(t, "stdio://stdin-stdout", spanAttr(span, "endpoint"))
}

func TestStdioAdapterSendEmitsSpan(t *testing.T) {
	client, _ := newStdioPeer(t)
	ctx, exporter, _ := newTraceContext(t, "trace-stdio-send")
	require.NoError(t, client.Connect(ctx))
	require.NoError(t, client.SendMessage(ctx, ACPMessage{
		Type:       TypeMessage,
		SenderID:   "client",
		ReceiverID: "srv",
		Content:    "spanned",
	}))

	span := waitForSpan(t, exporter, "acp.send")
	assert.Equal(t, "message", spanAttr(span, "message_type"))
	assert.Equal(t, "srv", spanAttr(span, "receiver_id"))
}

func TestStdioAdapterTraceChainConsistent(t *testing.T) {
	client, _ := newStdioPeer(t)
	ctx, exporter, rootID := newTraceContext(t, "trace-stdio-chain")
	require.NoError(t, client.Connect(ctx))
	require.NoError(t, client.SendMessage(ctx, ACPMessage{
		Type:       TypeMessage,
		SenderID:   "client",
		ReceiverID: "srv",
		Content:    "chain",
	}))

	connectSpan := waitForSpan(t, exporter, "acp.connect")
	sendSpan := waitForSpan(t, exporter, "acp.send")
	// Both spans share one trace id.
	assert.Equal(t, "trace-stdio-chain", connectSpan.TraceID)
	assert.Equal(t, "trace-stdio-chain", sendSpan.TraceID)
	// Both spans hang off the common root span (parent_span_id traceable).
	assert.Equal(t, rootID, connectSpan.ParentSpanID)
	assert.Equal(t, rootID, sendSpan.ParentSpanID)
}

func TestStdioAdapterDisconnectWithCanceledContext(t *testing.T) {
	client, _ := newStdioPeer(t)
	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err := client.Disconnect(canceled)
	assert.Error(t, err)
	assert.Equal(t, context.Canceled, err)
}

// waitForSpan polls the exporter until a span with the given name is collected.
func waitForSpan(t *testing.T, exporter *recordingExporter, name string) *tracing.SpanData {
	t.Helper()
	require.Eventually(t, func() bool {
		return exporter.spanByName(name) != nil
	}, 2*time.Second, 10*time.Millisecond, "expected span %q", name)
	return exporter.spanByName(name)
}
