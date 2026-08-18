// Package tracing provides distributed tracing primitives for emitting and
// exporting span telemetry. Every meaningful operation (LLM request, tool
// call, config load, command dispatch) can emit a Span. Spans carry
// trace_id / span_id / parent_span_id so the full execution tree can be
// reconstructed from JSONL logs regardless of run mode (subagent, parallel,
// sequential).
package tracing

// SpanKind describes the role a Span plays in a distributed trace.
type SpanKind string

const (
	// SpanKindClient represents an operation that makes an external request.
	SpanKindClient SpanKind = "CLIENT"
	// SpanKindServer represents an operation that receives an external request.
	SpanKindServer SpanKind = "SERVER"
	// SpanKindProducer represents an operation that sends an async message.
	SpanKindProducer SpanKind = "PRODUCER"
	// SpanKindConsumer represents an operation that receives an async message.
	SpanKindConsumer SpanKind = "CONSUMER"
	// SpanKindInternal represents an internal operation.
	SpanKindInternal SpanKind = "INTERNAL"
)

// SpanStatus describes the outcome of a Span.
type SpanStatus string

const (
	// SpanStatusOK indicates the span completed successfully.
	SpanStatusOK SpanStatus = "OK"
	// SpanStatusError indicates the span failed.
	SpanStatusError SpanStatus = "ERROR"
)

// Attribute is a key-value property attached to a Span.
type Attribute struct {
	Key       string `json:"key"`
	Value     any    `json:"value"`
	Sensitive bool   `json:"sensitive,omitempty"`
}

// SensitiveAttribute creates an Attribute marked as sensitive. Sensitive
// attributes are masked by the RedactingExporter before export.
func SensitiveAttribute(key string, value any) Attribute {
	return Attribute{Key: key, Value: value, Sensitive: true}
}

// SpanEvent is a timestamped log entry within a Span.
type SpanEvent struct {
	Name       string      `json:"name"`
	Timestamp  string      `json:"timestamp"` // RFC3339 nanosecond precision
	Attributes []Attribute `json:"attributes,omitempty"`
}

// SpanData is the complete serializable representation of a Span.
// It is used for JSONL export and persistence.
type SpanData struct {
	TraceID       string      `json:"trace_id"`
	SpanID        string      `json:"span_id"`
	ParentSpanID  string      `json:"parent_span_id,omitempty"`
	SpanKind      SpanKind    `json:"span_kind"`
	Name          string      `json:"name"`
	StartTime     string      `json:"start_time"`
	EndTime       string      `json:"end_time,omitempty"`
	Status        SpanStatus  `json:"status"`
	StatusMessage string      `json:"status_message,omitempty"`
	Attributes    []Attribute `json:"attributes,omitempty"`
	Events        []SpanEvent `json:"events,omitempty"`
}
