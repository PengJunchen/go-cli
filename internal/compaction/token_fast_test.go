package compaction

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFastTokenEstimator(t *testing.T) {
	est := NewFastTokenEstimator()

	// ASCII text: len(text)/4.
	n, err := est.Estimate("hello world") // 11 bytes, all ASCII
	require.NoError(t, err)
	assert.Equal(t, 2, n, "11 ASCII chars / 4 = 2")

	// CJK text: rune_count * 1.5.
	n, err = est.Estimate("你好世界") // 4 CJK runes
	require.NoError(t, err)
	assert.Equal(t, 6, n, "round(4 runes * 1.5) = 6")

	// Mixed text with enough CJK to trigger CJK formula.
	n, err = est.Estimate("你好abc") // 2 CJK + 3 ASCII = 5 runes, 9 bytes
	require.NoError(t, err)
	// cjk=2, runeCount=5, 2 > 5/3=1 -> CJK formula: round(5*1.5)=8
	assert.Equal(t, 8, n, "round(5 runes * 1.5) = 8")
}

func TestCompositeTokenEstimator_ShortText(t *testing.T) {
	comp := NewCompositeTokenEstimator(10000)
	text := "你好世界" // 12 bytes < 10000 threshold
	n, err := comp.Estimate(text)
	require.NoError(t, err)
	// Short text uses precise UnicodeTokenEstimator: 4 CJK * 2 = 8.
	assert.Equal(t, 8, n, "short text should use precise estimator")
}

func TestCompositeTokenEstimator_LongText(t *testing.T) {
	comp := NewCompositeTokenEstimator(10000)
	// 20000 ASCII bytes >= 10000 threshold -> uses fast estimator.
	text := strings.Repeat("a", 20000)
	n, err := comp.Estimate(text)
	require.NoError(t, err)
	// Fast estimator: 20000 / 4 = 5000.
	assert.Equal(t, 5000, n, "long text should use fast estimator")
}

func TestCompositeTokenEstimator_Performance(t *testing.T) {
	comp := NewCompositeTokenEstimator(10000)
	text := strings.Repeat("a", 100000) // 100K characters
	start := time.Now()
	n, err := comp.Estimate(text)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Equal(t, 25000, n, "100000 / 4 = 25000")
	assert.Less(t, elapsed, time.Millisecond, "100K chars should estimate in < 1ms, took %v", elapsed)
}
