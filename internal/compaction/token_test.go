package compaction

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestHeuristicTokenEstimatorCharsOverFour(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	est := NewHeuristicTokenEstimator()

	n, err := est.Estimate("abcdefgh") // 8 chars / 4
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestHeuristicTokenEstimatorEmpty(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	n, err := est.Estimate("")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestHeuristicTokenEstimatorTruncates(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	n, err := est.Estimate("hello") // 5 chars / 4 = 1
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestHeuristicTokenEstimatorNeverErrors(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	for _, text := range []string{"", " ", "こんにちは世界", "a longer english sentence"} {
		_, err := est.Estimate(text)
		assert.NoError(t, err, "estimate should never error for %q", text)
	}
}

func TestHeuristicTokenEstimatorCompileGuard(t *testing.T) {
	var _ TokenEstimator = (*HeuristicTokenEstimator)(nil)
	assert.NotNil(t, NewHeuristicTokenEstimator())
}
