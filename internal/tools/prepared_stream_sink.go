package tools

import (
	"errors"
	"reflect"
	"sync"
)

// ErrPreparedStreamSinkUnavailable indicates that a synchronous stream-sink
// attestation could not be created or used.
var ErrPreparedStreamSinkUnavailable = errors.New("tools: synchronous prepared stream sink is unavailable")

// ErrPreparedStreamSinkMismatch indicates that a dispatch supplied a different
// sink from the exact sink captured during preparation.
var ErrPreparedStreamSinkMismatch = errors.New("tools: prepared streaming sink does not match the captured sink")

// SynchronousStreamSink is a host-attested sink whose Send calls complete all
// invocation-owned output work before returning. Its unexported method seals
// implementations to this package; trusted host code obtains one through
// AttestSynchronousStreamSink. Calls through the wrapper are serialized, and
// the underlying Send callback must not recursively call Send on that wrapper.
type SynchronousStreamSink interface {
	StreamSink
	synchronousStreamSinkCapability()
}

type synchronousStreamSink struct {
	mu   sync.Mutex
	sink StreamSink
}

var _ SynchronousStreamSink = (*synchronousStreamSink)(nil)

func (*synchronousStreamSink) synchronousStreamSinkCapability() {}

func (s *synchronousStreamSink) Send(content, toolCallID, stream string) error {
	if s == nil || isNilStreamSink(s.sink) {
		return ErrPreparedStreamSinkUnavailable
	}
	// Streaming executors may call Send concurrently for stdout and stderr.
	// Serialize the underlying callback so a host's synchronous assertion
	// doesn't also assume that arbitrary sink implementations are concurrency-safe.
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sink.Send(content, toolCallID, stream)
}

// AttestSynchronousStreamSink wraps one exact sink instance after trusted host
// code has established that its Send method is synchronous and does not detach
// invocation-owned work. This is a host assertion, not a property inferred
// from the StreamSink interface or from a tool registration.
func AttestSynchronousStreamSink(sink StreamSink) (SynchronousStreamSink, error) {
	if isNilStreamSink(sink) {
		return nil, ErrPreparedStreamSinkUnavailable
	}
	return &synchronousStreamSink{sink: sink}, nil
}

func isNilStreamSink(sink StreamSink) bool {
	if sink == nil {
		return true
	}
	value := reflect.ValueOf(sink)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func isCapturedSynchronousStreamSink(expected *synchronousStreamSink, supplied StreamSink) bool {
	if expected == nil {
		return false
	}
	actual, ok := supplied.(*synchronousStreamSink)
	return ok && actual != nil && actual == expected
}
