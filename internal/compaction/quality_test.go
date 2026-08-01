package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestQualityMetricsPartialCompression(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()

	// 3 items in, 2 kept.
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("u", 400)},      // ~100 tokens
		{Role: RoleUser, Content: strings.Repeat("v", 200)},      // ~50 tokens
		{Role: RoleAssistant, Content: strings.Repeat("w", 100)}, // ~25 tokens
	}
	compressed := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("u", 400)},                      // ~100 tokens
		{Role: RoleSystem, Content: strings.Repeat("s", 80), IsCompaction: true}, // ~20 tokens
	}

	eval := NewDefaultQualityEvaluator(est, WithQualityStrategy(StrategySummary))
	metrics, err := eval.Evaluate(ctx, items, compressed)
	require.NoError(t, err)

	// Coverage: 2 kept / 3 total.
	assert.InDelta(t, 2.0/3.0, metrics.Coverage, 1e-9)
	// InfoLoss: (175-120)/175.
	before := estimateTokens(items, est)     // 175
	after := estimateTokens(compressed, est) // 120
	assert.InDelta(t, float64(before-after)/float64(before), metrics.InfoLoss, 1e-9)
	// CompressionRatio: 175/120.
	assert.InDelta(t, float64(before)/float64(after), metrics.CompressionRatio, 1e-9)
	assert.Equal(t, StrategySummary, metrics.Strategy)

	assertSpanEventually(t, exp, "compaction.quality")
}

func TestQualityMetricsEmptyInputNoPanic(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	eval := NewDefaultQualityEvaluator(est)
	metrics, err := eval.Evaluate(ctx, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 0.0, metrics.Coverage)
	assert.Equal(t, 0.0, metrics.InfoLoss)
	assert.Equal(t, 1.0, metrics.CompressionRatio)
	assert.Equal(t, StrategyNone, metrics.Strategy)
}

func TestQualityMetricsFullRemovalRatio(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{{Role: RoleUser, Content: strings.Repeat("u", 400)}}
	var compressed []TurnItem // nothing retained

	eval := NewDefaultQualityEvaluator(est)
	metrics, err := eval.Evaluate(ctx, items, compressed)
	require.NoError(t, err)

	// Everything removed: coverage 0, ratio 0 (avoided divide-by-zero/inf), loss 1.
	assert.Equal(t, 0.0, metrics.Coverage)
	assert.Equal(t, 0.0, metrics.CompressionRatio)
	assert.Equal(t, 1.0, metrics.InfoLoss)
}

func TestQualityMetricsUnchangedIdentity(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{{Role: RoleUser, Content: strings.Repeat("u", 400)}}
	compressed := []TurnItem{{Role: RoleUser, Content: strings.Repeat("u", 400)}}

	eval := NewDefaultQualityEvaluator(est)
	metrics, err := eval.Evaluate(ctx, items, compressed)
	require.NoError(t, err)

	// No loss, identity ratio.
	assert.Equal(t, 1.0, metrics.Coverage)
	assert.Equal(t, 0.0, metrics.InfoLoss)
	assert.Equal(t, 1.0, metrics.CompressionRatio)
}

func TestQualityEvaluatorCompileGuard(t *testing.T) {
	var _ QualityEvaluator = (*DefaultQualityEvaluator)(nil)
	assert.NotNil(t, NewDefaultQualityEvaluator(nil))
}
