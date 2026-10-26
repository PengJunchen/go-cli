package core

import (
	"context"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// attestedEventStreamSink returns the exact default in-memory event adapter
// only when EventStream.Send and its optional default MemoryEventBus publish
// are synchronous. Custom stream or bus implementations are not attested.
func attestedEventStreamSink(es EventStream) tools.SynchronousStreamSink {
	return attestedEventStreamSinkForContext(context.Background(), es)
}

func attestedEventStreamSinkForContext(ctx context.Context, es EventStream) tools.SynchronousStreamSink {
	stream, ok := es.(*EventStreamImpl)
	if !ok || stream == nil {
		return nil
	}
	switch bus := stream.bus.(type) {
	case nil:
	case *MemoryEventBus:
		if bus == nil {
			return nil
		}
	default:
		return nil
	}

	attested, err := tools.AttestSynchronousStreamSink(&eventStreamSink{ctx: ctx, es: stream, coordinates: agentEventCoordinatesFromContext(ctx)})
	if err != nil {
		return nil
	}
	return attested
}
