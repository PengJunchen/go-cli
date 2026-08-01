package compaction

import (
	"context"
	"strings"
	"sync"
	"testing"

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
