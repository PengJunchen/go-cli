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
	terminalOnce sync.Once
	done         chan struct{}
	result       AgentMessage
	hasRes       bool
	err          error
	sentCount    atomic.Int64
	gapCount     atomic.Uint64
	discard      DiscardPolicy
	blockTimeout time.Duration
	bus          EventBus
	statusHandle RunStatusHandle
}

var _ EventStream = (*EventStreamImpl)(nil)
var _ RunStatusSource = (*EventStreamImpl)(nil)

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
// Send blocks forever, preserving backward-compatible behavior. When the
// option is omitted, NewEventStream applies defaultBlockTimeout (30s).
func WithEventBlockTimeout(d time.Duration) EventStreamOption {
	return func(s *EventStreamImpl) { s.blockTimeout = d }
}

// NewEventStream creates an EventStreamImpl with the given buffer capacity.
func NewEventStream(capacity int, opts ...EventStreamOption) *EventStreamImpl {
	s := &EventStreamImpl{
		events:       make(chan AgentEvent, capacity),
		done:         make(chan struct{}),
		discard:      BlockUntilConsumed, // preserve backward-compatible blocking behavior
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
// The discard policy controls behavior when the bounded buffer is full:
//   - DiscardNewest: drop the incoming event (non-blocking).
//   - DiscardOldest: evict the oldest buffered event to make room (non-blocking).
//   - BlockUntilConsumed (default): block the sender until a consumer reads.
func (s *EventStreamImpl) Send(event AgentEvent) error {
	if s.closed.Load() {
		s.gapCount.Add(1)
		return nil
	}
	s.sendMu.RLock()
	defer s.sendMu.RUnlock()
	if s.closed.Load() {
		s.gapCount.Add(1)
		return nil
	}

	switch s.discard {
	case DiscardNewest:
		// Non-blocking: drop the new event if the buffer is full.
		select {
		case <-s.done:
			s.gapCount.Add(1)
			return nil
		case s.events <- event:
			s.sentCount.Add(1)
			s.publishToBus(event)
			slog.Debug("core.eventstream.send", "kind", event.Kind, "policy", "discard_newest")
			return nil
		default:
			s.gapCount.Add(1)
			slog.Warn("core.eventstream.discard", "kind", event.Kind, "policy", "discard_newest")
			return nil
		}

	case DiscardOldest:
		// Non-blocking: pop the oldest event to make room for the new one.
		select {
		case <-s.done:
			s.gapCount.Add(1)
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
				s.gapCount.Add(1)
				slog.Warn("core.eventstream.discard", "action", "evict_oldest", "policy", "discard_oldest")
			default:
			}
			select {
			case <-s.done:
				s.gapCount.Add(1)
				return nil
			case s.events <- event:
				s.sentCount.Add(1)
				s.publishToBus(event)
				slog.Debug("core.eventstream.send", "kind", event.Kind, "policy", "discard_oldest")
				return nil
			default:
				// Still full after eviction (concurrent sender); drop new event.
				s.gapCount.Add(1)
				return nil
			}
		}

	default: // BlockUntilConsumed
		select {
		case <-s.done:
			s.gapCount.Add(1)
			return nil
		case s.events <- event:
			s.sentCount.Add(1)
			s.publishToBus(event)
			return nil
		default:
			s.notifyBackpressure()
		}
		if s.blockTimeout > 0 {
			select {
			case <-s.done:
				s.gapCount.Add(1)
				return nil
			case s.events <- event:
				s.sentCount.Add(1)
				s.publishToBus(event)
				slog.Debug("core.eventstream.send", "kind", event.Kind, "policy", "block")
				return nil
			case <-time.After(s.blockTimeout):
				s.gapCount.Add(1)
				return ErrSendTimeout
			}
		}
		select {
		case <-s.done:
			s.gapCount.Add(1)
			return nil
		case s.events <- event:
			s.sentCount.Add(1)
			s.publishToBus(event)
			slog.Debug("core.eventstream.send", "kind", event.Kind, "policy", "block")
			return nil
		}
	}
}

func (s *EventStreamImpl) notifyBackpressure() {
	s.mu.Lock()
	handle := s.statusHandle
	s.mu.Unlock()
	if owner, ok := handle.(interface{ StartCleanupDeadline() }); ok {
		owner.StartCleanupDeadline()
	}
}

// SendTerminal makes one non-blocking attempt to enqueue the terminal event.
// A full or closed stream records a visible gap instead of delaying cleanup.
func (s *EventStreamImpl) SendTerminal(event AgentEvent) error {
	s.terminalOnce.Do(func() { s.sendTerminal(event) })
	return nil
}

func (s *EventStreamImpl) sendTerminal(event AgentEvent) {
	if s.closed.Load() {
		s.gapCount.Add(1)
		return
	}
	s.sendMu.RLock()
	defer s.sendMu.RUnlock()
	if s.closed.Load() {
		s.gapCount.Add(1)
		return
	}
	select {
	case <-s.done:
		s.gapCount.Add(1)
	case s.events <- event:
		s.sentCount.Add(1)
		s.publishToBus(event)
	default:
		s.gapCount.Add(1)
	}
}

// SentCount returns the number of events that were successfully sent to the
// stream. It is used by the harness to decide whether to fall back to fanning
// out the agent's stored events.
func (s *EventStreamImpl) SentCount() int {
	return int(s.sentCount.Load())
}

// GapCount reports attempted events that were discarded, timed out, or
// aborted while the stream was closing.
func (s *EventStreamImpl) GapCount() uint64 { return s.gapCount.Load() }

// SetRunStatusHandle associates the run's live status snapshot source.
func (s *EventStreamImpl) SetRunStatusHandle(handle RunStatusHandle) {
	s.mu.Lock()
	s.statusHandle = handle
	s.mu.Unlock()
}

// RunStatusHandle implements the optional RunStatusSource interface.
func (s *EventStreamImpl) RunStatusHandle() RunStatusHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusHandle
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
	return cloneAgentMessage(s.result), s.err
}

// Err returns the fatal stream error, if any.
func (s *EventStreamImpl) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
