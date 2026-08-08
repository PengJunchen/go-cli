package extension

import (
	"context"
	"sync"
)

// BridgeEvent is a single event forwarded by the EventBridge. Index carries a
// sequence number so consumers can verify FIFO ordering; Data holds the opaque
// payload; Err carries a source-side error that must be propagated to the
// consumer without being dropped.
type BridgeEvent struct {
	Index int
	Data  any
	Err   error
}

// EventBridge forwards events from a source channel to a destination channel
// using a single dedicated goroutine. Because there is exactly one forwarding
// goroutine per source, events are delivered in the same order they are
// received from the source (FIFO) and none are lost: every event read from the
// source is written to the destination before the next one is read. A WaitGroup
// tracks the forwarding goroutine so callers can wait for all in-flight events
// to be fully drained.
//
// The destination channel is closed when the source closes or the context is
// canceled, signalling end-of-stream to consumers.
type EventBridge struct {
	source <-chan BridgeEvent
	wg     sync.WaitGroup
}

// NewEventBridge creates an EventBridge that drains the given source channel.
func NewEventBridge(source <-chan BridgeEvent) *EventBridge {
	return &EventBridge{source: source}
}

// Forward starts a goroutine that drains the source and forwards each event to
// the returned buffered channel, preserving FIFO order and completeness. The
// returned channel is closed when forwarding completes (source closed or
// context canceled).
func (b *EventBridge) Forward(ctx context.Context) <-chan BridgeEvent {
	out := make(chan BridgeEvent, 64)
	b.wg.Add(1)
	go b.forwardEvents(ctx, out)
	return out
}

// forwardEvents drains the source channel and forwards each event to out,
// preserving FIFO order. It reads at most one event from the source at a time
// and writes it to out before reading the next, which guarantees neither
// reordering nor loss. It closes out when the source is exhausted or the
// context is canceled, then signals the WaitGroup so Wait can return.
func (b *EventBridge) forwardEvents(ctx context.Context, out chan<- BridgeEvent) {
	defer b.wg.Done()
	defer close(out)
	for {
		select {
		case ev, ok := <-b.source:
			if !ok {
				return
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// Wait blocks until all forwarding goroutines have exited and the destination
// channel has been closed. It is safe to call after the source has been closed.
func (b *EventBridge) Wait() {
	b.wg.Wait()
}
