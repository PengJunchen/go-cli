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
	mu        sync.Mutex
	events    chan AgentEvent
	closed    bool
	result    AgentMessage
	hasRes    bool
	err       error
	sentCount int
}

var _ EventStream = (*EventStreamImpl)(nil)

// NewEventStream creates an EventStreamImpl with the given buffer capacity.
func NewEventStream(capacity int) *EventStreamImpl {
	return &EventStreamImpl{events: make(chan AgentEvent, capacity)}
}

// Send enqueues an event. It returns nil after close so a best-effort send
// does not fail the loop. The mutex is NOT held during the channel send to
// avoid blocking Close()/Result() while waiting for a slow consumer.
func (s *EventStreamImpl) Send(event AgentEvent) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	// Send without holding the lock. Use recover to handle the race where
	// Close() closes the channel between the check above and this send.
	sent := true
	func() {
		defer func() {
			if r := recover(); r != nil {
				sent = false
			}
		}()
		s.events <- event
	}()

	if sent {
		s.mu.Lock()
		s.sentCount++
		s.mu.Unlock()

		slog.Info("core.eventstream.send", "kind", event.Kind)
	}
	return nil
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
