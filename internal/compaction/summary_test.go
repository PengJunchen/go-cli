package compaction

import (
	"context"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// fakeSummarizer implements compaction.Summarizer, returning a canned summary
// and recording every conversation passed to it.
type fakeSummarizer struct {
	mu      sync.Mutex
	summary string
	got     []string
}

// Compile-time assertion that the fake satisfies Summarizer.
var _ Summarizer = (*fakeSummarizer)(nil)

func (f *fakeSummarizer) Summarize(_ context.Context, conversation string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, conversation)
	return f.summary, nil
}

func (f *fakeSummarizer) conversations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.got))
	copy(out, f.got)
	return out
}

func TestFindCutPointAtMessageBoundary(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	compactor := NewSummaryCompactor(nil)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: strings.Repeat("u", 200)},
		{Role: RoleAssistant, Content: "reply"},
		{Role: RoleTool, ToolName: "read", ToolResult: strings.Repeat("t", 400)},
		{Role: RoleUser, Content: "keep me"},
		{Role: RoleAssistant, Content: "ok"},
	}

	cut := compactor.findCutPoint(items, 60, est)

	// The cut must be a whole-message boundary (not before a tool result).
	require.True(t, cut == len(items) || items[cut].Role != RoleTool,
		"cut %d must not split before a tool result", cut)

	// The kept suffix plus a placeholder must fit the budget.
	placeholder := estimateTokens([]TurnItem{{Content: summaryPlaceholder}}, est)
	keepTokens := estimateTokens(items[cut:], est)
	assert.LessOrEqual(t, placeholder+keepTokens, 60)
}

func TestSummaryCompactorProducesPlaceholderWithinBudget(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()
	sum := &fakeSummarizer{summary: "canned summary of older turns"}

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: strings.Repeat("u", 300)},
		{Role: RoleAssistant, Content: strings.Repeat("a", 300)},
		{Role: RoleTool, ToolName: "read", ToolResult: strings.Repeat("t", 400)},
		{Role: RoleUser, Content: "recent question"},
		{Role: RoleAssistant, Content: "recent answer"},
	}

	compactor := NewSummaryCompactor(sum)
	out, err := compactor.Compact(ctx, items, 30, est)
	require.NoError(t, err)

	// The first (summary) entry is a compaction placeholder.
	require.NotEmpty(t, out)
	assert.True(t, out[0].IsCompaction)
	assert.Equal(t, RoleSystem, out[0].Role)
	assert.Equal(t, "canned summary of older turns", out[0].Content)

	// The recent turns are preserved after the summary entry.
	assert.Equal(t, "recent question", out[len(out)-2].Content)
	assert.Equal(t, "recent answer", out[len(out)-1].Content)

	// Total must fit budget.
	assert.LessOrEqual(t, estimateTokens(out, est), 30)

	assertSpanEventually(t, exp, "compaction.summary")
}

func TestSummaryCompactorUsesPlaceholderOnEmptySummary(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	sum := &fakeSummarizer{summary: ""}

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: strings.Repeat("u", 500)},
		{Role: RoleUser, Content: "keep"},
	}

	compactor := NewSummaryCompactor(sum)
	out, err := compactor.Compact(ctx, items, 20, est)
	require.NoError(t, err)
	assert.Equal(t, summaryPlaceholder, out[0].Content)
	assert.True(t, out[0].IsCompaction)
}

func TestSummaryCompactorIncrementalOnlySummarizesUncompactedRegion(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	sum := &fakeSummarizer{summary: "incremental summary"}

	// A prior compaction entry already exists at the head of the region.
	items := []TurnItem{
		{Role: RoleSystem, Content: "[existing summary ...]", IsCompaction: true},
		{Role: RoleUser, Content: strings.Repeat("u", 300)},
		{Role: RoleTool, ToolName: "write", ToolResult: strings.Repeat("w", 400)},
		{Role: RoleUser, Content: "latest turn"},
	}

	compactor := NewSummaryCompactor(sum)
	out, err := compactor.Compact(ctx, items, 25, est)
	require.NoError(t, err)

	// Exactly one compaction entry remains: the freshly produced one.
	countCompaction := 0
	for _, it := range out {
		if it.IsCompaction {
			countCompaction++
		}
	}
	assert.Equal(t, 1, countCompaction)

	// The summarizer was asked to summarize a region and the existing summary
	// was folded into the prompt rather than dropped.
	got := sum.conversations()
	require.NotEmpty(t, got)
	assert.Contains(t, got[0], "[existing summary ...]")
}

func TestSummaryCompactorEscalatesWhenBudgetTooTight(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	sum := &fakeSummarizer{summary: ""}

	// A single large item and a budget so small that even the summary
	// placeholder alone exceeds it.
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("x", 400)},
	}

	compactor := NewSummaryCompactor(sum)
	_, err := compactor.Compact(ctx, items, 1, est)
	require.ErrorIs(t, err, ErrRequiresTruncating)
}

func TestSummaryCompactorCompileGuard(t *testing.T) {
	var _ Compactor = (*SummaryCompactor)(nil)
	assert.NotNil(t, NewSummaryCompactor(nil))
}

func TestNoopSummarizerFailsLoudly(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("u", 200)},
		{Role: RoleUser, Content: "recent"},
	}

	compactor := NewSummaryCompactor(nil) // fallback summarizer
	_, err := compactor.Compact(ctx, items, 30, est)
	require.ErrorIs(t, err, errNoSummarizer)
}

// runeCountEstimator returns the rune count as the token count, letting tests
// distinguish estimator-driven truncation from len()/4 truncation.
type runeCountEstimator struct{}

func (runeCountEstimator) Estimate(text string) (int, error) {
	return utf8.RuneCountInString(text), nil
}

func TestClampSummaryUsesEstimator(t *testing.T) {
	est := runeCountEstimator{} // 1 token per rune
	compactor := NewSummaryCompactor(nil, WithMaxSummaryTokens(10))

	summary := strings.Repeat("x", 100) // 100 runes = 100 tokens
	clamped := compactor.clampSummary(summary, est)

	// With the rune-count estimator the budget of 10 means 10 runes.
	// If len()/4 were used instead, the result would be 40 chars.
	assert.Equal(t, 10, utf8.RuneCountInString(clamped))
	n, _ := est.Estimate(clamped)
	assert.LessOrEqual(t, n, 10)
}

func TestClampSummaryCJKAccuracy(t *testing.T) {
	est := NewHeuristicTokenEstimator() // unicode-aware: CJK = 2 tokens
	compactor := NewSummaryCompactor(nil, WithMaxSummaryTokens(50))

	summary := strings.Repeat("你", 100) // 100 CJK chars = 200 tokens
	clamped := compactor.clampSummary(summary, est)

	// Budget 50 / 2 tokens-per-CJK = 25 runes. The old len()/4 approach would
	// compute 300 bytes / 4 = 75 tokens and truncate at byte 200 (66.7 CJK
	// chars, splitting a multi-byte rune). The estimator-based approach
	// truncates cleanly at 25 runes.
	assert.Equal(t, 25, utf8.RuneCountInString(clamped))
	n, _ := est.Estimate(clamped)
	assert.LessOrEqual(t, n, 50)
}
