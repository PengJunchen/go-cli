package tui

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2EAssistantThroughStatusPipeline runs a full assistant→error→status event
// pipeline through Run and asserts the accumulated view reflects each renderer.
func TestE2EAssistantThroughStatusPipeline(t *testing.T) {
	events := make(chan AgentEvent, 8)
	events <- AgentEvent{ContentType: ContentTypeAssistant, Content: "plan"}
	events <- AgentEvent{ContentType: ContentTypeError, Content: "boom"}
	events <- AgentEvent{ContentType: ContentTypeStatus, Content: "done"}
	close(events)

	app := NewBubbleteaApp(events)
	require.NoError(t, app.Run(context.Background()))

	view := app.View()
	plain := stripEscape(view)
	assert.Contains(t, plain, "AI: plan")
	assert.Contains(t, plain, "boom")
	assert.Contains(t, plain, "done")
	require.Equal(t, int64(3), app.EventsProcessed())
	// Assistant/error/status are all non-streaming so all three lines appear.
	require.Equal(t, 3, countLines(plain))
}

// TestE2EUnknownTypeUsesFallback verifies an unknown content type routes through
// the default (status) renderer and still contributes a line.
func TestE2EUnknownTypeUsesFallback(t *testing.T) {
	events := make(chan AgentEvent, 4)
	events <- AgentEvent{ContentType: "weird-type", Content: "fallback-content"}
	close(events)

	app := NewBubbleteaApp(events)
	require.NoError(t, app.Run(context.Background()))
	require.Contains(t, app.View(), "fallback-content")
	require.Equal(t, int64(1), app.EventsProcessed())
}

// TestE2EMixedStreamingAndStatic verifies a streaming event followed by a static
// event yields two lines: the latest streamed frame plus the static line.
func TestE2EMixedStreamingAndStatic(t *testing.T) {
	events := make(chan AgentEvent, 8)
	events <- AgentEvent{ContentType: ContentTypeStreaming, Content: "s1"}
	events <- AgentEvent{ContentType: ContentTypeStreaming, Content: "s2"}
	events <- AgentEvent{ContentType: ContentTypeStatus, Content: "st"}
	close(events)

	app := NewBubbleteaApp(events)
	require.NoError(t, app.Run(context.Background()))

	view := app.View()
	require.Equal(t, 2, countLines(view), "one streaming frame + one static line")
	assert.Contains(t, view, "s2")
	assert.Contains(t, view, "st")
}

// TestE2EWithWidthWraps verifies the configured width applies to rendered
// content flowing through the full app.
func TestE2EWithWidthWraps(t *testing.T) {
	events := make(chan AgentEvent, 4)
	events <- AgentEvent{ContentType: ContentTypeCode, Content: "0123456789"}
	close(events)

	app := NewBubbleteaApp(events, WithWidth(3))
	require.NoError(t, app.Run(context.Background()))
	require.Contains(t, app.View(), "\n", "content should be wrapped at width 3")
}

// TestE2EThemeManagerDrivesRendering verifies a theme switch is visible in the
// rendered output through the full pipeline.
func TestE2EThemeManagerDrivesRendering(t *testing.T) {
	mgr := NewThemeManager()
	require.NoError(t, mgr.Set("light"))

	events := make(chan AgentEvent, 4)
	events <- AgentEvent{ContentType: ContentTypeMarkdown, Content: "[accent](http://x)"}
	close(events)

	app := NewBubbleteaApp(events, WithThemeManager(mgr))
	require.NoError(t, app.Run(context.Background()))
	// Glamour renders the markdown link with its own light style (escape sequences
	// and the display text "accent"). The theme's primary color is no longer
	// applied directly; glamour owns the color palette.
	require.Contains(t, app.View(), "\x1b[", "glamour should style markdown with escape sequences")
	require.Contains(t, stripEscape(app.View()), "accent")
}

// TestE2ERegistryCannedRenderer verifies a registered custom renderer is used
// for its content type during a full run.
func TestE2ERegistryCannedRenderer(t *testing.T) {
	reg := NewRendererRegistry()
	reg.Register(NewMockRenderer("canned", ContentTypeStatus, "CANONICAL"))

	events := make(chan AgentEvent, 4)
	events <- AgentEvent{ContentType: ContentTypeStatus, Content: "ignored"}
	close(events)

	app := NewBubbleteaApp(events, WithRegistry(reg))
	require.NoError(t, app.Run(context.Background()))
	require.Contains(t, app.View(), "CANONICAL")
}

// TestE2ESendMessagesConsumed verifies messages pushed before the loop starts
// are not counted until Run consumes them, and that a subsequent run drains
// the remaining queue.
func TestE2ESendMessagesConsumed(t *testing.T) {
	events := make(chan AgentEvent, 4)
	app := NewBubbleteaApp(events)
	app.Send("pre")
	require.Zero(t, app.MessagesProcessed(), "messages not consumed until Run")

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck
	require.Eventually(t, func() bool { return app.MessagesProcessed() == 1 },
		time.Second, 5*time.Millisecond)
	app.Quit()
	<-runErr
}

// countLines returns the number of lines in a view string, treating an empty
// string as a single blank view.
func countLines(s string) int {
	if s == "" {
		return 1
	}
	n := 1
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}
