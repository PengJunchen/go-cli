//go:build headless

package tests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tui"
)

// p4TUIRender verifies the retained event transport in a headless build.
// Rich rendering assertions remain unchanged in phase4_tui_full_test.go.
func p4TUIRender(ctx context.Context, t *testing.T) {
	t.Helper()
	stream := core.NewEventStream(4)
	source := tui.BridgeEventsOwned(ctx, stream)
	require.NoError(t, stream.Send(core.AgentEvent{Kind: "message", Content: "hello assistant"}))
	require.NoError(t, stream.Send(core.AgentEvent{Kind: "done", Content: "status-line-text"}))
	stream.Close()

	var received []tui.AgentEvent
	for event := range source.Events() {
		received = append(received, event)
	}
	require.Len(t, received, 2)
	assert.Equal(t, tui.ContentTypeAssistant, received[0].ContentType)
	assert.Equal(t, "hello assistant", received[0].Content)
	assert.Equal(t, tui.ContentTypeStatus, received[1].ContentType)
	assert.Equal(t, "status-line-text", received[1].Content)
	select {
	case <-source.Done():
	case <-ctx.Done():
		t.Fatal("headless event owner failed to join")
	}
}
