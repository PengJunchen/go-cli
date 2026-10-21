//go:build !headless

package tests

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tui"
)

// p4TUIRender feeds agent events into a BubbleteaApp backed by the default
// renderer registry and asserts the rendered view buffer contains the payload.
func p4TUIRender(ctx context.Context, t *testing.T) {
	t.Helper()

	events := make(chan tui.AgentEvent, 4)
	events <- tui.AgentEvent{Type: "status", Content: "status-line-text", ContentType: tui.ContentTypeStatus}
	events <- tui.AgentEvent{Type: "assistant", Content: "hello assistant", ContentType: tui.ContentTypeAssistant}
	close(events)

	app := tui.NewBubbleteaApp(events)
	require.NoError(t, app.Run(ctx))

	view := app.View()
	assert.Contains(t, view, "status-line-text")
	assert.Contains(t, view, "AI: ")
	assert.Contains(t, view, "hello assistant")
	require.GreaterOrEqual(t, app.EventsProcessed(), int64(2), "the app must drain both agent events")

	// A default registry must expose all 24 built-in renderers.
	registry := tui.NewDefaultRegistry()
	assert.GreaterOrEqual(t, len(registry.List()), 24, "default registry must register the 24 built-in renderers")
	_, ok := registry.Get(tui.ContentTypeMarkdown)
	require.True(t, ok, "markdown renderer must be registered")
}
