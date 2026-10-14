package production

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestTelemetryRecordAndSnapshot(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck

	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "tokens", Value: 10}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "tokens", Value: 5}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "calls", Value: 1, Labels: map[string]string{"tool": "edit"}}))

	snap := tm.Snapshot()
	assert.Equal(t, 15.0, snap[metricKey("tokens", nil)])
	assert.Equal(t, 1.0, snap[metricKey("calls", map[string]string{"tool": "edit"})])
}

func TestTelemetryRecordEmptyNameIgnored(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck

	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "", Value: 42}))
	snap := tm.Snapshot()
	assert.Len(t, snap, 2, "empty metric name should not add a regular series")
	assert.Zero(t, snap[metricKey(unknownLabelValueMetric, nil)])
	assert.Zero(t, snap[metricKey(seriesLimitDroppedMetric, nil)])
}

func TestTelemetryNameAndOption(t *testing.T) {
	tm := NewDefaultTelemetry()
	assert.Equal(t, "default-telemetry", tm.Name())
	assert.Equal(t, "custom", NewDefaultTelemetry(WithName("custom")).Name())
}

func TestTelemetryConcurrentRecord(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck

	var wg sync.WaitGroup
	const goroutines = 8
	const per = 100
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				discardErr(tm.Record(ctx, TelemetryMetric{Name: "count", Value: 1}))
			}
		}()
	}
	wg.Wait()

	snap := tm.Snapshot()
	assert.Equal(t, float64(goroutines*per), snap[metricKey("count", nil)])
}

func TestTelemetryRecordSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, exporter := newIdemTestCtx(t)
	tm := NewDefaultTelemetry()

	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "latency", Value: 2.5}))

	require.Eventually(t, func() bool {
		return exporter.hasSpan("telemetry.record")
	}, time.Second, 5*time.Millisecond, "expected telemetry.record span")
}

func TestTelemetryContextCancellation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck

	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "x", Value: 1}))
	snap := tm.Snapshot()
	assert.Equal(t, 1.0, snap[metricKey("x", nil)])
}

func TestTelemetryLabelDimensionsStoredSeparately(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck

	require.NoError(t, tm.Record(ctx, TelemetryMetric{
		Name:   "http_requests",
		Value:  5,
		Labels: map[string]string{"method": "GET", "status": "200"},
	}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{
		Name:   "http_requests",
		Value:  3,
		Labels: map[string]string{"method": "POST", "status": "500"},
	}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{
		Name:   "http_requests",
		Value:  2,
		Labels: map[string]string{"method": "GET", "status": "200"},
	}))

	snap := tm.Snapshot()
	// Same name, different labels → separate keys, NOT aggregated.
	assert.Equal(t, 7.0, snap[metricKey("http_requests", map[string]string{"method": "GET", "status": "200"})])
	assert.Equal(t, 3.0, snap[metricKey("http_requests", map[string]string{"method": "POST", "status": "500"})])
	// Bare name key should not exist.
	_, ok := snap[metricKey("http_requests", nil)]
	assert.False(t, ok, "metrics with labels should not aggregate under bare name")
}

func TestTelemetrySnapshotReturnsLabelDimensions(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck

	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "latency", Value: 1.5}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{
		Name:   "latency",
		Value:  2.0,
		Labels: map[string]string{"route": "/api"},
	}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{
		Name:   "latency",
		Value:  3.0,
		Labels: map[string]string{"route": "/health"},
	}))

	snap := tm.Snapshot()
	// Three distinct keys: unlabelled family + two label variants.
	assert.Contains(t, snap, metricKey("latency", nil))
	assert.Contains(t, snap, metricKey("latency", map[string]string{"route": "/api"}))
	assert.Contains(t, snap, metricKey("latency", map[string]string{"route": "/health"}))
	assert.Equal(t, 1.5, snap[metricKey("latency", nil)])
	assert.Equal(t, 2.0, snap[metricKey("latency", map[string]string{"route": "/api"})])
	assert.Equal(t, 3.0, snap[metricKey("latency", map[string]string{"route": "/health"})])
}

func TestMetricKeyDeterministic(t *testing.T) {
	// No labels have one stable, framed representation.
	assert.Equal(t, metricKey("gauge", nil), metricKey("gauge", map[string]string{}))

	// Labels sorted by key for deterministic output.
	k1 := metricKey("req", map[string]string{"status": "200", "method": "GET"})
	k2 := metricKey("req", map[string]string{"method": "GET", "status": "200"})
	assert.Equal(t, k1, k2, "label order should not affect key")
}

func TestTelemetryLabelsAreInjective(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck
	ctx := context.Background()

	cases := []struct {
		name    string
		metricA string
		labelsA map[string]string
		metricB string
		labelsB map[string]string
	}{
		{
			name:    "separator in a label value",
			metricA: "requests",
			labelsA: map[string]string{"a": "x,b=y"},
			metricB: "requests",
			labelsB: map[string]string{"a": "x", "b": "y"},
		},
		{
			name:    "bare family resembles a labeled family",
			metricA: "requests{method=GET}",
			metricB: "requests",
			labelsB: map[string]string{"method": "GET"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyA := metricKey(tc.metricA, tc.labelsA)
			keyB := metricKey(tc.metricB, tc.labelsB)
			require.NotEqual(t, keyA, keyB, "different metric tuples must have different keys")

			require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: tc.metricA, Value: 1, Labels: tc.labelsA}))
			require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: tc.metricB, Value: 2, Labels: tc.labelsB}))
			snap := tm.Snapshot()
			assert.Equal(t, 1.0, snap[keyA])
			assert.Equal(t, 2.0, snap[keyB])
		})
	}
}

func TestTelemetryBuiltinLabelsFoldUnknownValuesAndRejectUnknownKeys(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck
	ctx := context.Background()

	require.NoError(t, tm.Record(ctx, TelemetryMetric{
		Name:  "tool.call.count",
		Value: 1,
		Labels: map[string]string{
			"tool_class": "custom-tool-name",
			"cache_hit":  "true",
		},
	}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{
		Name:  "tool.call.count",
		Value: 2,
		Labels: map[string]string{
			"tool_class": "read",
			"cache_hit":  "unrecognized-cache-state",
		},
	}))
	snap := tm.Snapshot()
	assert.Equal(t, 1.0, snap[metricKey("tool.call.count", map[string]string{"tool_class": "other", "cache_hit": "true"})])
	assert.Equal(t, 2.0, snap[metricKey("tool.call.count", map[string]string{"tool_class": "read", "cache_hit": "other"})])
	assert.Equal(t, 2.0, snap[metricKey(unknownLabelValueMetric, nil)])

	err := tm.Record(ctx, TelemetryMetric{
		Name:  "tool.call.count",
		Value: 1,
		Labels: map[string]string{
			"tool":      "raw-tool-name",
			"cache_hit": "false",
		},
	})
	require.ErrorIs(t, err, ErrUnknownMetricLabel, "builtin metric families must reject unrecognized label keys")
	assert.NotContains(t, tm.Snapshot(), metricKey("tool.call.count", map[string]string{"tool": "raw-tool-name", "cache_hit": "false"}))

	err = tm.Record(ctx, TelemetryMetric{Name: "tool.call.duration_ms", Value: 1})
	require.ErrorIs(t, err, ErrInvalidMetricLabels, "required built-in dimensions must be present")
	err = tm.Record(ctx, TelemetryMetric{Name: "llm.tokens.input", Value: 1, Labels: map[string]string{"model": "private-model"}})
	require.ErrorIs(t, err, ErrUnknownMetricLabel, "token metrics must reject labels")
}

func TestTelemetryReservedCountersCannotBeForged(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck
	ctx := context.Background()

	for _, name := range []string{unknownLabelValueMetric, seriesLimitDroppedMetric} {
		for _, labels := range []map[string]string{nil, {"forged": "true"}} {
			err := tm.Record(ctx, TelemetryMetric{Name: name, Value: 999, Labels: labels})
			require.ErrorIs(t, err, ErrReservedTelemetryMetric, "reserved snapshot counters must not be writable via Record")
		}
		assert.Contains(t, tm.Snapshot(), metricKey(name, nil))
		assert.Zero(t, tm.Snapshot()[metricKey(name, nil)])
	}
}

func TestTelemetrySeriesCapAndReservedSnapshotCounters(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck
	ctx := context.Background()

	const seriesCap = DefaultMaxSeries
	for i := 0; i < seriesCap; i++ {
		require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: fmt.Sprintf("custom.metric.%04d", i), Value: 1}))
	}

	err := tm.Record(ctx, TelemetryMetric{Name: "custom.metric.overflow.1", Value: 1})
	require.True(t, errors.Is(err, ErrTelemetrySeriesLimit), "a new tuple beyond the global series cap must be rejected")
	err = tm.Record(ctx, TelemetryMetric{Name: "custom.metric.overflow.2", Value: 1})
	require.True(t, errors.Is(err, ErrTelemetrySeriesLimit), "every rejected new tuple must be counted")
	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "custom.metric.0000", Value: 2}), "existing tuples remain aggregatable at capacity")

	snap := tm.Snapshot()
	regularSeries := 0
	for key := range snap {
		if key == metricKey("unknown_label_value_total", nil) || key == metricKey("series_limit_dropped_total", nil) {
			continue
		}
		regularSeries++
	}
	assert.Equal(t, seriesCap, regularSeries)
	assert.Equal(t, 3.0, snap[metricKey("custom.metric.0000", nil)])
	assert.Equal(t, 0.0, snap[metricKey("unknown_label_value_total", nil)])
	assert.Equal(t, 2.0, snap[metricKey("series_limit_dropped_total", nil)])
}

func TestTelemetryConcurrentSeriesCapIsBounded(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck
	ctx := context.Background()

	const (
		writers      = 8
		seriesPerRun = 256
		seriesCap    = DefaultMaxSeries
	)
	var wg sync.WaitGroup
	done := make(chan struct{})
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < seriesPerRun; i++ {
				_ = tm.Record(ctx, TelemetryMetric{Name: fmt.Sprintf("parallel.%02d.%03d", g, i), Value: 1})
			}
		}(g)
	}
	go func() {
		wg.Wait()
		close(done)
	}()

	for {
		select {
		case <-done:
			regularSeries := 0
			for key := range tm.Snapshot() {
				if key == metricKey("unknown_label_value_total", nil) || key == metricKey("series_limit_dropped_total", nil) {
					continue
				}
				regularSeries++
			}
			assert.Equal(t, seriesCap, regularSeries)
			assert.Equal(t, float64(writers*seriesPerRun-seriesCap), tm.Snapshot()[metricKey("series_limit_dropped_total", nil)])
			return
		default:
			_ = tm.Snapshot()
		}
	}
}
