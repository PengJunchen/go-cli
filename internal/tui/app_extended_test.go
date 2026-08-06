package tui

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAppDrawAppendNonStreaming verifies non-streaming entries append to the
// accordion model in order.
func TestAppDrawAppendNonStreaming(t *testing.T) {
	app := NewBubbleteaApp(nil)
	app.addEntry("status", "first")
	app.addEntry("status", "second")
	view := app.View()
	assert.Contains(t, view, "first")
	assert.Contains(t, view, "second")
}

// TestAppDrawReplaceStreaming verifies streaming content types replace the
// previous frame instead of appending.
func TestAppDrawReplaceStreaming(t *testing.T) {
	app := NewBubbleteaApp(nil)
	app.addEntry("streaming", "a")
	app.addEntry("streaming", "b")
	view := app.View()
	assert.Contains(t, view, "b")
	assert.NotContains(t, view, "a")
}

// TestAppDrawStreamingFirstAppends verifies the first streaming frame is appended
// when the buffer is empty (no last frame to replace yet).
func TestAppDrawStreamingFirstAppends(t *testing.T) {
	app := NewBubbleteaApp(nil)
	app.addEntry("streaming", "only")
	assert.Contains(t, app.View(), "only")
}

// TestIsStreamingRenderContentType verifies the streaming content type check.
func TestIsStreamingRenderContentType(t *testing.T) {
	assert.True(t, isStreamingRenderContentType(ContentTypeStreaming))
	assert.True(t, isStreamingRenderContentType(ContentTypeStreamingCode))
	assert.True(t, isStreamingRenderContentType(ContentTypeStreamingThink))
	assert.False(t, isStreamingRenderContentType(ContentTypeStatus))
	assert.False(t, isStreamingRenderContentType(ContentTypeToolCall))
}

// TestAppSendDropsWhenFull verifies Send drops a message rather than blocking
// when the internal queue is saturated. It must never block the caller.
func TestAppSendDropsWhenFull(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	require.Len(t, app.msgCh, 0)

	// Fill the buffered (capacity 16) queue completely.
	for i := 0; i < cap(app.msgCh); i++ {
		app.Send(i)
	}
	require.Len(t, app.msgCh, cap(app.msgCh))

	// The next Send must drop (default branch) without blocking.
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.Send("overflow")
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Send blocked on a full queue; expected non-blocking drop")
	}
	assert.Len(t, app.msgCh, cap(app.msgCh), "overflow message should have been dropped")
}

// TestAppHandleEventUnknownTypeFallback verifies handleEvent routes an unknown
// content type through the default renderer and still draws to the view.
func TestAppHandleEventUnknownTypeFallback(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.handleEvent(context.Background(), AgentEvent{ContentType: "unknown-ct", Content: "payload"})
	assert.Contains(t, app.View(), "payload")
	assert.Equal(t, int64(0), app.EventsProcessed(), "handleEvent should not bump the event counter")
}

// TestAppCleanupSingleRelease verifies cleanup is idempotent and closes Done only
// once and resets the stream buffer.
func TestAppCleanupSingleRelease(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	app.cleanup()
	recvClosed := false
	select {
	case <-app.Done():
		recvClosed = true
	default:
	}
	require.True(t, recvClosed, "Done should be closed after cleanup")

	// A second cleanup must not panic and must not reset buffer to a new map the
	// second time (idempotent).
	app.cleanup()
	require.True(t, app.cleaned)
}

// TestAppRunWithNilEventsChannel verifies Run returns promptly when the event
// channel is (nil) closed, exercising the events-closed exit path.
func TestAppRunWithNilEventsChannel(t *testing.T) {
	closed := make(chan AgentEvent)
	close(closed)
	app := NewBubbleteaApp(closed)
	require.NoError(t, app.Run(context.Background()))
	assert.Equal(t, int64(0), app.EventsProcessed())
	select {
	case <-app.Done():
	default:
		t.Fatal("Done not closed after events channel closed")
	}
}

// TestAppQuitIsIdempotent verifies calling Quit multiple times across
// goroutines does not panic and stops the loop exactly once.
func TestAppQuitIsIdempotent(t *testing.T) {
	events := make(chan AgentEvent, 1)
	defer close(events)
	app := NewBubbleteaApp(events)
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	for i := 0; i < 10; i++ {
		app.Quit()
	}
	select {
	case err := <-runErr:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Run did not return after repeated Quit")
	}
}

// TestAppViewEmptyBeforeRun verifies View returns an empty string before any
// events are processed.
func TestAppViewEmptyBeforeRun(t *testing.T) {
	app := NewBubbleteaApp(make(chan AgentEvent, 1))
	assert.Equal(t, "", app.View())
}

// TestAppOptionsOverride verifies WithRegistry and WithWidth options are applied
// at construction.
func TestAppOptionsOverride(t *testing.T) {
	reg := NewRendererRegistry()
	reg.Register(NewMockRenderer("custom", ContentTypeStatus, "custom-out"))
	app := NewBubbleteaApp(make(chan AgentEvent, 1), WithRegistry(reg), WithWidth(42))
	require.Equal(t, reg, app.reg)
	require.Equal(t, 42, app.width)
}

// TestAppSendAndEventsBothConsumed verifies the loop interleaves Send messages
// with the event stream without losing either.
func TestAppSendAndEventsBothConsumed(t *testing.T) {
	events := make(chan AgentEvent, 16)
	app := NewBubbleteaApp(events, WithWidth(10))
	runErr := make(chan error, 1)
	go func() { runErr <- app.Run(context.Background()) }() //nolint:errcheck

	events <- AgentEvent{ContentType: ContentTypeCode, Content: "x"}
	app.Send("msg1")
	app.Send("msg2")
	events <- AgentEvent{ContentType: ContentTypeCode, Content: "y"}

	require.Eventually(t, func() bool {
		return app.EventsProcessed() == 2 && app.MessagesProcessed() == 2
	}, time.Second, 5*time.Millisecond, "events/messages not fully consumed")

	app.Quit()
	<-runErr
}

// TestAppToolOutputAppendsToToolCall verifies that tool_output events append
// to the last tool_call entry instead of creating a new top-level entry.
func TestAppToolOutputAppendsToToolCall(t *testing.T) {
	app := NewBubbleteaApp(nil)

	app.handleEvent(context.Background(), AgentEvent{
		ContentType: ContentTypeToolCall,
		Content:     "bash echo hi",
	})
	app.handleEvent(context.Background(), AgentEvent{
		ContentType: ContentTypeToolOutput,
		Content:     "line one",
		Stream:      "stdout",
	})
	app.handleEvent(context.Background(), AgentEvent{
		ContentType: ContentTypeToolOutput,
		Content:     "line two",
		Stream:      "stderr",
	})

	// The accordion should have exactly one top-level entry (the tool_call).
	require.Equal(t, 1, app.accordion.Len())
	entries := app.accordion.Entries()
	require.Len(t, entries, 1)
	full := entries[0].Full
	assert.Contains(t, full, "bash echo hi")
	assert.Contains(t, full, "line one")
	assert.Contains(t, full, "[output]")
	assert.Contains(t, full, "line two")
	assert.Contains(t, full, "[err]")
}

// TestAppToolOutputNoToolCallFallsBack verifies that a tool_output event with
// no preceding tool_call entry falls through to creating a new entry.
func TestAppToolOutputNoToolCallFallsBack(t *testing.T) {
	app := NewBubbleteaApp(nil)

	app.handleEvent(context.Background(), AgentEvent{
		ContentType: ContentTypeToolOutput,
		Content:     "orphan output",
		Stream:      "stdout",
	})

	require.Equal(t, 1, app.accordion.Len())
	entries := app.accordion.Entries()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Full, "orphan output")
}
