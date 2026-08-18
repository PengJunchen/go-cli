package acp

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

// TestRPCDispatch verifies that the RPCDispatcher routes a request to a
// registered "echo" handler and that the adapter relays the result back as an
// ACP RPC response.
func TestRPCDispatch(t *testing.T) {
	client := newStubClient()
	mw := NewACPMiddleware("acp-bridge", client)

	d := NewRPCDispatcher()
	d.Register("echo", func(_ context.Context, params json.RawMessage) (any, error) {
		return params, nil
	})

	adapter := NewACPMiddlewareAdapter(mw, nil, client).WithRPCDispatcher(d)
	t.Cleanup(adapter.Close)

	inner := &echoLoop{}
	wrapped := adapter.Wrap(inner)
	_, _ = wrapped.Run(context.Background(), core.Submission{Content: "start"}) //nolint:errcheck

	// Build and send an RPC request through the client's receive channel.
	rpcMsg := RPCMessage{
		JSONRPC: "2.0",
		Method:  "echo",
		Params:  json.RawMessage(`"hello"`),
		ID:      1,
	}
	rpcData, err := json.Marshal(rpcMsg)
	require.NoError(t, err)

	client.recv <- ACPMessage{
		Type:       TypeRPC,
		SenderID:   "peer",
		ReceiverID: "me",
		Content:    string(rpcData),
	}

	// Wait for the RPC response to be relayed back.
	require.Eventually(t, func() bool {
		return client.sentCount() > 0
	}, 2*time.Second, 10*time.Millisecond, "RPC response should be sent")

	reply := client.lastSent()
	assert.Equal(t, TypeRPC, reply.Type)
	assert.Equal(t, "me", reply.SenderID)
	assert.Equal(t, "peer", reply.ReceiverID)
	assert.False(t, reply.Timestamp.IsZero())

	var resp RPCResponse
	require.NoError(t, json.Unmarshal([]byte(reply.Content), &resp))
	assert.Equal(t, "2.0", resp.JSONRPC)
	assert.Equal(t, int64(1), resp.ID)
	assert.Nil(t, resp.Error)
	// The echo handler returns the raw params; when decoded into any, the
	// JSON string "hello" becomes a Go string.
	assert.Equal(t, "hello", resp.Result)
}

// TestRPCMethod verifies that the dispatcher routes to the correct handler
// when multiple methods are registered.
func TestRPCMethod(t *testing.T) {
	d := NewRPCDispatcher()

	var addCalled, subCalled int32
	d.Register("add", func(_ context.Context, _ json.RawMessage) (any, error) {
		atomic.StoreInt32(&addCalled, 1)
		return "add-result", nil
	})
	d.Register("sub", func(_ context.Context, _ json.RawMessage) (any, error) {
		atomic.StoreInt32(&subCalled, 1)
		return "sub-result", nil
	})

	// Dispatch "add" - only the add handler should fire.
	result, err := d.Dispatch(context.Background(), RPCMessage{
		JSONRPC: "2.0",
		Method:  "add",
		ID:      1,
	})
	require.NoError(t, err)
	assert.Equal(t, "add-result", result)
	assert.Equal(t, int32(1), atomic.LoadInt32(&addCalled), "add handler should be invoked")
	assert.Equal(t, int32(0), atomic.LoadInt32(&subCalled), "sub handler should not be invoked")

	// Dispatch "sub" - only the sub handler should fire.
	result, err = d.Dispatch(context.Background(), RPCMessage{
		JSONRPC: "2.0",
		Method:  "sub",
		ID:      2,
	})
	require.NoError(t, err)
	assert.Equal(t, "sub-result", result)
	assert.Equal(t, int32(1), atomic.LoadInt32(&subCalled), "sub handler should be invoked")
}

// TestRPCError verifies that dispatching an unregistered method through the
// adapter produces a response with error code -32601 (method not found).
func TestRPCError(t *testing.T) {
	client := newStubClient()
	mw := NewACPMiddleware("acp-bridge", client)

	d := NewRPCDispatcher() // no handlers registered

	adapter := NewACPMiddlewareAdapter(mw, nil, client).WithRPCDispatcher(d)
	t.Cleanup(adapter.Close)

	inner := &echoLoop{}
	wrapped := adapter.Wrap(inner)
	_, _ = wrapped.Run(context.Background(), core.Submission{Content: "start"}) //nolint:errcheck

	rpcMsg := RPCMessage{
		JSONRPC: "2.0",
		Method:  "nonexistent",
		ID:      42,
	}
	rpcData, err := json.Marshal(rpcMsg)
	require.NoError(t, err)

	client.recv <- ACPMessage{
		Type:       TypeRPC,
		SenderID:   "peer",
		ReceiverID: "me",
		Content:    string(rpcData),
	}

	require.Eventually(t, func() bool {
		return client.sentCount() > 0
	}, 2*time.Second, 10*time.Millisecond, "error response should be sent")

	reply := client.lastSent()
	assert.Equal(t, TypeRPC, reply.Type)

	var resp RPCResponse
	require.NoError(t, json.Unmarshal([]byte(reply.Content), &resp))
	assert.Equal(t, int64(42), resp.ID)
	require.NotNil(t, resp.Error)
	assert.Equal(t, RPCCodeMethodNotFound, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "nonexistent")
}

// TestRPCNotification verifies that an RPCMessage with ID=0 (notification)
// invokes the handler but does not produce a response.
func TestRPCNotification(t *testing.T) {
	client := newStubClient()
	mw := NewACPMiddleware("acp-bridge", client)

	d := NewRPCDispatcher()
	var called int32
	d.Register("notify", func(_ context.Context, _ json.RawMessage) (any, error) {
		atomic.AddInt32(&called, 1)
		return nil, nil
	})

	adapter := NewACPMiddlewareAdapter(mw, nil, client).WithRPCDispatcher(d)
	t.Cleanup(adapter.Close)

	inner := &echoLoop{}
	wrapped := adapter.Wrap(inner)
	_, _ = wrapped.Run(context.Background(), core.Submission{Content: "start"}) //nolint:errcheck

	rpcMsg := RPCMessage{
		JSONRPC: "2.0",
		Method:  "notify",
		ID:      0, // notification - no response expected
	}
	rpcData, err := json.Marshal(rpcMsg)
	require.NoError(t, err)

	client.recv <- ACPMessage{
		Type:       TypeRPC,
		SenderID:   "peer",
		ReceiverID: "me",
		Content:    string(rpcData),
	}

	// The handler should still be invoked.
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&called) == 1
	}, 2*time.Second, 10*time.Millisecond, "notification handler should be invoked")

	// No response should be sent for a notification.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, 0, client.sentCount(), "no response should be sent for a notification")
}
