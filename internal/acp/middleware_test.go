package acp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

func TestACPMiddlewareName(t *testing.T) {
	m := NewACPMiddleware("acp-bridge", nil)
	assert.Equal(t, "acp-bridge", m.Name())
}

// TestACPMiddlewareConvertsMessage verifies that an ACP message carried in
// AgentInput.Data is converted into an agent event: the content drives the
// AgentFunc and the produced response is relayed back to the peer.
func TestACPMiddlewareConvertsMessage(t *testing.T) {
	client, _ := newStdioPeer(t)
	require.NoError(t, client.Connect(context.Background()))

	m := NewACPMiddleware("acp-bridge", client)
	next := func(_ context.Context, in extension.AgentInput) (extension.AgentOutput, error) {
		assert.Equal(t, "incoming content", in.Message)
		assert.Equal(t, map[string]string{"topic": "demo"}, in.Data)
		return extension.AgentOutput{Text: "handled: " + in.Message}, nil
	}

	out, err := m.WrapAgent(next)(context.Background(), extension.AgentInput{
		Message: "ignored-surface",
		Data: ACPMessage{
			Type:       TypeMessage,
			SenderID:   "peer",
			ReceiverID: "client",
			Content:    "incoming content",
			Metadata:   map[string]string{"topic": "demo"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "handled: incoming content", out.Text)
}

// TestACPMiddlewarePassesThrough verifies that inputs without an ACP message
// are passed through the AgentFunc unchanged.
func TestACPMiddlewarePassesThrough(t *testing.T) {
	m := NewACPMiddleware("acp-bridge", nil)
	called := false
	next := func(_ context.Context, in extension.AgentInput) (extension.AgentOutput, error) {
		called = true
		return extension.AgentOutput{Text: "ok:" + in.Message}, nil
	}

	out, err := m.WrapAgent(next)(context.Background(), extension.AgentInput{Message: "plain"})
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, "ok:plain", out.Text)
}

// TestACPMiddlewareConvertsWithoutClient verifies the conversion path works
// even when no client is configured (no reply is relayed).
func TestACPMiddlewareConvertsWithoutClient(t *testing.T) {
	m := NewACPMiddleware("acp-bridge", nil)
	next := func(_ context.Context, in extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{Text: "handled:" + in.Message}, nil
	}

	out, err := m.WrapAgent(next)(context.Background(), extension.AgentInput{
		Data: ACPMessage{Type: TypeMessage, SenderID: "peer", Content: "loose"},
	})
	require.NoError(t, err)
	assert.Equal(t, "handled:loose", out.Text)
}
