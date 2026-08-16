package core

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// errNoResult reports that an EventStream has not yet received a result.
var errNoResult = errors.New("core: no result recorded on event stream")

// ErrSendTimeout reports that a Send on an EventStream using the
// BlockUntilConsumed policy timed out waiting for a consumer.
var ErrSendTimeout = errors.New("core: event stream send timed out")

// defaultBlockTimeout is the fallback applied by NewEventStream when
// WithEventBlockTimeout is not used. It prevents the agent loop from
// freezing indefinitely when a consumer is slow or absent.
const defaultBlockTimeout = 30 * time.Second

// EventStream is the asynchronous stream through which an agent emits events.
// It supports back-pressure-aware sending and closes when the run completes.
type EventStream interface {
	// Send enqueues an event into the stream.
	Send(event AgentEvent) error
	// Events returns the read-only event channel. Consumers read until it is
	// closed.
	Events() <-chan AgentEvent
	// Close closes the stream; buffered events may still be consumed.
	Close()
	// Result returns the final message of the run once the stream is closed.
	Result() (AgentMessage, error)
	// Err returns a fatal error that occurred on the stream, if any.
	Err() error
}

// EventStreamImpl is the default in-memory EventStream. It buffers events and
// records the final result and error of a run.
type EventStreamImpl struct {
	mu           sync.Mutex
	sendMu       sync.RWMutex
	events       chan AgentEvent
	closed       atomic.Bool
	closeOnce    sync.Once
	done         chan struct{}
	result       AgentMessage
	hasRes       bool
	err          error
	sentCount    atomic.Int64
	discard      DiscardPolicy
	blockTimeout time.Duration
	bus          EventBus
}

var _ EventStream = (*EventStreamImpl)(nil)

// EventStreamOption configures an EventStreamImpl at construction time.
type EventStreamOption func(*EventStreamImpl)

// WithEventDiscardPolicy sets the policy applied when the bounded event
// buffer is full: DiscardOldest evicts the oldest event, DiscardNewest
// drops the incoming event, BlockUntilConsumed (default) blocks the
// sender until a consumer reads.
func WithEventDiscardPolicy(p DiscardPolicy) EventStreamOption {
	return func(s *EventStreamImpl) { s.discard = p }
}

// WithEventBus wires an EventBus that receives a copy of every event
// successfully sent to the stream. The publish is non-blocking and
// nil-safe: when bus is nil, no dual-write occurs.
func WithEventBus(bus EventBus) EventStreamOption {
	return func(s *EventStreamImpl) { s.bus = bus }
}

// WithEventBlockTimeout sets the maximum duration a Send blocks under the
// BlockUntilConsumed policy before returning ErrSendTimeout. When d <= 0
// Send blocks forever, preserving backward-compatible behaviour. When the
// option is omitted, NewEventStream applies defaultBlockTimeout (30s).
func WithEventBlockTimeout(d time.Duration) EventStreamOption {
	return func(s *EventStreamImpl) { s.blockTimeout = d }
}

// NewEventStream creates an EventStreamImpl with the given buffer capacity.
func NewEventStream(capacity int, opts ...EventStreamOption) *EventStreamImpl {
	s := &EventStreamImpl{
		events:       make(chan AgentEvent, capacity),
		done:         make(chan struct{}),
		discard:      BlockUntilConsumed, // preserve backward-compatible blocking behaviour
		blockTimeout: defaultBlockTimeout,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Send enqueues an event. It returns nil after close so a best-effort send
// does not fail the loop. The atomic fast-path check and the select on the
// done channel together ensure that a send never blocks after Close and
// never panics on a closed channel. The sendMu read-lock guarantees that
// Close cannot close the events channel while a send is in-flight.
//
// The discard policy controls behaviour when the bounded buffer is full:
//   - DiscardNewest: drop the incoming event (non-blocking).
//   - DiscardOldest: evict the oldest buffered event to make room (non-blocking).
//   - BlockUntilConsumed (default): block the sender until a consumer reads.
func (s *EventStreamImpl) Send(event AgentEvent) error {
	if s.closed.Load() {
		return nil
	}
	s.sendMu.RLock()
	defer s.sendMu.RUnlock()
	if s.closed.Load() {
		return nil
	}

	switch s.discard {
	case DiscardNewest:
		// Non-blocking: drop the new event if the buffer is full.
		select {
		case <-s.done:
			return nil
		case s.events <- event:
			s.sentCount.Add(1)
			s.publishToBus(event)
			slog.Debug("core.eventstream.send", "kind", event.Kind, "policy", "discard_newest")
			return nil
		default:
			slog.Warn("core.eventstream.discard", "kind", event.Kind, "policy", "discard_newest")
			return nil
		}

	case DiscardOldest:
		// Non-blocking: pop the oldest event to make room for the new one.
		select {
		case <-s.done:
			return nil
		case s.events <- event:
			s.sentCount.Add(1)
			s.publishToBus(event)
			slog.Debug("core.eventstream.send", "kind", event.Kind, "policy", "discard_oldest")
			return nil
		default:
			// Buffer full: evict the oldest event.
			select {
			case <-s.events:
				slog.Warn("core.eventstream.discard", "action", "evict_oldest", "policy", "discard_oldest")
			default:
			}
			select {
			case <-s.done:
				return nil
			case s.events <- event:
				s.sentCount.Add(1)
				s.publishToBus(event)
				slog.Debug("core.eventstream.send", "kind", event.Kind, "policy", "discard_oldest")
				return nil
			default:
				// Still full after eviction (concurrent sender); drop new event.
				return nil
			}
		}

	default: // BlockUntilConsumed
		if s.blockTimeout > 0 {
			select {
			case <-s.done:
				return nil
			case s.events <- event:
				s.sentCount.Add(1)
				s.publishToBus(event)
				slog.Debug("core.eventstream.send", "kind", event.Kind, "policy", "block")
				return nil
			case <-time.After(s.blockTimeout):
				return ErrSendTimeout
			}
		}
		select {
		case <-s.done:
			return nil
		case s.events <- event:
			s.sentCount.Add(1)
			s.publishToBus(event)
			slog.Debug("core.eventstream.send", "kind", event.Kind, "policy", "block")
			return nil
		}
	}
}

// SentCount returns the number of events that were successfully sent to the
// stream. It is used by the harness to decide whether to fall back to fanning
// out the agent's stored events.
func (s *EventStreamImpl) SentCount() int {
	return int(s.sentCount.Load())
}

// publishToBus forwards a copy of the event to the wired EventBus (if any).
// The call is non-blocking and nil-safe.
func (s *EventStreamImpl) publishToBus(event AgentEvent) {
	if s.bus != nil {
		s.bus.Publish(event)
	}
}

// Events returns the event channel.
func (s *EventStreamImpl) Events() <-chan AgentEvent { return s.events }

// Close closes the stream and the event channel. It is idempotent via
// sync.Once. The done channel is closed first to unblock any in-flight
// Send (which exits via the select), then sendMu.Lock waits for all
// senders to release their read-lock before closing the events channel.
func (s *EventStreamImpl) Close() {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.done)
		s.sendMu.Lock()
		close(s.events)
		s.sendMu.Unlock()
		slog.Info("core.eventstream.close")
	})
}

// Result returns the recorded result of the run.
func (s *EventStreamImpl) Result() (AgentMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasRes {
		return AgentMessage{}, errNoResult
	}
	return s.result, s.err
}

// Err returns the fatal stream error, if any.
func (s *EventStreamImpl) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
