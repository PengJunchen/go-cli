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
	tm := NewDefaultTelemetry()

	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "tokens", Value: 10}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "tokens", Value: 5}))
	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "calls", Value: 1, Labels: map[string]string{"tool": "edit"}}))

	snap := tm.Snapshot()
	assert.Equal(t, 15.0, snap["tokens"])
	assert.Equal(t, 1.0, snap["calls"])
}

func TestTelemetryRecordEmptyNameIgnored(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := context.Background()
	tm := NewDefaultTelemetry()

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
	tm := NewDefaultTelemetry()

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
	tm := NewDefaultTelemetry()

	require.NoError(t, tm.Record(ctx, TelemetryMetric{Name: "x", Value: 1}))
	snap := tm.Snapshot()
	assert.Equal(t, 1.0, snap["x"])
}
