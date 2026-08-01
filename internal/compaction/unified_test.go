package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// largeToolItems returns a conversation dominated by oversized tool results so
// that only the truncating fallback can fit a small budget.
func largeToolItems() []TurnItem {
	items := []TurnItem{
		{Role: RoleUser, Content: "what is in the file?"},
		{Role: RoleAssistant, Content: "let me read it"},
	}
	for i := 0; i < 30; i++ {
		items = append(items, TurnItem{
			Role:       RoleTool,
			ToolName:   "read",
			ToolResult: strings.Repeat("x", 200),
		})
	}
	return items
}

func TestUnifiedCompactorMicroFirst(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()
	items := largeToolItems()

	// Micro succeeds on its own (large tool results are placeholdered).
	uni := NewUnifiedCompactor(WithTriggerReason("threshold"))
	out, err := uni.Compact(ctx, items, 300, est)
	require.NoError(t, err)
	assert.Equal(t, StrategyMicro, uni.LastStrategy())
	assert.LessOrEqual(t, estimateTokens(out, est), 300)

	// A single compaction span is emitted for the routing decision.
	assertSpanEventually(t, exp, "compaction")
}

func TestUnifiedCompactorEscalatesMicroToSummary(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()
	items := largeToolItems()

	// Micro always escalates; summary produces a valid small result.
	micro := NewRecordingCompactor("micro").WithError(ErrRequiresTruncating)
	summary := NewRecordingCompactor("summary").WithResult(
		[]TurnItem{{Role: RoleSystem, Content: "summary", IsCompaction: true}},
	)
	truncating := NewRecordingCompactor("truncating").WithError(assert.AnError)

	uni := NewUnifiedCompactor(
		WithMicro(micro),
		WithSummary(summary),
		WithTruncating(truncating),
	)
	out, err := uni.Compact(ctx, items, 100, est)
	require.NoError(t, err)
	assert.Equal(t, StrategySummary, uni.LastStrategy())
	assert.Equal(t, []string{"micro"}, micro.Called(), "micro was tried first and escalated")
	assert.Equal(t, []string{"summary"}, summary.Called(), "summary won after micro failed")
	assert.Empty(t, truncating.Called(), "truncating was never reached")
	assert.Len(t, out, 1)

	assertSpanEventually(t, exp, "compaction")
}

func TestUnifiedCompactorEscalatesToTruncatingWhenNoSummarizer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()
	items := largeToolItems()

	// No summary configured: router skips straight to truncating.
	micro := NewRecordingCompactor("micro").WithError(ErrRequiresTruncating)
	truncating := NewRecordingCompactor("truncating").WithResult(
		[]TurnItem{{Role: RoleSystem, Content: "sys"}},
	)

	uni := NewUnifiedCompactor(
		WithMicro(micro),
		WithTruncating(truncating),
	)
	out, err := uni.Compact(ctx, items, 100, est)
	require.NoError(t, err)
	assert.Equal(t, StrategyTruncating, uni.LastStrategy())
	assert.Equal(t, []string{"micro"}, micro.Called(), "micro escalates")
	assert.Equal(t, []string{"truncating"}, truncating.Called(), "router hops straight to truncating without a summary")
	assert.Len(t, out, 1)

	assertSpanEventually(t, exp, "compaction")
}

func TestUnifiedCompactorAlwaysWithinBudgetWithRealCompactors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	items := largeToolItems()

	// Real micro + no summary + real truncating: the fallback guarantees the
	// result never exceeds the tiny budget.
	uni := NewUnifiedCompactor()
	out, err := uni.Compact(ctx, items, 40, est)
	require.NoError(t, err)
	assert.Equal(t, StrategyTruncating, uni.LastStrategy())
	assert.LessOrEqual(t, estimateTokens(out, est), 40)

	// All system entries survive truncation.
	for _, it := range out {
		assert.NotEqual(t, RoleTool, it.Role, "tools older than budget are dropped")
	}
}

func TestUnifiedCompactorEmptyInput(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	uni := NewUnifiedCompactor()
	out, err := uni.Compact(ctx, nil, 100, est)
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Equal(t, StrategyMicro, uni.LastStrategy())
}

func TestUnifiedCompactorCompileGuard(t *testing.T) {
	var _ Compactor = (*UnifiedCompactor)(nil)
	assert.NotNil(t, NewUnifiedCompactor())
	assert.Equal(t, StrategyNone, NewUnifiedCompactor().LastStrategy())
}
