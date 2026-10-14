package extension

import (
	"context"
	"sync"
	"sync/atomic"
)

// BridgeEvent is a single event forwarded by the EventBridge. Index carries a
// sequence number so consumers can verify FIFO ordering; Data holds the opaque
// payload; Err carries a source-side error for the consumer if the event is
// forwarded.
type BridgeEvent struct {
	Index int
	Data  any
	Err   error
}

// EventBridge forwards events from a source channel to a destination channel
// using a single dedicated goroutine. Events successfully sent to the
// destination retain source order (FIFO). If cancellation wins the pending
// send after an event is read, that event is counted by Stats and dropped. A
// WaitGroup tracks the forwarding goroutine so callers can wait for it to exit.
//
// The destination channel is closed when the source closes or the context is
// canceled. The source channel remains owned by the caller and is not closed
// by the bridge.
type EventBridge struct {
	source <-chan BridgeEvent
	wg     sync.WaitGroup

	forwardOnce      sync.Once
	out              chan BridgeEvent
	forwarded        atomic.Uint64
	droppedAfterRead atomic.Uint64
}

// EventBridgeStats is a payload-free snapshot of forwarding outcomes.
type EventBridgeStats struct {
	// Forwarded is the number of events successfully sent to the destination.
	Forwarded uint64
	// DroppedAfterRead is the number of events read from the source but not sent
	// to the destination because the context was canceled.
	DroppedAfterRead uint64
}

// NewEventBridge creates an EventBridge that forwards events from the given
// source channel until it closes or forwarding is canceled.
func NewEventBridge(source <-chan BridgeEvent) *EventBridge {
	return &EventBridge{source: source}
}

// Forward starts a goroutine that drains the source and forwards events to the
// returned buffered channel in FIFO order. If cancellation wins after an event
// has been read but before it is sent, the event is counted as dropped in
// Stats. The returned channel is closed when forwarding completes (source
// closed or context canceled). Forward may be called multiple times; every call
// returns the same channel and only the first call starts the forwarding
// goroutine.
func (b *EventBridge) Forward(ctx context.Context) <-chan BridgeEvent {
	b.forwardOnce.Do(func() {
		b.out = make(chan BridgeEvent, 64)
		b.wg.Add(1)
		go b.forwardEvents(ctx, b.out)
	})
	return b.out
}

// forwardEvents drains the source channel and forwards events to out in FIFO
// order. It reads at most one event at a time. If cancellation wins while an
// event is waiting to be sent, it records the drop and exits. It closes out
// when the source is exhausted or the context is canceled, then signals the
// WaitGroup so Wait can return.
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
				b.forwarded.Add(1)
			case <-ctx.Done():
				b.droppedAfterRead.Add(1)
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// Stats returns a thread-safe, payload-free snapshot of forwarding outcomes.
func (b *EventBridge) Stats() EventBridgeStats {
	return EventBridgeStats{
		Forwarded:        b.forwarded.Load(),
		DroppedAfterRead: b.droppedAfterRead.Load(),
	}
}

// Wait blocks until all forwarding goroutines have exited and the destination
// channel has been closed. It is safe to call after the source has been closed.
func (b *EventBridge) Wait() {
	b.wg.Wait()
}
