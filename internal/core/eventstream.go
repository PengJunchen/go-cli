package core

import (
	"errors"
	"log/slog"
	"sync"
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
	mu     sync.Mutex
	events chan AgentEvent
	closed bool
	result AgentMessage
	hasRes bool
	err    error
}

var _ EventStream = (*EventStreamImpl)(nil)

// NewEventStream creates an EventStreamImpl with the given buffer capacity.
func NewEventStream(capacity int) *EventStreamImpl {
	return &EventStreamImpl{events: make(chan AgentEvent, capacity)}
}

// Send enqueues an event. It returns nil after close so a best-effort send
// does not fail the loop.
func (s *EventStreamImpl) Send(event AgentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.events <- event
	}
	slog.Info("core.eventstream.send", "kind", event.Kind)
	return nil
}

// Events returns the event channel.
func (s *EventStreamImpl) Events() <-chan AgentEvent { return s.events }

// Close closes the stream and the event channel.
func (s *EventStreamImpl) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.events)
		slog.Info("core.eventstream.close")
	}
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
