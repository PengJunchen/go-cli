package extension

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBridgeEventsInOrder verifies that EventBridge forwards events in the same
// order they were produced by the source (FIFO). A single forwarding goroutine
// reads one event at a time and writes it to the destination before reading the
// next, so ordering is preserved.
func TestBridgeEventsInOrder(t *testing.T) {
	const n = 100
	src := make(chan BridgeEvent, n)
	for i := 0; i < n; i++ {
		src <- BridgeEvent{Index: i, Data: i}
	}
	close(src)

	bridge := NewEventBridge(src)
	out := bridge.Forward(context.Background())

	for i := 0; i < n; i++ {
		select {
		case ev := <-out:
			assert.Equal(t, i, ev.Index, "events must arrive in FIFO order")
		case <-time.After(time.Second):
			t.Fatalf("did not receive event %d", i)
		}
	}

	// After the source is drained and closed, the destination must close.
	select {
	case _, ok := <-out:
		require.False(t, ok, "out should be closed after source closes")
	case <-time.After(time.Second):
		t.Fatal("out did not close after source closed")
	}
	bridge.Wait()
}

// TestBridgeNoEventLoss verifies that no events are dropped between the source
// and the destination, even when the destination is consumed concurrently with
// production. Every event read from the source is written to the destination
// before the next is read, so loss is impossible under normal operation.
func TestBridgeNoEventLoss(t *testing.T) {
	const n = 500
	src := make(chan BridgeEvent, n)
	for i := 0; i < n; i++ {
		src <- BridgeEvent{Index: i, Data: fmt.Sprintf("ev-%d", i)}
	}
	close(src)

	bridge := NewEventBridge(src)
	out := bridge.Forward(context.Background())

	seen := make(map[int]bool, n)
	for i := 0; i < n; i++ {
		select {
		case ev := <-out:
			require.False(t, seen[ev.Index], "duplicate event %d", ev.Index)
			seen[ev.Index] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("did not receive event %d (received %d/%d)", i, len(seen), n)
		}
	}
	require.Len(t, seen, n, "all events must be delivered, none lost")

	select {
	case _, ok := <-out:
		require.False(t, ok, "out should close after source closes")
	case <-time.After(time.Second):
		t.Fatal("out did not close")
	}
	bridge.Wait()
}

// TestBridgeCloseOnSourceClose verifies that closing the source channel causes
// the bridge to close the destination channel, signaling end-of-stream to
// consumers. Wait returns once the forwarding goroutine has exited.
func TestBridgeCloseOnSourceClose(t *testing.T) {
	src := make(chan BridgeEvent)
	bridge := NewEventBridge(src)
	out := bridge.Forward(context.Background())

	go func() {
		src <- BridgeEvent{Index: 1, Data: "only"}
		close(src)
	}()

	select {
	case ev := <-out:
		assert.Equal(t, 1, ev.Index)
	case <-time.After(time.Second):
		t.Fatal("did not receive event")
	}

	select {
	case _, ok := <-out:
		require.False(t, ok, "out should close when source closes")
	case <-time.After(time.Second):
		t.Fatal("out did not close after source closed")
	}
	bridge.Wait()
}

// TestBridgeErrorPropagation verifies that a BridgeEvent carrying an error is
// forwarded to the destination unchanged. The bridge must not swallow, wrap, or
// drop source-side errors.
func TestBridgeErrorPropagation(t *testing.T) {
	src := make(chan BridgeEvent, 2)
	wantErr := errors.New("source boom")
	src <- BridgeEvent{Index: 0, Data: "ok"}
	src <- BridgeEvent{Index: 1, Err: wantErr}
	close(src)

	bridge := NewEventBridge(src)
	out := bridge.Forward(context.Background())

	select {
	case ev := <-out:
		require.NoError(t, ev.Err)
		assert.Equal(t, 0, ev.Index)
	case <-time.After(time.Second):
		t.Fatal("did not receive first event")
	}

	select {
	case ev := <-out:
		require.Error(t, ev.Err, "error event must be propagated")
		assert.True(t, errors.Is(ev.Err, wantErr), "error must be propagated unchanged, got %v", ev.Err)
		assert.Equal(t, 1, ev.Index)
	case <-time.After(time.Second):
		t.Fatal("did not receive error event")
	}

	bridge.Wait()
}

// TestEventBridgeCancelAfterReadReportsDropped verifies that cancellation is
// accounted for when the bridge has already read an event but its full output
// buffer prevents forwarding it.
func TestEventBridgeCancelAfterReadReportsDropped(t *testing.T) {
	const outputCapacity = 64
	src := make(chan BridgeEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bridge := NewEventBridge(src)
	out := bridge.Forward(ctx)

	sendDeadline := time.NewTimer(2 * time.Second)
	defer sendDeadline.Stop()
	for i := 0; i < outputCapacity+1; i++ {
		select {
		case src <- BridgeEvent{Index: i, Data: fmt.Sprintf("ev-%d", i)}:
		case <-sendDeadline.C:
			t.Fatalf("timed out sending event %d", i)
		}
	}

	// The unbuffered source send above handshakes with the bridge's receive of
	// event 65. Since the bridge forwards serially, the preceding 64 events
	// already fill its output buffer when this cancellation is signaled.
	cancel()

	waited := make(chan struct{})
	go func() {
		bridge.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("bridge did not stop after cancellation")
	}

	forwarded := 0
	for ev := range out {
		assert.Equal(t, forwarded, ev.Index, "forwarded events must remain FIFO")
		forwarded++
	}
	require.Equal(t, outputCapacity, forwarded)

	stats := bridge.Stats()
	assert.Equal(t, uint64(outputCapacity), stats.Forwarded)
	assert.Equal(t, uint64(1), stats.DroppedAfterRead)
}
