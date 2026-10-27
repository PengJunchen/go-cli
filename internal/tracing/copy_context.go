package tracing

import "context"

// CopyTraceContext copies only trace correlation into a new context value
// tree. It deliberately does not retain the source's runtime capabilities.
func CopyTraceContext(target, source context.Context) context.Context {
	if source == nil {
		return target
	}
	if tracer := tracerFromContext(source); tracer != nil {
		target = context.WithValue(target, tracerKey{}, tracer)
	}
	if span, ok := source.Value(spanCtxKey{}).(spanContext); ok {
		target = context.WithValue(target, spanCtxKey{}, span)
	}
	return target
}
