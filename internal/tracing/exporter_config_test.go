package tracing

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeBatchSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want int
	}{
		{name: "zero_clamped_to_one", n: 0, want: 1},
		{name: "negative_clamped_to_one", n: -5, want: 1},
		{name: "one_unchanged", n: 1, want: 1},
		{name: "positive_unchanged", n: 100, want: 100},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, normalizeBatchSize(tc.n))
		})
	}
}

func TestNormalizeInterval(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		d    time.Duration
		want time.Duration
	}{
		{name: "zero_defaults", d: 0, want: exporterDefaultFlushInterval},
		{name: "negative_defaults", d: -1 * time.Second, want: exporterDefaultFlushInterval},
		{name: "subsecond_preserved", d: 500 * time.Millisecond, want: 500 * time.Millisecond},
		{name: "positive_preserved", d: 5 * time.Second, want: 5 * time.Second},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, normalizeInterval(tc.d))
		})
	}
}

func TestExporterDefaultConstants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, time.Second, exporterDefaultFlushInterval, "exporterDefaultFlushInterval should be 1s")
	assert.Equal(t, 5*time.Second, exporterDefaultHTTPTimeout, "exporterDefaultHTTPTimeout should be 5s")
}
