package acp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestACPCClientServerContracts verifies that the concrete adapters and the
// default server satisfy their interfaces at compile time.
func TestACPCClientServerContracts(t *testing.T) {
	var _ ACPClient = (*gRPCAdapter)(nil)
	var _ ACPClient = (*StdioAdapter)(nil)
	var _ ACPServer = (*DefaultACPServer)(nil)
	var _ ACPServer = (*MockACPServer)(nil)

	assert.Equal(t, "gRPC", ACPTransportGRPC.String())
	assert.Equal(t, "Stdio", ACPTransportStdio.String())
}

// TestACPMessageJSON verifies the ACPMessage wire format round-trips through
// JSON with the expected field names.
func TestACPMessageJSON(t *testing.T) {
	ts := time.Now().Truncate(time.Second).UTC()
	orig := ACPMessage{
		Type:       TypeMessage,
		SenderID:   "alice",
		ReceiverID: "bob",
		Content:    "hello",
		Metadata:   map[string]string{"k": "v"},
		Timestamp:  ts,
	}

	data, err := json.Marshal(orig)
	require.NoError(t, err)

	// The wire field names must match the ACP convention.
	for _, key := range []string{"type", "sender_id", "receiver_id", "content", "metadata", "timestamp"} {
		assert.Contains(t, string(data), "\""+key+"\"", "missing JSON key %q", key)
	}

	var got ACPMessage
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, TypeMessage, got.Type)
	assert.Equal(t, "alice", got.SenderID)
	assert.Equal(t, "bob", got.ReceiverID)
	assert.Equal(t, "hello", got.Content)
	assert.Equal(t, "v", got.Metadata["k"])
	assert.True(t, got.Timestamp.Equal(ts), "timestamp mismatch: %v != %v", got.Timestamp, ts)
}

// TestDefaultACPServerLifecycle verifies Start/Stop/Name/Running state.
func TestDefaultACPServerLifecycle(t *testing.T) {
	srv, ok := NewDefaultACPServer("mysrv").(*DefaultACPServer)
	require.True(t, ok, "expected a *DefaultACPServer")
	assert.Equal(t, "mysrv", srv.Name())
	assert.False(t, srv.Running())

	require.NoError(t, srv.Start(context.Background()))
	assert.True(t, srv.Running())

	require.NoError(t, srv.Stop(context.Background()))
	assert.False(t, srv.Running())
}
