//go:build e2e && !headless

package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pengjunchen/go-cli/internal/tui"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestET_Phase21_TUI_AC4_ToolCallCollapsedRendering verifies that
// RenderCollapsed produces a single-line summary with the tool name and args.
func TestET_Phase21_TUI_AC4_ToolCallCollapsedRendering(t *testing.T) {
	r := tui.NewToolCallRenderer()
	result := r.RenderCollapsed("read", map[string]any{"path": "/tmp/test.txt"})

	assert.False(t, strings.Contains(result, "\n"), "collapsed render should be single-line")
	assert.Contains(t, result, "read", "should contain tool name")
	assert.Contains(t, result, "path", "should contain arg key")
	assert.Contains(t, result, "/tmp/test.txt", "should contain arg value")
}

// TestET_Phase21_TUI_AC5_ToolCallExpandedRendering verifies that
// RenderExpanded produces a multi-line output with full args and result.
func TestET_Phase21_TUI_AC5_ToolCallExpandedRendering(t *testing.T) {
	r := tui.NewToolCallRenderer()
	result := r.RenderExpanded("read", map[string]any{"path": "/tmp/test.txt"}, "file content")

	assert.True(t, strings.Contains(result, "\n"), "expanded render should be multi-line")
	assert.Contains(t, result, "read", "should contain tool name")
	assert.Contains(t, result, "path", "should contain arg key")
	assert.Contains(t, result, "/tmp/test.txt", "should contain arg value")
	assert.Contains(t, result, "file content", "should contain result")
}

// TestET_Phase21_TUI_AC10_GoroutineLeakCheck verifies that no goroutine leaks
// occur after running TUI operations.
func TestET_Phase21_TUI_AC10_GoroutineLeakCheck(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	events := make(chan tui.AgentEvent, 5)
	app := tui.NewBubbleteaApp(events)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- app.Run(ctx)
	}()

	// Send a few events to exercise the TUI.
	events <- tui.AgentEvent{Type: "message", Content: "hello", ContentType: tui.ContentTypeAssistant}
	events <- tui.AgentEvent{Type: "tool", Content: "read", ContentType: tui.ContentTypeToolCall}

	// Close the event channel to signal the render loop to stop.
	close(events)

	select {
	case <-done:
		// App exited normally.
	case <-time.After(5 * time.Second):
		app.Quit()
		t.Fatal("app did not stop within 5 seconds")
	}

	// Ensure the app has fully cleaned up.
	select {
	case <-app.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("app did not signal Done() within 2 seconds")
	}
}
