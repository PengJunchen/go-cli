// Package e2e_20260802 contains end-to-end integration tests for the TUI
// (theme, renderer registry, BubbleteaApp lifecycle) and ACP (client, stdio
// adapter, gRPC adapter, middleware bridging) modules.
package e2e_20260802 //nolint:staticcheck

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/extension"
)

// =============================================================================
// ACP: ACPClient connect/disconnect/send/receive (via StdioAdapter)
// =============================================================================

func TestACP_StdioAdapterBidirectionalCommunication(t *testing.T) {
	// Create two connected pipes for bidirectional communication.
	// clientWriter -> serverReader: client sends to server.
	// serverWriter -> clientReader: server sends to client.
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	client := acp.NewStdioAdapter(clientReader, clientWriter, acp.WithName("client-agent"))
	server := acp.NewStdioAdapter(serverReader, serverWriter, acp.WithName("server-agent"))

	ctx := context.Background()

	// Start draining clientReader in a goroutine so that server's Connect
	// write does not block.
	var serverConnectMsg []byte
	readyCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Read the server's connect message that gets written to serverWriter (clientReader).
		buf := make([]byte, 256)
		n, readErr := clientReader.Read(buf)
		if readErr == nil {
			serverConnectMsg = make([]byte, n)
			copy(serverConnectMsg, buf[:n])
		}
		close(readyCh)
	}()

	var wg2 sync.WaitGroup
	wg2.Add(1)
	serverDone := make(chan struct{})
	errCh := make(chan error, 1)

	// Server goroutine.
	go func() {
		defer wg2.Done()
		err := server.Connect(ctx)
		require.NoError(t, err)
		defer server.Disconnect(ctx) //nolint:errcheck,gosec

		msgCh := server.ReceiveMessages()

		// First message should be the client's connect message.
		select {
		case connectMsg := <-msgCh:
			assert.Equal(t, acp.TypeConnect, connectMsg.Type)
			assert.Equal(t, "client-agent", connectMsg.SenderID)
		case <-time.After(5 * time.Second):
			errCh <- fmt.Errorf("timed out waiting for connect message on server")
			return
		}

		// Second message should be the test message.
		select {
		case msg := <-msgCh:
			assert.Equal(t, "client-agent", msg.SenderID)
			assert.Equal(t, "hello from client", msg.Content)
		case <-time.After(5 * time.Second):
			errCh <- fmt.Errorf("timed out waiting for message on server")
			return
		}

		// Send response back.
		reply := acp.ACPMessage{
			Type:       acp.TypeResponse,
			SenderID:   "server-agent",
			ReceiverID: "client-agent",
			Content:    "hello from server",
		}
		sendErr := server.SendMessage(ctx, reply)
		assert.NoError(t, sendErr)

		close(serverDone)
	}()

	// Wait for the server's connect message to be read from clientReader,
	// so client.Connect won't deadlock when writing.
	<-readyCh
	require.NotEmpty(t, serverConnectMsg, "server should have sent a connect message")

	// Now client can connect because the server is already connected
	// and its read loop is consuming serverReader.
	err := client.Connect(ctx)
	require.NoError(t, err)
	defer client.Disconnect(ctx) //nolint:errcheck,gosec

	sendMsg := acp.ACPMessage{
		Type:       acp.TypeMessage,
		SenderID:   "client-agent",
		ReceiverID: "server-agent",
		Content:    "hello from client",
	}
	err = client.SendMessage(ctx, sendMsg)
	require.NoError(t, err)

	// Receive response from server.
	clientCh := client.ReceiveMessages()
	select {
	case response := <-clientCh:
		assert.Equal(t, "server-agent", response.SenderID)
		assert.Equal(t, "hello from server", response.Content)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for response on client")
	}

	<-serverDone
	wg.Wait()
	wg2.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
}

func TestACP_StdioAdapterNameDefault(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close() //nolint:errcheck,gosec
	defer w.Close() //nolint:errcheck,gosec

	adapter := acp.NewStdioAdapter(r, w)
	assert.Equal(t, "stdio", adapter.Name())
}

func TestACP_StdioAdapterNameCustom(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close() //nolint:errcheck,gosec
	defer w.Close() //nolint:errcheck,gosec

	adapter := acp.NewStdioAdapter(r, w, acp.WithName("my-agent"))
	assert.Equal(t, "my-agent", adapter.Name())
}

func TestACP_StdioAdapterSendBeforeConnect(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close() //nolint:errcheck,gosec
	defer w.Close() //nolint:errcheck,gosec

	adapter := acp.NewStdioAdapter(r, w)
	msg := acp.ACPMessage{Type: acp.TypeMessage, SenderID: "a", Content: "msg"}
	err := adapter.SendMessage(context.Background(), msg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

func TestACP_StdioAdapterReceiveBeforeConnect(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close() //nolint:errcheck,gosec
	defer w.Close() //nolint:errcheck,gosec

	adapter := acp.NewStdioAdapter(r, w)
	ch := adapter.ReceiveMessages()
	assert.Nil(t, ch)
}

func TestACP_StdioAdapterConnectDisconnect(t *testing.T) {
	adapterIn, peerIn := io.Pipe()
	peerOut, adapterOut := io.Pipe()
	t.Cleanup(func() {
		_ = adapterIn.Close()
		_ = peerIn.Close()
		_ = peerOut.Close()
		_ = adapterOut.Close()
	})

	// The peer reads both frames and closes its owned input after observing the
	// disconnect frame. That real EOF lets the adapter join its reader worker.
	peerErrCh := make(chan error, 1)
	go func() {
		defer func() {
			_ = peerIn.Close()
			_ = peerOut.Close()
		}()
		reader := bufio.NewReader(peerOut)
		for _, expected := range []string{acp.TypeConnect, acp.TypeDisconnect} {
			frame, err := reader.ReadString('\n')
			if err != nil {
				peerErrCh <- err
				return
			}
			var message acp.ACPMessage
			if err := json.Unmarshal([]byte(frame), &message); err != nil {
				peerErrCh <- fmt.Errorf("decode stdio frame: %w", err)
				return
			}
			if message.Type != expected {
				peerErrCh <- fmt.Errorf("expected frame type %q, got %q", expected, message.Type)
				return
			}
		}
		peerErrCh <- nil
	}()

	adapter := acp.NewStdioAdapter(adapterIn, adapterOut)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := adapter.Connect(ctx)
	assert.NoError(t, err)

	err = adapter.Disconnect(ctx)
	assert.NoError(t, err)
	select {
	case err := <-peerErrCh:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatalf("stdio peer did not finish before deadline: %v", ctx.Err())
	}
}

// =============================================================================
// ACP: gRPC adapter
// =============================================================================

func TestACP_GRPCAdapterName(t *testing.T) {
	adapter := acp.NewGRPCAdapter("http://localhost:9/acp", acp.WithName("grpc-agent"))
	assert.Equal(t, "grpc-agent", adapter.Name())
}

func TestACP_GRPCAdapterNameDefault(t *testing.T) {
	adapter := acp.NewGRPCAdapter("http://localhost:9/acp")
	assert.Equal(t, "grpc", adapter.Name())
}

func TestACP_GRPCAdapterSendBeforeConnect(t *testing.T) {
	adapter := acp.NewGRPCAdapter("http://localhost:9/acp")
	msg := acp.ACPMessage{Type: acp.TypeMessage, SenderID: "a", Content: "msg"}
	err := adapter.SendMessage(context.Background(), msg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

// =============================================================================
// ACP: ACPMiddleware bridging
// =============================================================================

func TestACP_ACPMiddlewarePassthrough(t *testing.T) {
	mw := acp.NewACPMiddleware("bridge", nil)

	next := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: "result: " + input.Message}, nil
	}

	wrapped := mw.WrapAgent(next)

	// Non-ACPMessage data passes through.
	out, err := wrapped(context.Background(), extension.AgentInput{
		Message: "hello",
		Data:    "plain-string",
	})
	assert.NoError(t, err)
	assert.Equal(t, "result: hello", out.Text)
}

func TestACP_ACPMiddlewareConvertsMessage(t *testing.T) {
	mw := acp.NewACPMiddleware("bridge", nil)

	next := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: "processed: " + input.Message}, nil
	}

	wrapped := mw.WrapAgent(next)

	acpMsg := acp.ACPMessage{
		Type:       acp.TypeMessage,
		SenderID:   "sender",
		ReceiverID: "receiver",
		Content:    "acp content",
		Metadata:   map[string]string{"key": "value"},
	}

	out, err := wrapped(context.Background(), extension.AgentInput{
		Message: "ignored",
		Data:    acpMsg,
	})
	assert.NoError(t, err)
	assert.Equal(t, "processed: acp content", out.Text)
}

func TestACP_ACPMiddlewareNonMessagePassesThrough(t *testing.T) {
	mw := acp.NewACPMiddleware("bridge", nil)

	next := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: input.Message}, nil
	}

	wrapped := mw.WrapAgent(next)

	// An ACP message of type "connect" should pass through.
	acpMsg := acp.ACPMessage{
		Type:    acp.TypeConnect,
		Content: "should be ignored",
	}

	out, err := wrapped(context.Background(), extension.AgentInput{
		Message: "raw message",
		Data:    acpMsg,
	})
	assert.NoError(t, err)
	assert.Equal(t, "raw message", out.Text)
}

func TestACP_DefaultACPServer(t *testing.T) {
	srv := acp.NewDefaultACPServer("test-server")
	assert.Equal(t, "test-server", srv.Name())

	// Cast to concrete type to check Running.
	ds, ok := srv.(*acp.DefaultACPServer)
	require.True(t, ok)

	assert.False(t, ds.Running())

	err := srv.Start(context.Background())
	assert.NoError(t, err)
	assert.True(t, ds.Running())

	err = srv.Stop(context.Background())
	assert.NoError(t, err)
	assert.False(t, ds.Running())
}

func TestACP_ACPMessageStruct(t *testing.T) {
	now := time.Now()
	msg := acp.ACPMessage{
		Type:       acp.TypeMessage,
		SenderID:   "agent-a",
		ReceiverID: "agent-b",
		Content:    "test message",
		Metadata:   map[string]string{"k": "v"},
		Timestamp:  now,
	}

	assert.Equal(t, acp.TypeMessage, msg.Type)
	assert.Equal(t, "agent-a", msg.SenderID)
	assert.Equal(t, "agent-b", msg.ReceiverID)
	assert.Equal(t, "test message", msg.Content)
	assert.Equal(t, "v", msg.Metadata["k"])
	assert.Equal(t, now, msg.Timestamp)
}

func TestACP_ACPTransportString(t *testing.T) {
	assert.Equal(t, "gRPC", acp.ACPTransportGRPC.String())
	assert.Equal(t, "Stdio", acp.ACPTransportStdio.String())
}
