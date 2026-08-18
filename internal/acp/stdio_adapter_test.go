package acp

import (
	"bufio"
	"context"
	"encoding/json"
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

// TestStdioAdapterDisconnectUnreadPipeReturns verifies that Disconnect returns
// within a reasonable time even when the peer never reads from the output pipe
// (io.Pipe writes block until read). This guards against goroutine leaks that
// would accumulate if the best-effort disconnect write blocked forever.
func TestStdioAdapterDisconnectUnreadPipeReturns(t *testing.T) {
	clientR, serverW := io.Pipe() // client reads, server writes
	serverR, clientW := io.Pipe() // server reads, client writes

	client := NewStdioAdapter(clientR, clientW, WithName("client"))

	// Drain only the connect frame so Connect doesn't block; stop reading
	// afterwards so the disconnect frame write blocks forever.
	go func() {
		rd := bufio.NewReader(serverR)
		_, _ = rd.ReadBytes('\n') //nolint:errcheck // drain connect frame only
	}()

	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))

	// Close the client's input so readLoop's blocked reader unblocks.
	closeIgnored(serverW)

	// Disconnect must return promptly even though the disconnect write blocks.
	done := make(chan error, 1)
	go func() {
		done <- client.Disconnect(ctx)
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect blocked when peer pipe was never read")
	}

	// Cleanup: closing serverR unblocks the leaked inner writeLine goroutine.
	closeIgnored(clientW)
	closeIgnored(serverR)
	closeIgnored(clientR)
}

// TestStdioAdapterDisconnectDeliversFrame verifies that when the peer reads
// normally, the disconnect frame is delivered correctly.
func TestStdioAdapterDisconnectDeliversFrame(t *testing.T) {
	clientR, serverW := io.Pipe() // client reads, server writes
	serverR, clientW := io.Pipe() // server reads, client writes

	client := NewStdioAdapter(clientR, clientW, WithName("client"))

	// Continuously read frames the client sends.
	frames := make(chan []byte, 4)
	go func() {
		rd := bufio.NewReader(serverR)
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				frames <- line
			}
			if err != nil {
				return
			}
		}
	}()

	ctx := context.Background()
	require.NoError(t, client.Connect(ctx))

	// Consume the connect frame.
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for connect frame")
	}

	// Close the client's input so readLoop exits after Disconnect.
	closeIgnored(serverW)

	require.NoError(t, client.Disconnect(ctx))

	// The disconnect frame should arrive promptly.
	select {
	case line := <-frames:
		var msg ACPMessage
		require.NoError(t, json.Unmarshal(line, &msg))
		assert.Equal(t, TypeDisconnect, msg.Type)
		assert.Equal(t, "client", msg.SenderID)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for disconnect frame")
	}

	closeIgnored(clientW)
	closeIgnored(serverR)
	closeIgnored(clientR)
}

// waitForSpan polls the exporter until a span with the given name is collected.
func waitForSpan(t *testing.T, exporter *recordingExporter, name string) *tracing.SpanData {
	t.Helper()
	require.Eventually(t, func() bool {
		return exporter.spanByName(name) != nil
	}, 2*time.Second, 10*time.Millisecond, "expected span %q", name)
	return exporter.spanByName(name)
}
