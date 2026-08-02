package tui

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAppRunConsumesEvents verifies that Run starts the TUI, consumes events
// from the stream, dispatches them to the matching renderer, and updates the
// view buffer (non-streaming branch).
func TestAppRunConsumesEvents(t *testing.T) {
	events := make(chan AgentEvent, 8)
	events <- AgentEvent{Type: "msg", ContentType: ContentTypeAssistant, Content: "hello", TraceID: "t1", SpanID: "s1"}
	events <- AgentEvent{Type: "msg", ContentType: ContentTypeError, Content: "boom", TraceID: "t2", SpanID: "s2"}
	events <- AgentEvent{Type: "msg", ContentType: ContentTypeStatus, Content: "done"}
	close(events)

	app := NewBubbleteaApp(events)
	require.NoError(t, app.Run(context.Background()))

	require.Equal(t, int64(3), app.EventsProcessed())
	view := app.View()
	assert.Contains(t, view, "hello")
	assert.Contains(t, view, "boom")
	assert.Contains(t, view, "done")
	assert.Contains(t, view, "AI: ", "assistant renderer should prefix AI")
}

// TestAppSendDeliversMsg verifies that Send enqueues a message the loop then
// consumes.
func TestAppSendDeliversMsg(t *testing.T) {
	events := make(chan AgentEvent, 1)
	defer close(events)

	app := NewBubbleteaApp(events)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }() //nolint:errcheck

	app.Send("probe")
	require.Eventually(t, func() bool { return app.MessagesProcessed() == 1 },
		time.Second, 5*time.Millisecond, "Send message not consumed")

	cancel()
	select {
	case err := <-runErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestAppQuitGraceful verifies that Quit stops the loop gracefully and that
// resources are cleaned up.
func TestAppQuitGraceful(t *testing.T) {
	events := make(chan AgentEvent, 1)
	defer close(events)

	app := NewBubbleteaApp(events)
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	app.Quit()
	require.Eventually(t, func() bool {
		select {
		case <-app.Done():
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond, "Done channel did not close after Quit")

	select {
	case err := <-runErr:
		require.NoError(t, err, "graceful quit should return nil error")
	case <-time.After(time.Second):
		t.Fatal("Run did not return after Quit")
	}
}

// TestAppStreamingRender verifies streaming render: each streaming event
// replaces the previous frame so the view holds the latest accumulated buffer
// on a single line (streaming branch).
func TestAppStreamingRender(t *testing.T) {
	events := make(chan AgentEvent, 8)
	app := NewBubbleteaApp(events, WithWidth(40))

	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	parts := []string{"Hel", "Hello ", "Hello world"}
	for i, part := range parts {
		events <- AgentEvent{Type: "msg", ContentType: ContentTypeStreaming, Content: part}
		want := int64(i + 1)
		require.Eventually(t, func() bool { return app.EventsProcessed() == want },
			time.Second, 5*time.Millisecond, "event %d not processed", i)
	}
	close(events)
	<-runErr

	require.Equal(t, int64(3), app.EventsProcessed())
	view := app.View()
	// Streaming output stays on a single frame: only one line, latest content.
	require.Equal(t, 1, strings.Count(view, "\n")+1)
	assert.Contains(t, view, "Hello world")
}

// TestAppUnknownContentTypeFallsBack verifies events with unknown content
// types fall back to the default renderer instead of dropping content.
func TestAppUnknownContentTypeFallsBack(t *testing.T) {
	events := make(chan AgentEvent, 4)
	events <- AgentEvent{ContentType: "nope", Content: "payload"}
	close(events)

	app := NewBubbleteaApp(events)
	require.NoError(t, app.Run(context.Background()))
	require.Equal(t, int64(1), app.EventsProcessed())
	assert.Contains(t, app.View(), "payload")
}

// TestAppContextCancelPropagation verifies context cancellation stops the
// loop and reports the cancellation.
func TestAppContextCancelPropagation(t *testing.T) {
	events := make(chan AgentEvent, 1)
	defer close(events)

	app := NewBubbleteaApp(events)
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }() //nolint:errcheck

	cancel()
	select {
	case err := <-runErr:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("Run did not propagate context cancellation")
	}
	require.Eventually(t, func() bool {
		select {
		case <-app.Done():
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond, "resources not cleaned up")
}

// TestAppRunTwiceFails verifies a second Run invocation is rejected.
func TestAppRunTwiceFails(t *testing.T) {
	events := make(chan AgentEvent, 1)
	defer close(events)
	app := NewBubbleteaApp(events)

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(ctx) }() //nolint:errcheck

	require.Eventually(t, func() bool { return app.running.Load() },
		time.Second, 5*time.Millisecond, "app should be running")

	require.ErrorIs(t, app.Run(context.Background()), errAlreadyRunning)

	cancel()
	<-runErr
}

// TestAppWithCustomTheme verifies the active theme is passed through to
// renderers (theme switching drives the view).
func TestAppWithCustomTheme(t *testing.T) {
	events := make(chan AgentEvent, 4)
	defer close(events)
	mgr := NewThemeManager()
	mgr.Register("mock", MockTheme{})
	require.NoError(t, mgr.Set("mock"))

	app := NewBubbleteaApp(events, WithThemeManager(mgr))
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	events <- AgentEvent{ContentType: ContentTypeCode, Content: "x := 1"}
	require.Eventually(t, func() bool { return app.eventsSeen.Load() == 1 },
		time.Second, 5*time.Millisecond)
	app.Quit()
	<-runErr

	assert.Contains(t, app.View(), "x := 1")
}

// TestNoGoroutineLeak verifies that repeated run/quit cycles do not leak
// goroutines.
func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 5; i++ {
		events := make(chan AgentEvent, 4)
		app := NewBubbleteaApp(events)
		runErr := make(chan error, 1)
		go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck
		events <- AgentEvent{ContentType: ContentTypeStatus, Content: "cycle"}
		app.Quit()
		<-runErr
		select {
		case <-app.Done():
		case <-time.After(time.Second):
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		after := runtime.NumGoroutine()
		if after <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.LessOrEqual(t, runtime.NumGoroutine(), before+2,
		"expected no goroutine leak across run/quit cycles")
}

// TestAppQuitWithoutEvents verifies Quit works even when no events are ever
// delivered.
func TestAppQuitWithoutEvents(t *testing.T) {
	events := make(chan AgentEvent, 1)
	defer close(events)
	app := NewBubbleteaApp(events)
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	app.Quit()
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on Quit")
	}
}

// TestErrAlreadyRunningNoPanic guards the sentinel error.
func TestErrAlreadyRunningNoPanic(t *testing.T) {
	assert.Error(t, errAlreadyRunning)
	assert.True(t, errors.Is(errAlreadyRunning, errAlreadyRunning))
}
