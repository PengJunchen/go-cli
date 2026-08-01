package production

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// TelemetryMetric is a single time-stamped, optionally labeled metric.
type TelemetryMetric struct {
	// Name identifies the metric.
	Name string
	// Value is the numeric value of the metric.
	Value float64
	// Labels attach key/value dimensions to the metric.
	Labels map[string]string
	// Timestamp records when the metric was produced.
	Timestamp time.Time
}

// Telemetry records named metrics for operational monitoring.
type Telemetry interface {
	// Record stores a metric, aggregating repeated values by name.
	Record(ctx context.Context, metric TelemetryMetric) error
	// Name returns the telemetry identifier.
	Name() string
}

// DefaultTelemetry collects metrics in memory. Repeated records for the same
// name are summed. A snapshot can be taken for export or assertion.
type DefaultTelemetry struct {
	mu       sync.RWMutex
	values   map[string]float64
	lastSeen map[string]time.Time
	name     string
}

// Compile-time assertion that DefaultTelemetry satisfies Telemetry.
var _ Telemetry = (*DefaultTelemetry)(nil)

// NewDefaultTelemetry returns an empty in-memory DefaultTelemetry.
func NewDefaultTelemetry(opts ...Option) *DefaultTelemetry {
	o := applyOptions(opts)
	name := o.name
	if name == "" {
		name = "default-telemetry"
	}
	return &DefaultTelemetry{
		values:   make(map[string]float64),
		lastSeen: make(map[string]time.Time),
		name:     name,
	}
}

// Record stores a metric, summing the value into the running total for its
// name. It emits a telemetry.record span and a debug-level log.
func (t *DefaultTelemetry) Record(ctx context.Context, metric TelemetryMetric) error {
	span, ctx := tracing.SpanFromContext(ctx, "telemetry.record", tracing.SpanKindInternal)
	defer span.End()

	if metric.Name == "" {
		return nil
	}
	ts := metric.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	t.mu.Lock()
	t.values[metric.Name] += metric.Value
	t.lastSeen[metric.Name] = ts
	t.mu.Unlock()

	span.SetAttributes(
		tracing.Attribute{Key: "metric_name", Value: metric.Name},
		tracing.Attribute{Key: "value", Value: metric.Value},
		tracing.Attribute{Key: "label_count", Value: len(metric.Labels)},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.DebugContext(ctx, "telemetry.record",
		"metric_name", metric.Name,
		"value", metric.Value,
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// Snapshot returns a copy of the aggregated metric values keyed by name.
func (t *DefaultTelemetry) Snapshot() map[string]float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]float64, len(t.values))
	for k, v := range t.values {
		out[k] = v
	}
	return out
}

// Name returns the telemetry identifier.
func (t *DefaultTelemetry) Name() string { return t.name }
