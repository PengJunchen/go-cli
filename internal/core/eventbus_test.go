package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestEventBus_PublishSubscribe(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewMemoryEventBus()
	defer bus.Close()

	ch := bus.Subscribe(ctx)

	bus.Publish(AgentEvent{Kind: "message", Content: "hello"})
	bus.Publish(AgentEvent{Kind: "done", Content: "bye"})

	got := receiveEvents(t, ch, 2, 2*time.Second)
	require.Len(t, got, 2)
	assert.Equal(t, "message", got[0].Kind)
	assert.Equal(t, "hello", got[0].Content)
	assert.Equal(t, "done", got[1].Kind)
}

func TestEventBus_MultipleSubscribers(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewMemoryEventBus()
	defer bus.Close()

	ch1 := bus.Subscribe(ctx)
	ch2 := bus.Subscribe(ctx)

	bus.Publish(AgentEvent{Kind: "message", Content: "broadcast"})

	got1 := receiveEvents(t, ch1, 1, 2*time.Second)
	got2 := receiveEvents(t, ch2, 1, 2*time.Second)

	require.Len(t, got1, 1)
	require.Len(t, got2, 1)
	assert.Equal(t, "broadcast", got1[0].Content)
	assert.Equal(t, "broadcast", got2[0].Content)
}

func TestEventBus_CloseClosesSubscribers(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewMemoryEventBus()
	ch := bus.Subscribe(ctx)

	bus.Close()

	// After Close, the subscriber channel should be closed.
	_, ok := <-ch
	assert.False(t, ok, "channel should be closed after bus Close")
}

func TestEventBus_ContextCancelUnsubscribes(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())

	bus := NewMemoryEventBus()
	defer bus.Close()

	ch := bus.Subscribe(ctx)

	cancel()

	// After ctx is canceled, the subscriber channel should be closed.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, ok := <-ch
		if !ok {
			return
		}
	}
	t.Fatal("subscriber channel was not closed after context cancellation")
}

func TestEventBus_PublishAfterClose(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	bus := NewMemoryEventBus()
	bus.Close()

	// Publishing after close should not panic.
	assert.NotPanics(t, func() {
		bus.Publish(AgentEvent{Kind: "message"})
	})
}

func TestEventBus_FullBufferDrops(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bus := NewMemoryEventBus()
	defer bus.Close()

	// Use a subscriber but never read from it so the 64-slot buffer fills.
	ch := bus.Subscribe(ctx)

	// Publish more than the buffer size (64). Publish must not block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 128; i++ {
			bus.Publish(AgentEvent{Kind: "message", Content: "fill"})
		}
	}()

	select {
	case <-done:
		// Success: Publish did not block.
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked when subscriber buffer was full")
	}

	// Drain the channel; we should get at most the buffer size.
	count := 0
drain:
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				break drain
			}
			count++
		default:
			break drain
		}
	}
	assert.LessOrEqual(t, count, 64, "should not receive more than buffer capacity")
}

// receiveEvents reads up to n events from ch within the timeout. It does not
// require the channel to close; it returns as soon as n events are collected.
func receiveEvents(t *testing.T, ch <-chan AgentEvent, n int, timeout time.Duration) []AgentEvent {
	t.Helper()
	var got []AgentEvent
	deadline := time.Now().Add(timeout)
	for len(got) < n && time.Now().Before(deadline) {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-time.After(timeout):
			return got
		}
	}
	return got
}
