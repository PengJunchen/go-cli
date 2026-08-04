package tracing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// TraceSpan defines the interface for a tracing span.
// A span starts timing at creation and ends (and is exported) when End is called.
type TraceSpan interface {
	// TraceID returns the ID of the trace this span belongs to.
	TraceID() string
	// SpanID returns the unique ID of this span.
	SpanID() string
	// ParentSpanID returns the parent span's ID, or "" for a root span.
	ParentSpanID() string
	// Name returns the operation name of the span.
	Name() string
	// StartTime returns the span's start time.
	StartTime() time.Time
	// EndTime returns the span's end time, or the zero value before End.
	EndTime() time.Time
	// SetAttributes appends attributes to the span.
	SetAttributes(attrs ...Attribute)
	// AddEvent adds a timestamped event to the span.
	AddEvent(name string, attrs ...Attribute)
	// SetStatus sets the span status and message.
	SetStatus(status SpanStatus, msg string)
	// End ends the span. End is idempotent; the span is exported exactly once.
	End()
	// Context returns a context carrying this span's trace information,
	// suitable for creating child spans.
	Context() context.Context
}

// tracerKey is the context key under which the owning *Tracer is stored.
// It allows SpanFromContext to create child spans without a direct reference
// to a Tracer.
type tracerKey struct{}

// spanCtxKey is the context key under which the current span context is stored.
type spanCtxKey struct{}

// spanContext holds the minimal trace information propagated to child spans.
type spanContext struct {
	traceID string
	spanID  string
}

// spanIDFromContext returns the parent span ID carried in ctx, or "" if none.
func spanIDFromContext(ctx context.Context) string {
	sc, ok := ctx.Value(spanCtxKey{}).(spanContext)
	if !ok {
		return ""
	}
	return sc.spanID
}

// tracerFromContext returns the *Tracer stored in ctx, or nil if none.
func tracerFromContext(ctx context.Context) *Tracer {
	t, ok := ctx.Value(tracerKey{}).(*Tracer)
	if !ok {
		return nil
	}
	return t
}

// Tracer is the factory for Spans. Each CLI process owns one Tracer that
// manages the current TraceID, an enable switch and the destination
// TraceExporter.
type Tracer struct {
	traceID  string
	exporter TraceExporter
	enabled  atomic.Bool
}

// NewTracer creates a new Tracer. When traceID is empty a random ID is
// generated.
func NewTracer(traceID string, exporter TraceExporter) *Tracer {
	if traceID == "" {
		traceID = generateID()
	}
	t := &Tracer{
		traceID:  traceID,
		exporter: exporter,
	}
	t.enabled.Store(true)
	return t
}

// TraceID returns the ID of the trace managed by this Tracer.
func (t *Tracer) TraceID() string { return t.traceID }

// SetEnabled toggles span creation. When disabled, Start returns a noopSpan
// so tracing is effectively zero-overhead until re-enabled.
func (t *Tracer) SetEnabled(enabled bool) { t.enabled.Store(enabled) }

// Start creates and starts a new Span. If ctx carries parent span information,
// the new span inherits it as its parent_span_id. The returned context carries
// both the new span context and this Tracer, so child spans can be created via
// Tracer.Start or the SpanFromContext helper.
func (t *Tracer) Start(ctx context.Context, name string, kind SpanKind) (TraceSpan, context.Context) {
	if !t.enabled.Load() {
		return &noopSpan{}, ctx
	}

	parentSpanID := spanIDFromContext(ctx)
	spanID := generateID()
	span := &localSpan{
		traceID:      t.traceID,
		spanID:       spanID,
		parentSpanID: parentSpanID,
		kind:         kind,
		name:         name,
		startTime:    time.Now(),
		exporter:     t.exporter,
	}

	sc := spanContext{traceID: t.traceID, spanID: spanID}
	newCtx := context.WithValue(ctx, spanCtxKey{}, sc)
	newCtx = context.WithValue(newCtx, tracerKey{}, t)
	span.ctx = newCtx

	return span, newCtx
}

// ContextWithTracer returns a context that carries the Tracer so that
// SpanFromContext can create real spans. It does not start a span itself.
// Callers use this to inject a Tracer at the outermost layer of a request so
// that all downstream SpanFromContext calls (middleware, loop, tools) share
// the same trace.
func (t *Tracer) ContextWithTracer(ctx context.Context) context.Context {
	return context.WithValue(ctx, tracerKey{}, t)
}

// SpanFromContext starts a child Span using the Tracer stored in ctx. It is
// the ergonomic entry point for tracing arbitrary code paths:
//
//	span, ctx := tracing.SpanFromContext(ctx, "config.load", tracing.SpanKindInternal)
//	defer span.End()
//
// SpanFromContext needs a *Tracer to create a real span, so Start injects the
// Tracer into the returned context (via tracerKey). When no Tracer is present
// (e.g. tracing is disabled), a noopSpan is returned and ctx is untouched.
func SpanFromContext(ctx context.Context, name string, kind SpanKind) (TraceSpan, context.Context) {
	if t := tracerFromContext(ctx); t != nil {
		return t.Start(ctx, name, kind)
	}
	return &noopSpan{}, ctx
}

// generateID returns a random 32-char hex ID.
func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// localSpan is the concrete TraceSpan implementation.
type localSpan struct {
	traceID      string
	spanID       string
	parentSpanID string
	kind         SpanKind
	name         string
	startTime    time.Time
	endTime      time.Time
	status       SpanStatus
	statusMsg    string
	attributes   []Attribute
	events       []SpanEvent
	exporter     TraceExporter
	ctx          context.Context

	mu    sync.Mutex
	ended bool
}

func (s *localSpan) TraceID() string      { return s.traceID }
func (s *localSpan) SpanID() string       { return s.spanID }
func (s *localSpan) ParentSpanID() string { return s.parentSpanID }
func (s *localSpan) Name() string         { return s.name }
func (s *localSpan) StartTime() time.Time { return s.startTime }

// EndTime returns the span's end time. It is guarded by the mutex because End
// may write s.endTime concurrently.
func (s *localSpan) EndTime() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endTime
}

// SetAttributes appends attributes to the span.
func (s *localSpan) SetAttributes(attrs ...Attribute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attributes = append(s.attributes, attrs...)
}

// AddEvent appends a timestamped event to the span.
func (s *localSpan) AddEvent(name string, attrs ...Attribute) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, SpanEvent{
		Name:       name,
		Timestamp:  time.Now().Format(time.RFC3339Nano),
		Attributes: attrs,
	})
}

// SetStatus sets the span status and message.
func (s *localSpan) SetStatus(status SpanStatus, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
	s.statusMsg = msg
}

// Context returns the context carrying this span's trace information.
func (s *localSpan) Context() context.Context { return s.ctx }

// End finalizes the span. End is idempotent: only the first call records the
// end time and triggers an asynchronous export when an exporter is present.
func (s *localSpan) End() {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.endTime = time.Now()
	s.mu.Unlock()

	if s.exporter != nil {
		go func() {
			if err := s.exporter.ExportSpan(context.Background(), s); err != nil {
				slog.Warn("failed to export span", "span_id", s.spanID, "err", err)
			}
		}()
	}
}

// ToSpanData converts the localSpan into a serializable SpanData.
func (s *localSpan) ToSpanData() SpanData {
	s.mu.Lock()
	defer s.mu.Unlock()

	status := s.status
	if status == "" {
		status = SpanStatusOK
	}

	return SpanData{
		TraceID:       s.traceID,
		SpanID:        s.spanID,
		ParentSpanID:  s.parentSpanID,
		SpanKind:      s.kind,
		Name:          s.name,
		StartTime:     s.startTime.Format(time.RFC3339Nano),
		EndTime:       s.endTime.Format(time.RFC3339Nano),
		Status:        status,
		StatusMessage: s.statusMsg,
		Attributes:    s.attributes,
		Events:        s.events,
	}
}

// noopSpan is a zero-cost TraceSpan used when tracing is disabled.
type noopSpan struct{}

func (noopSpan) TraceID() string               { return "" }
func (noopSpan) SpanID() string                { return "" }
func (noopSpan) ParentSpanID() string          { return "" }
func (noopSpan) Name() string                  { return "" }
func (noopSpan) StartTime() time.Time          { return time.Time{} }
func (noopSpan) EndTime() time.Time            { return time.Time{} }
func (noopSpan) SetAttributes(...Attribute)    {}
func (noopSpan) AddEvent(string, ...Attribute) {}
func (noopSpan) SetStatus(SpanStatus, string)  {}
func (noopSpan) End()                          {}
func (noopSpan) Context() context.Context      { return context.Background() }

// Compile-time assertions that the concrete span types satisfy TraceSpan.
var _ TraceSpan = (*localSpan)(nil)
var _ TraceSpan = (*noopSpan)(nil)
