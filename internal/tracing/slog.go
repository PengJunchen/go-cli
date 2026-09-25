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

type redactingSlogHandler struct {
	inner          slog.Handler
	sensitiveGroup bool
}

// NewRedactingSlogHandler wraps inner and masks user-controlled text attributes
// while preserving log messages, operation names, trace identifiers and
// numeric metadata. It can be installed around the CLI's configured handler
// to protect prompt and tool input fields emitted by all runtime packages.
func NewRedactingSlogHandler(inner slog.Handler) slog.Handler {
	return &redactingSlogHandler{inner: inner}
}

func (h *redactingSlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *redactingSlogHandler) Handle(ctx context.Context, record slog.Record) error {
	redacted := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	attrs := make([]slog.Attr, 0, record.NumAttrs())
	record.Attrs(func(attr slog.Attr) bool {
		if h.sensitiveGroup {
			attrs = append(attrs, redactSlogValue(attr))
		} else {
			attrs = append(attrs, redactSlogAttr(attr))
		}
		return true
	})
	redacted.AddAttrs(attrs...)
	return h.inner.Handle(ctx, redacted)
}

func (h *redactingSlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	redacted := make([]slog.Attr, len(attrs))
	for i := range attrs {
		if h.sensitiveGroup {
			redacted[i] = redactSlogValue(attrs[i])
		} else {
			redacted[i] = redactSlogAttr(attrs[i])
		}
	}
	return &redactingSlogHandler{inner: h.inner.WithAttrs(redacted), sensitiveGroup: h.sensitiveGroup}
}

func (h *redactingSlogHandler) WithGroup(name string) slog.Handler {
	return &redactingSlogHandler{
		inner:          h.inner.WithGroup(name),
		sensitiveGroup: h.sensitiveGroup || isSensitiveAttributeKey(name),
	}
}

func redactSlogAttr(attr slog.Attr) slog.Attr {
	value := attr.Value.Resolve()
	if isSensitiveAttributeKey(attr.Key) {
		return redactSlogValue(attr)
	}
	if value.Kind() == slog.KindGroup {
		group := append([]slog.Attr(nil), value.Group()...)
		for i := range group {
			group[i] = redactSlogAttr(group[i])
		}
		attr.Value = slog.GroupValue(group...)
		return attr
	}
	return attr
}

func redactSlogValue(attr slog.Attr) slog.Attr {
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		group := append([]slog.Attr(nil), value.Group()...)
		for i := range group {
			group[i] = redactSlogValue(group[i])
		}
		attr.Value = slog.GroupValue(group...)
		return attr
	}
	attr.Value = slog.StringValue(redactedValueWithLength(value.Any()))
	return attr
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
