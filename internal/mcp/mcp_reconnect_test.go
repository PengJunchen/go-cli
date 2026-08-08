package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMCPNoReconnectWhenConnected verifies that a successful CallTool does not
// trigger any reconnection attempts.
func TestMCPNoReconnectWhenConnected(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer closeConn(serverConn)

	// Simple echo server.
	go func() {
		sc := bufio.NewScanner(serverConn)
		for sc.Scan() {
			var req struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			frame := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"content": "ok"},
			}
			b, _ := json.Marshal(frame)
			fmt.Fprintf(serverConn, "%s\n", b)
		}
		closeConn(serverConn)
	}()

	conn := NewJSONRPCLineTransport(clientConn, clientConn, clientConn.Close)
	adapter := NewOfficialSDKAdapter(MCPServerConfig{Name: "srv", Transport: MCPTransportStdio}, WithConnection(conn))
	require.NoError(t, adapter.Connect(context.Background()))

	result, err := adapter.CallTool(context.Background(), "echo", nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Content)

	// No reconnection should have been attempted.
	assert.Equal(t, 0, adapter.adapterCore.reconnectAttempts, "no reconnect attempts expected on success")
}

// TestMCPReconnectMaxAttemptsExhausted verifies that when the connection is
// lost, the adapter attempts to reconnect up to maxReconnect times and then
// returns the error.
func TestMCPReconnectMaxAttemptsExhausted(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()

	// Close the server side immediately so reads/writes on the client side
	// fail with a connection error.
	closeConn(serverConn)

	conn := NewJSONRPCLineTransport(clientConn, clientConn, clientConn.Close)
	adapter := NewOfficialSDKAdapter(MCPServerConfig{Name: "srv", Transport: MCPTransportStdio}, WithConnection(conn))
	require.NoError(t, adapter.Connect(context.Background()))

	// Limit reconnect attempts to 1 to keep the test fast (1s backoff).
	adapter.adapterCore.maxReconnect = 1

	_, err := adapter.CallTool(context.Background(), "echo", nil)
	require.Error(t, err, "CallTool must fail when the connection is lost")

	// Exactly 1 reconnection attempt should have been made.
	assert.Equal(t, 1, adapter.adapterCore.reconnectAttempts,
		"exactly maxReconnect reconnection attempts should have been made")
}

// TestMCPReconnectRPCErrorNoReconnect verifies that server-side RPC errors do
// not trigger reconnection (they are not transport-level failures).
func TestMCPReconnectRPCErrorNoReconnect(t *testing.T) {
	t.Parallel()

	serverConn, clientConn := net.Pipe()
	defer closeConn(serverConn)

	// Server that responds with an RPC error for every request.
	go func() {
		sc := bufio.NewScanner(serverConn)
		for sc.Scan() {
			var req struct {
				ID int64 `json:"id"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			frame := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -1, "message": "tool not found"},
			}
			b, _ := json.Marshal(frame)
			fmt.Fprintf(serverConn, "%s\n", b)
		}
		closeConn(serverConn)
	}()

	conn := NewJSONRPCLineTransport(clientConn, clientConn, clientConn.Close)
	adapter := NewOfficialSDKAdapter(MCPServerConfig{Name: "srv", Transport: MCPTransportStdio}, WithConnection(conn))
	require.NoError(t, adapter.Connect(context.Background()))

	_, err := adapter.CallTool(context.Background(), "echo", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rpc error")

	// RPC errors must not trigger reconnection.
	assert.Equal(t, 0, adapter.adapterCore.reconnectAttempts,
		"RPC errors must not trigger reconnection")
}

// TestMCPReconnectSucceeds verifies that when the connection drops and a
// reconnect re-establishes it, the retried call succeeds.
func TestMCPReconnectSucceeds(t *testing.T) {
	t.Parallel()

	// We use a flaky connection that fails the first request but succeeds
	// after reconnection. We simulate this with a pipe whose server side
	// closes after the first request, then we provide a fresh pipe via a
	// custom transport swap.
	serverConn1, clientConn1 := net.Pipe()

	var firstCall atomic.Bool
	firstCall.Store(true)

	// First server: responds once, then closes.
	go func() {
		sc := bufio.NewScanner(serverConn1)
		for sc.Scan() {
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			if firstCall.Load() && req.Method == "tools/call" {
				firstCall.Store(false)
				// Close the connection to simulate a drop.
				closeConn(serverConn1)
				return
			}
			frame := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"content": "ok"},
			}
			b, _ := json.Marshal(frame)
			fmt.Fprintf(serverConn1, "%s\n", b)
		}
	}()

	conn := NewJSONRPCLineTransport(clientConn1, clientConn1, clientConn1.Close)
	adapter := NewOfficialSDKAdapter(MCPServerConfig{Name: "srv", Transport: MCPTransportStdio}, WithConnection(conn))
	require.NoError(t, adapter.Connect(context.Background()))

	// Prepare a second connection that will be used after reconnection.
	serverConn2, clientConn2 := net.Pipe()
	defer closeConn(serverConn2)
	go func() {
		sc := bufio.NewScanner(serverConn2)
		for sc.Scan() {
			var req struct {
				ID     int64  `json:"id"`
				Method string `json:"method"`
			}
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				continue
			}
			frame := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]any{"content": "recovered"},
			}
			b, _ := json.Marshal(frame)
			fmt.Fprintf(serverConn2, "%s\n", b)
		}
		closeConn(serverConn2)
	}()

	// Swap the transport after the first failure so reconnection picks up the
	// new pipe. We use a goroutine to detect the first connection closing and
	// then swap.
	go func() {
		// Give the first call time to fail.
		time.Sleep(200 * time.Millisecond)
		adapter.adapterCore.mu.Lock()
		adapter.adapterCore.conn = NewJSONRPCLineTransport(clientConn2, clientConn2, clientConn2.Close)
		adapter.adapterCore.connected = false
		adapter.adapterCore.mu.Unlock()
	}()

	// Use a generous context timeout to allow the backoff + reconnect.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := adapter.CallTool(ctx, "echo", nil)
	require.NoError(t, err, "CallTool should succeed after reconnection")
	assert.Equal(t, "recovered", result.Content)
	assert.GreaterOrEqual(t, adapter.adapterCore.reconnectAttempts, 1)
}

// TestReconnectBackoff verifies the exponential backoff schedule.
func TestReconnectBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 8 * time.Second}, // capped
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, reconnectBackoff(tt.attempt))
	}
}

// TestIsConnectionError verifies the error classification.
func TestIsConnectionError(t *testing.T) {
	t.Parallel()

	assert.True(t, isConnectionError(fmt.Errorf("mcp: write request: broken pipe")))
	assert.True(t, isConnectionError(fmt.Errorf("mcp: read response: EOF")))
	assert.False(t, isConnectionError(fmt.Errorf("mcp: rpc error: tool not found")))
	assert.False(t, isConnectionError(nil))
}
