package core

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
)

// EventBus is a pub/sub indirection layer for AgentEvent. It allows multiple
// consumers to subscribe to the same stream of events, decoupling the producer
// (EventStream) from the consumers (TUI, SSE server, etc.).
type EventBus interface {
	// Subscribe returns a channel that receives all events published after the
	// call. The channel is closed when the bus is closed or the context is
	// canceled. Each subscriber gets its own channel.
	Subscribe(ctx context.Context) <-chan AgentEvent
	// Publish sends an event to all current subscribers. Non-blocking: if a
	// subscriber's channel is full, the event is dropped for that subscriber.
	Publish(event AgentEvent)
	// Close shuts down the bus, closing all subscriber channels.
	Close()
}

// MemoryEventBus is an in-memory EventBus implementation.
type MemoryEventBus struct {
	mu            sync.RWMutex
	subscribers   map[chan AgentEvent]struct{}
	closed        bool
	closedCh      chan struct{}
	droppedEvents atomic.Uint64
}

// NewMemoryEventBus creates a new MemoryEventBus.
func NewMemoryEventBus() *MemoryEventBus {
	return &MemoryEventBus{
		subscribers: make(map[chan AgentEvent]struct{}),
		closedCh:    make(chan struct{}),
	}
}

// Subscribe returns a channel that receives published events. The channel has
// a buffer of 64. When ctx is canceled or the bus is closed, the channel is
// closed.
func (b *MemoryEventBus) Subscribe(ctx context.Context) <-chan AgentEvent {
	ch := make(chan AgentEvent, 64)
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		close(ch)
		return ch
	}
	b.subscribers[ch] = struct{}{}
	b.mu.Unlock()

	go func() {
		select {
		case <-ctx.Done():
		case <-b.closedCh:
		}
		b.removeSubscriber(ch)
	}()
	return ch
}

// Publish sends an event to all subscribers. If a subscriber's channel is full,
// the event is dropped for that subscriber (non-blocking) and the drop is
// counted and logged.
func (b *MemoryEventBus) Publish(event AgentEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return
	}
	for ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			// Subscriber buffer full, drop event.
			b.droppedEvents.Add(1)
			slog.Warn("core.eventbus.drop",
				"kind", event.Kind,
				"reason", "subscriber_buffer_full",
				"total_dropped", b.droppedEvents.Load(),
			)
		}
	}
}

// DroppedEvents returns the total number of events dropped because a
// subscriber's buffer was full. The counter is monotonically increasing.
func (b *MemoryEventBus) DroppedEvents() uint64 {
	return b.droppedEvents.Load()
}

// Close shuts down the bus and closes all subscriber channels.
func (b *MemoryEventBus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	close(b.closedCh)
	for ch := range b.subscribers {
		close(ch)
	}
	b.subscribers = nil
}

func (b *MemoryEventBus) removeSubscriber(ch chan AgentEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subscribers[ch]; ok {
		delete(b.subscribers, ch)
		close(ch)
	}
}

var _ EventBus = (*MemoryEventBus)(nil)
