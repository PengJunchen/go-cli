package core

import (
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
)

// errNoResult reports that an EventStream has not yet received a result.
var errNoResult = errors.New("core: no result recorded on event stream")

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
	mu        sync.Mutex
	sendMu    sync.RWMutex
	events    chan AgentEvent
	closed    atomic.Bool
	closeOnce sync.Once
	done      chan struct{}
	result    AgentMessage
	hasRes    bool
	err       error
	sentCount int
}

var _ EventStream = (*EventStreamImpl)(nil)

// NewEventStream creates an EventStreamImpl with the given buffer capacity.
func NewEventStream(capacity int) *EventStreamImpl {
	return &EventStreamImpl{
		events: make(chan AgentEvent, capacity),
		done:   make(chan struct{}),
	}
}

// Send enqueues an event. It returns nil after close so a best-effort send
// does not fail the loop. The atomic fast-path check and the select on the
// done channel together ensure that a send never blocks after Close and
// never panics on a closed channel. The sendMu read-lock guarantees that
// Close cannot close the events channel while a send is in-flight.
func (s *EventStreamImpl) Send(event AgentEvent) error {
	if s.closed.Load() {
		return nil
	}
	s.sendMu.RLock()
	defer s.sendMu.RUnlock()
	if s.closed.Load() {
		return nil
	}
	select {
	case <-s.done:
		return nil
	case s.events <- event:
		s.mu.Lock()
		s.sentCount++
		s.mu.Unlock()
		slog.Info("core.eventstream.send", "kind", event.Kind)
		return nil
	}
}

// SentCount returns the number of events that were successfully sent to the
// stream. It is used by the harness to decide whether to fall back to fanning
// out the agent's stored events.
func (s *EventStreamImpl) SentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sentCount
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
