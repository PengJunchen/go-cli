package production

import (
	"context"
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
	assert.Equal(t, 15.0, snap["tokens"])
	assert.Equal(t, 1.0, snap["calls{tool=edit}"])
}

func TestTelemetryRecordEmptyNameIgnored(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	tm := NewDefaultTelemetry().(*DefaultTelemetry) //nolint:errcheck

	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "", Value: 42}))
	assert.Empty(t, tm.Snapshot(), "empty metric name should not be stored")
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
	assert.Equal(t, float64(goroutines*per), snap["count"])
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
	assert.Equal(t, 1.0, snap["x"])
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
	assert.Equal(t, 7.0, snap["http_requests{method=GET,status=200}"])
	assert.Equal(t, 3.0, snap["http_requests{method=POST,status=500}"])
	// Bare name key should not exist.
	_, ok := snap["http_requests"]
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
	// Three distinct keys: bare name + two label variants.
	assert.Contains(t, snap, "latency")
	assert.Contains(t, snap, "latency{route=/api}")
	assert.Contains(t, snap, "latency{route=/health}")
	assert.Equal(t, 1.5, snap["latency"])
	assert.Equal(t, 2.0, snap["latency{route=/api}"])
	assert.Equal(t, 3.0, snap["latency{route=/health}"])
}

func TestMetricKeyDeterministic(t *testing.T) {
	// No labels → bare name.
	assert.Equal(t, "gauge", metricKey("gauge", nil))
	assert.Equal(t, "gauge", metricKey("gauge", map[string]string{}))

	// Labels sorted by key for deterministic output.
	k1 := metricKey("req", map[string]string{"status": "200", "method": "GET"})
	k2 := metricKey("req", map[string]string{"method": "GET", "status": "200"})
	assert.Equal(t, "req{method=GET,status=200}", k1)
	assert.Equal(t, k1, k2, "label order should not affect key")
}
