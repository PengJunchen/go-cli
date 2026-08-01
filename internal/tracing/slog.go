package tracing

import (
	"context"
	"log/slog"
)

// traceHandler wraps an slog.Handler to inject trace fields (trace_id,
// span_id, parent_span_id) into every record emitted by the logger.
type traceHandler struct {
	inner slog.Handler
	span  TraceSpan
}

// NewTraceLogger returns an slog.Logger whose records automatically carry the
// given span's trace_id, span_id and parent_span_id. base may be nil to use
// slog.Default().
func NewTraceLogger(span TraceSpan, base *slog.Logger) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	return slog.New(&traceHandler{inner: base.Handler(), span: span})
}

// Enabled forwards to the inner handler.
func (h *traceHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle injects the trace fields into the record before delegating to the
// inner handler.
func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	r.AddAttrs(
		slog.String("trace_id", h.span.TraceID()),
		slog.String("span_id", h.span.SpanID()),
	)
	if pid := h.span.ParentSpanID(); pid != "" {
		r.AddAttrs(slog.String("parent_span_id", pid))
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs forwards to the inner handler while keeping the trace fields.
func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{inner: h.inner.WithAttrs(attrs), span: h.span}
}

// WithGroup forwards to the inner handler while keeping the trace fields.
func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{inner: h.inner.WithGroup(name), span: h.span}
}
