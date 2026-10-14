package production

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

const (
	// DefaultMaxSeries bounds the number of caller-provided metric tuples kept
	// by DefaultTelemetry. Reserved snapshot counters are outside this limit.
	DefaultMaxSeries = 1024

	unknownLabelValueMetric  = "unknown_label_value_total"
	seriesLimitDroppedMetric = "series_limit_dropped_total"
)

var (
	// ErrUnknownMetricLabel reports an unrecognized label key for a built-in
	// metric family.
	ErrUnknownMetricLabel = errors.New("unknown telemetry metric label")
	// ErrInvalidMetricLabels reports missing or invalid labels for a built-in
	// metric family.
	ErrInvalidMetricLabels = errors.New("invalid telemetry metric labels")
	// ErrReservedTelemetryMetric reports attempts to write an internally
	// maintained snapshot counter through Record.
	ErrReservedTelemetryMetric = errors.New("reserved telemetry metric")
	// ErrTelemetrySeriesLimit reports that a new metric tuple exceeded the
	// DefaultTelemetry series limit.
	ErrTelemetrySeriesLimit = errors.New("telemetry series limit reached")
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
// normalized metric tuple are summed. A snapshot can be taken for export or
// assertion.
type DefaultTelemetry struct {
	mu                 sync.RWMutex
	values             map[string]float64
	lastSeen           map[string]time.Time
	unknownLabelValue  uint64
	seriesLimitDropped uint64
	name               string
}

// Compile-time assertion that DefaultTelemetry satisfies Telemetry.
var _ Telemetry = (*DefaultTelemetry)(nil)

// NewDefaultTelemetry returns an empty in-memory DefaultTelemetry.
func NewDefaultTelemetry(opts ...Option) Telemetry {
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
// normalized name+labels tuple. It emits a telemetry.record span and a debug-level log.
func (t *DefaultTelemetry) Record(ctx context.Context, metric TelemetryMetric) error {
	span, ctx := tracing.SpanFromContext(ctx, "telemetry.record", tracing.SpanKindInternal)
	defer span.End()

	if metric.Name == "" {
		return nil
	}
	if isReservedMetric(metric.Name) {
		span.SetStatus(tracing.SpanStatusError, ErrReservedTelemetryMetric.Error())
		return ErrReservedTelemetryMetric
	}

	labels, unknownValues, err := normalizeBuiltinMetric(metric.Name, metric.Labels)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
		return err
	}
	ts := metric.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	key := metricKey(metric.Name, labels)
	t.mu.Lock()
	t.unknownLabelValue += uint64(unknownValues)
	_, exists := t.values[key]
	if !exists && len(t.values) >= DefaultMaxSeries {
		t.seriesLimitDropped++
		t.mu.Unlock()
		span.SetStatus(tracing.SpanStatusError, ErrTelemetrySeriesLimit.Error())
		return ErrTelemetrySeriesLimit
	}
	t.values[key] += metric.Value
	t.lastSeen[key] = ts
	t.mu.Unlock()

	span.SetAttributes(
		tracing.Attribute{Key: "metric_name", Value: metric.Name},
		tracing.Attribute{Key: "value", Value: metric.Value},
		tracing.Attribute{Key: "label_count", Value: len(labels)},
	)
	logger := tracing.NewTraceLogger(span, slog.Default())
	logger.DebugContext(ctx, "telemetry.record",
		"metric_name", metric.Name,
		"value", metric.Value,
	)
	span.SetStatus(tracing.SpanStatusOK, "")
	return nil
}

// Snapshot returns a copy of regular metric values keyed by an injective,
// versioned encoding of each normalized name+labels tuple. It always includes
// the two internally maintained, non-writable counters.
func (t *DefaultTelemetry) Snapshot() map[string]float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]float64, len(t.values)+2)
	for k, v := range t.values {
		out[k] = v
	}
	out[metricKey(unknownLabelValueMetric, nil)] = float64(t.unknownLabelValue)
	out[metricKey(seriesLimitDroppedMetric, nil)] = float64(t.seriesLimitDropped)
	return out
}

// Name returns the telemetry identifier.
func (t *DefaultTelemetry) Name() string { return t.name }

func isReservedMetric(name string) bool {
	return name == unknownLabelValueMetric || name == seriesLimitDroppedMetric
}

func normalizeBuiltinMetric(name string, input map[string]string) (map[string]string, int, error) {
	var allowed map[string]map[string]struct{}
	switch name {
	case "tool.call.count":
		allowed = map[string]map[string]struct{}{
			"tool_class": finiteValues("read", "mutation", "process", "web", "mcp", "skill", "extension", "other"),
			"cache_hit":  finiteValues("true", "false"),
		}
	case "tool.call.duration_ms":
		allowed = map[string]map[string]struct{}{
			"tool_class": finiteValues("read", "mutation", "process", "web", "mcp", "skill", "extension", "other"),
		}
	case "llm.tokens.input", "llm.tokens.output":
		allowed = map[string]map[string]struct{}{}
	default:
		return cloneLabels(input), 0, nil
	}

	for key := range input {
		if _, ok := allowed[key]; !ok {
			return nil, 0, ErrUnknownMetricLabel
		}
	}
	if len(input) != len(allowed) {
		return nil, 0, ErrInvalidMetricLabels
	}

	labels := cloneLabels(input)
	unknownValues := 0
	for key, values := range allowed {
		value, ok := labels[key]
		if !ok {
			return nil, 0, ErrInvalidMetricLabels
		}
		if _, ok := values[value]; !ok {
			labels[key] = "other"
			unknownValues++
		}
	}
	return labels, unknownValues, nil
}

func finiteValues(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func cloneLabels(labels map[string]string) map[string]string {
	if len(labels) == 0 {
		return nil
	}
	copy := make(map[string]string, len(labels))
	for key, value := range labels {
		copy[key] = value
	}
	return copy
}

// metricKey builds an injective versioned key for a metric family and its
// labels. Each string is prefixed by its UTF-8 byte length, and label keys are
// sorted by bytes, so delimiters in names, keys, or values cannot create a
// collision between distinct tuples.
func metricKey(name string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var builder strings.Builder
	builder.WriteString("metric-v1|")
	writeMetricField(&builder, name)
	builder.WriteString(strconv.Itoa(len(keys)))
	builder.WriteByte('|')
	for _, key := range keys {
		writeMetricField(&builder, key)
		writeMetricField(&builder, labels[key])
	}
	return builder.String()
}

func writeMetricField(builder *strings.Builder, value string) {
	builder.WriteString(strconv.Itoa(len(value)))
	builder.WriteByte(':')
	builder.WriteString(value)
	builder.WriteByte('|')
}
