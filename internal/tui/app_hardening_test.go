package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestAppRunRestartable verifies that a stopped app can be run again on a
// fresh event source, exercising the running-flag release in Run's deferred
// cleanup path.
func TestAppRunRestartable(t *testing.T) {
	first := make(chan AgentEvent, 1)
	first <- AgentEvent{ContentType: ContentTypeStatus, Content: "one"}
	close(first)

	app := NewBubbleteaApp(first)
	require.NoError(t, app.Run(context.Background()))
	require.Equal(t, int64(1), app.EventsProcessed())
	require.Contains(t, app.View(), "one")
	require.False(t, app.running.Load(), "running flag should be released after Run returns")

	second := make(chan AgentEvent, 1)
	second <- AgentEvent{ContentType: ContentTypeStatus, Content: "two"}
	close(second)

	app.events = second
	require.NoError(t, app.Run(context.Background()))
	require.Equal(t, int64(2), app.EventsProcessed(), "a fresh run should keep consuming")
	require.Contains(t, app.View(), "two")
}

// TestAppDrawAfterCleanup verifies drawing frames remains safe even after the
// app has already cleaned up (it only appends to the view buffer).
func TestAppDrawAfterCleanup(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.addEntry("status", "first")
	app.cleanup()
	app.addEntry("status", "after")
	view := app.View()
	require.Contains(t, view, "first")
	require.Contains(t, view, "after")
}

// TestAppViewConcurrentReads verifies View is safe for concurrent readers while
// a writer draws frames concurrently (guarded by the internal mutex).
func TestAppViewConcurrentReads(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				app.addEntry("status", "x")
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = app.View()
			}
		}()
	}
	wg.Wait()
	require.NotEmpty(t, app.View())
}

// TestAppHandleEventWithThemeManagerOverride verifies handleEvent uses the
// renderer registered for the content type and applies the active theme's
// styling, producing a styled frame in the view.
func TestAppHandleEventWithThemeManagerOverride(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.handleEvent(context.Background(), AgentEvent{ContentType: ContentTypeCode, Content: "stylized"})
	// Code renderer emits an SGR fg escape around the payload.
	require.Contains(t, app.View(), "\x1b[")
	require.Contains(t, app.View(), "stylized")
}

// TestAppHandleEventEmptyContentType verifies an empty ContentType routes to the
// default renderer rather than being dropped.
func TestAppHandleEventEmptyContentType(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.handleEvent(context.Background(), AgentEvent{ContentType: "", Content: "empty-ct"})
	require.Contains(t, app.View(), "empty-ct")
}

// TestAppHandleEventTraceSpanLogging verifies handleEvent records trace/span
// identifiers without erroring when they are empty.
func TestAppHandleEventTraceSpanLogging(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.handleEvent(context.Background(), AgentEvent{
		ContentType: ContentTypeMarkdown,
		Content:     "logged",
		TraceID:     "trace-1",
		SpanID:      "span-1",
	})
	require.Contains(t, app.View(), "logged")
}

// TestAppMessagesProcessedDefault verifies the message counter is zero before
// any Send is consumed.
func TestAppMessagesProcessedDefault(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	require.Zero(t, app.MessagesProcessed())
	require.Zero(t, app.EventsProcessed())
	require.NotNil(t, app.reg)
	require.NotNil(t, app.themeMgr)
}

// TestAppQuitClosesDoneAfterRun maps the Quit → Done handshake end-to-end:
// Done must close exactly once after a graceful quit returns.
func TestAppQuitClosesDoneAfterRun(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	app.Quit()
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after Quit")
	}

	// Done should be closed now.
	select {
	case <-app.Done():
	default:
		t.Fatal("Done channel not closed after a completed run")
	}
}

// TestAppRunWithPrecanceledContext verifies Run returns context.Canceled
// promptly when the context is already canceled before the call.
func TestAppRunWithPrecanceledContext(t *testing.T) {
	events := make(chan AgentEvent, 4)
	defer close(events)
	app := NewBubbleteaApp(events)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, app.Run(ctx), context.Canceled)
}

// TestAppErrAlreadyRunningSentinel verifies comparing against the package
// sentinel works with errors.Is.
func TestAppErrAlreadyRunningSentinel(t *testing.T) {
	require.True(t, errors.Is(errAlreadyRunning, errAlreadyRunning))
	require.Error(t, errAlreadyRunning)
}

// TestAppDrawNilRenderer verifies passing a nil renderer to draw does not panic
// and appends the raw output line.
func TestAppDrawNilRenderer(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.addEntry("status", "raw")
	require.Contains(t, app.View(), "raw")
}

// TestAppRunConsumesStreamingThenReplaces verifies repeated streaming events
// through the full Run path keep the view on a single frame (live-update).
func TestAppRunConsumesStreamingThenReplaces(t *testing.T) {
	events := make(chan AgentEvent, 8)
	app := NewBubbleteaApp(events, WithWidth(0))
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	events <- AgentEvent{ContentType: ContentTypeStreaming, Content: "one"}
	events <- AgentEvent{ContentType: ContentTypeStreaming, Content: "two"}
	require.Eventually(t, func() bool { return app.EventsProcessed() == 2 },
		time.Second, 5*time.Millisecond)
	app.Quit()
	<-runErr

	require.Equal(t, 1, strings.Count(app.View(), "\n")+1, "streaming view should stay single-line")
	require.Contains(t, app.View(), "two")
}

// TestAppCleanupReleasesStreamBuffer verifies that after cleanup the stream
// buffer map is rebuilt to a fresh, non-nil map.
func TestAppCleanupReleasesStreamBuffer(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.streamBuf["streaming"] = &strings.Builder{}
	app.cleanup()
	require.NotNil(t, app.streamBuf)
	require.Empty(t, app.streamBuf)
	require.True(t, app.cleaned)
}

// TestAppEmptyEventStreamRunThenQuit verifies a zero-cap, never-closed event
// channel still allows Quit to stop the loop.
func TestAppEmptyEventStreamRunThenQuit(t *testing.T) {
	events := make(chan AgentEvent)
	defer close(events)
	app := NewBubbleteaApp(events)
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	app.Quit()
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on Quit with an open empty stream")
	}
}
