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

func TestHeuristicEstimateASCII(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	// Pure ASCII alphanumerics: ~0.25 tokens per char.
	n, err := est.Estimate("abcdefgh") // 8 ASCII letters
	require.NoError(t, err)
	assert.Equal(t, 2, n) // 8 * 0.25 = 2.0
}

func TestHeuristicEstimateCJK(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	// CJK runes count as 2 tokens each.
	n, err := est.Estimate("你好世界") // 4 CJK chars
	require.NoError(t, err)
	assert.Equal(t, 8, n) // 4 * 2 = 8
}

func TestHeuristicEstimateMixed(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	// 3 ASCII letters (0.25 each = 0.75) + 2 CJK (2 each = 4) = 4.75 -> 5
	n, err := est.Estimate("abc你好")
	require.NoError(t, err)
	assert.Equal(t, 5, n)
}
