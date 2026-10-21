//go:build !headless

package tests

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestE2E_Phase49_TUIHelpOverlay verifies that the TUI BubbleteaApp can be
// constructed and its exported methods work correctly (49-21).
func TestE2E_Phase49_TUIHelpOverlay(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Create an event channel and construct the app.
	events := make(chan tui.AgentEvent, 10)
	app := tui.NewBubbleteaApp(events)
	require.NotNil(t, app)

	// The app should be constructable without panicking.
	// We cannot call Run() in a test (requires a terminal), but we can
	// verify the exported methods are available and don't panic.

	// Send a test event to the channel (non-blocking since buffered).
	select {
	case events <- tui.AgentEvent{Type: "run", Content: "test"}:
	default:
	}

	// Quit should not panic.
	app.Quit()
}
