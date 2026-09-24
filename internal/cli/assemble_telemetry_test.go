package cli

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/production"
)

func TestRecordToolTelemetryUsesBoundedToolClasses(t *testing.T) {
	telemetry, ok := production.NewDefaultTelemetry().(*production.DefaultTelemetry)
	require.True(t, ok)
	ctx := context.Background()

	recordToolTelemetry(ctx, telemetry, "read", 17*time.Millisecond, true)
	recordToolTelemetry(ctx, telemetry, "grep", 23*time.Millisecond, true)
	recordToolTelemetry(ctx, telemetry, "user-private-tool-9f3a", 7*time.Millisecond, false)

	snapshot := telemetry.Snapshot()
	counts := make([]float64, 0, 2)
	durations := make([]float64, 0, 2)
	for key, value := range snapshot {
		if strings.Contains(key, "tool.call.count") {
			counts = append(counts, value)
		}
		if strings.Contains(key, "tool.call.duration_ms") {
			durations = append(durations, value)
		}
		assert.NotContains(t, key, "user-private-tool-9f3a", "dynamic tool names must not become metric labels")
	}
	sort.Float64s(counts)
	sort.Float64s(durations)
	assert.Equal(t, []float64{1, 2}, counts, "built-in read tools should aggregate; custom names should share other")
	assert.Equal(t, []float64{7, 40}, durations)
}
