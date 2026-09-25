package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type wrappedPreparedEventStream struct {
	*EventStreamImpl
}

type customPreparedEventBus struct{}

func (customPreparedEventBus) Subscribe(context.Context) <-chan AgentEvent { return nil }
func (customPreparedEventBus) Publish(AgentEvent)                          {}
func (customPreparedEventBus) Close()                                      {}

func TestAttestedEventStreamSinkAcceptsDefaultInMemoryStream(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		stream *EventStreamImpl
	}{
		{name: "nil bus", stream: NewEventStream(2)},
		{name: "memory bus", stream: NewEventStream(2, WithEventBus(NewMemoryEventBus()))},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sink := attestedEventStreamSink(testCase.stream)
			require.NotNil(t, sink)
			require.NoError(t, sink.Send("output", "call-1", "stdout"))
			select {
			case event := <-testCase.stream.Events():
				assert.Equal(t, "tool_output", event.Kind)
				assert.Equal(t, "output", event.Content)
				assert.Equal(t, "call-1", event.ToolCallID)
				assert.Equal(t, "stdout", event.Stream)
			default:
				t.Fatal("attested sink did not synchronously send through the default stream")
			}
		})
	}
}

func TestAttestedEventStreamSinkRejectsUnknownStreamAndBus(t *testing.T) {
	stream := NewEventStream(2)
	var nilBus *MemoryEventBus
	for _, testCase := range []struct {
		name string
		es   EventStream
	}{
		{name: "nil stream"},
		{name: "wrapped stream", es: wrappedPreparedEventStream{EventStreamImpl: stream}},
		{name: "custom bus", es: NewEventStream(2, WithEventBus(customPreparedEventBus{}))},
		{name: "typed nil memory bus", es: NewEventStream(2, WithEventBus(nilBus))},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Nil(t, attestedEventStreamSink(testCase.es))
		})
	}
}
