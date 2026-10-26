package compaction

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// fakeSummarizer implements compaction.Summarizer, returning a canned summary
// and recording every conversation passed to it.
type fakeSummarizer struct {
	mu      sync.Mutex
	summary string
	err     error
	got     []string
}

// Compile-time assertion that the fake satisfies Summarizer.
var _ Summarizer = (*fakeSummarizer)(nil)

func (f *fakeSummarizer) Summarize(_ context.Context, conversation string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, conversation)
	return f.summary, f.err
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
		{Role: RoleAssistant, Content: "reply", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: strings.Repeat("t", 400)},
		{Role: RoleUser, Content: "keep me"},
		{Role: RoleAssistant, Content: "ok"},
	}

	cut, err := compactor.findCutPoint(items, 60, est)
	require.NoError(t, err)

	// The cut must be a boundary between complete item groups.
	_, groups, err := groupTurnItems(items)
	require.NoError(t, err)
	isBoundary := cut == len(items)
	for _, group := range groups {
		if cut == group.start {
			isBoundary = true
			break
		}
	}
	require.True(t, isBoundary, "cut %d must be between complete item groups", cut)

	// The kept suffix plus a placeholder must fit the budget.
	placeholder := estimateTokens([]TurnItem{{Role: RoleUser, Content: summaryPlaceholder, IsCompaction: true}}, est)
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
		{Role: RoleAssistant, Content: strings.Repeat("a", 300), ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: strings.Repeat("t", 400)},
		{Role: RoleUser, Content: "recent question"},
		{Role: RoleAssistant, Content: "recent answer"},
	}

	compactor := NewSummaryCompactor(sum)
	outResult, err := compactor.Compact(ctx, items, 50, est)
	out := outResult.Items
	require.NoError(t, err)

	// The mandatory system prompt precedes the derived summary.
	require.NotEmpty(t, out)
	require.GreaterOrEqual(t, len(out), 4)
	assert.Equal(t, RoleSystem, out[0].Role)
	assert.True(t, out[1].IsCompaction)
	assert.Equal(t, RoleUser, out[1].Role)
	assert.Equal(t, "canned summary of older turns", out[1].Content)
	assert.Empty(t, out[1].ToolCalls)
	assert.Empty(t, out[1].ToolCallID)

	// The recent turns are preserved after the summary entry.
	assert.Equal(t, "recent question", out[len(out)-2].Content)
	assert.Equal(t, "recent answer", out[len(out)-1].Content)

	// Total must fit budget.
	assert.LessOrEqual(t, estimateTokens(out, est), 50)

	assertSpanEventually(t, exp, "compaction.summary")
}

func TestSummaryCompactorPreservesMandatorySystemEntriesAtTheirCutPositions(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	est := runeCountEstimator{}
	summarizer := &fakeSummarizer{summary: "derived summary"}

	systemBefore := TurnItem{
		ID: "system-before", ParentID: "root", Role: RoleSystem,
		Content: "system-one", EstimatedTokens: len("system-one"),
		FinishReason: "configured", InputKind: "policy",
		Usage: &llm.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12},
	}
	systemAfter := TurnItem{
		ID: "system-after", ParentID: "root", Role: RoleSystem,
		Content:      "system-two",
		FinishReason: "configured", InputKind: "policy",
		Usage: &llm.Usage{InputTokens: 8, OutputTokens: 1, TotalTokens: 9},
	}
	items := []TurnItem{
		systemBefore,
		{Role: RoleUser, Content: strings.Repeat("old ", 100)},
		systemAfter,
		{Role: RoleUser, Content: "recent"},
	}
	original := append([]TurnItem(nil), items...)

	outResult, err := NewSummaryCompactor(summarizer).Compact(ctx, items, 140, est)

	out := outResult.Items
	require.NoError(t, err)
	require.Len(t, out, 4)
	assert.Equal(t, systemBefore, out[0], "the original system item and metadata must survive unchanged")
	assert.True(t, out[1].IsCompaction)
	assert.Equal(t, RoleUser, out[1].Role, "derived summaries remain untrusted user context")
	assert.Equal(t, "derived summary", out[1].Content)
	assert.Equal(t, systemAfter, out[2], "system items after the cut retain their original position and metadata")
	assert.Equal(t, "recent", out[3].Content)
	assert.LessOrEqual(t, estimateTokens(out, est), 140)
	assert.Equal(t, original, items, "compaction must not normalize the caller's input in place")

	prompts := summarizer.conversations()
	require.Len(t, prompts, 1)
	assert.NotContains(t, prompts[0], "system-one")
	assert.NotContains(t, prompts[0], "system-two")
}

func TestSummaryCompactorRejectsMandatorySystemOverBudgetBeforeSummarizing(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	summarizer := &fakeSummarizer{summary: "must not run"}
	items := []TurnItem{
		{Role: RoleSystem, Content: "mandatory-system-prompt", EstimatedTokens: 1},
		{Role: RoleUser, Content: strings.Repeat("old ", 100)},
	}

	_, err := NewSummaryCompactor(summarizer).Compact(ctx, items, 10, runeCountEstimator{})
	require.ErrorIs(t, err, ErrMandatorySystemPromptOverBudget)
	assert.Empty(t, summarizer.conversations(), "over-budget mandatory instructions must fail before summary dispatch")
}

func TestSummaryCompactorBudgetIncludesProjectedSummaryWrapper(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	summarizer := &fakeSummarizer{summary: "summary"}
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("old ", 100)},
		{Role: RoleUser, Content: "keep"},
	}

	_, err := NewSummaryCompactor(summarizer).Compact(ctx, items, 20, runeCountEstimator{})
	require.ErrorIs(t, err, ErrRequiresTruncating,
		"the provider-visible untrusted-summary framing must count against the conversation budget")
	assert.NotEmpty(t, summarizer.conversations())
}

func TestSummaryCompactorReservesBoundedSummaryForDurableHistory(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	const revision = "revision-summary-headroom"
	const branch = "branch-main"
	const runID = "run-summary-headroom"
	priorSource := func(ordinal uint64, entryID string) llm.RuntimeMetadata {
		return llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: revision, BranchID: branch,
			SourceEntryID: entryID, PriorOrdinal: ordinal,
		}}
	}
	currentContent := "current123"
	current := TurnItem{
		Role: RoleUser, Content: currentContent,
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourceRunTranscript, RunID: runID, PriorSourceCount: 3, TranscriptIndex: 0,
		}},
	}
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("a", 46), Runtime: priorSource(0, "entry-old-a")},
		{Role: RoleUser, Content: strings.Repeat("b", 46), Runtime: priorSource(1, "entry-old-b")},
		{Role: RoleUser, Content: "c", Runtime: priorSource(2, "entry-old-c")},
		current,
	}
	const summaryTokens = 20
	const budget = 112
	summarizer := &fakeSummarizer{summary: strings.Repeat("s", summaryTokens)}
	compactor := NewSummaryCompactor(summarizer, WithMaxSummaryTokens(summaryTokens))
	estimator := runeCountEstimator{}

	// With the rune-count estimator, the bounded summary plus its provider
	// framing costs 4 role runes + 74 untrusted-prefix runes + 20 summary runes.
	// The unchanged current input costs 4 role runes + 10 content runes, so the
	// 112-token budget fits them exactly. The old placeholder reservation also
	// left the 5-token third prior item in the suffix, which made the real
	// 20-token summary result exceed this budget.
	result, err := compactor.Compact(ctx, items, budget, estimator)
	require.NoError(t, err)
	require.Equal(t, StrategySummary, result.Strategy)
	require.NotNil(t, result.NewProjection)
	assert.Equal(t, strings.Repeat("s", summaryTokens), result.NewProjection.Summary)
	assert.Equal(t, llm.SourceThrough{PriorHistoryCount: 3}, result.NewProjection.Through,
		"the larger cut summarizes only the complete prior prefix")
	assert.LessOrEqual(t, estimateTokens(result.Items, estimator), budget)
	require.Len(t, result.Items, 2)
	assert.True(t, result.Items[0].IsCompaction)
	resultCurrent := result.Items[1]
	resultCurrent.EstimatedTokens = 0
	assert.Equal(t, current, resultCurrent, "the current source-backed input must remain exact and outside the summary")
	assert.Equal(t, current, items[3], "compaction must not mutate caller-owned current input")
}

func TestUnifiedCompactorFallsBackAfterTransientSummaryError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	summarizer := &fakeSummarizer{err: errors.New("temporary summary failure")}
	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: strings.Repeat("old ", 300)},
		{Role: RoleUser, Content: "keep"},
	}
	uni := NewUnifiedCompactor(WithSummary(NewSummaryCompactor(summarizer)))

	outResult, err := uni.Compact(ctx, items, 20, est)

	out := outResult.Items
	require.NoError(t, err)
	assert.Equal(t, StrategyTruncating, uni.LastStrategy())
	assert.LessOrEqual(t, estimateTokens(out, est), 20)
	require.NotEmpty(t, out)
	assert.Equal(t, RoleSystem, out[0].Role)
	assert.Contains(t, summarizer.conversations()[0], "old")
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
	outResult, err := compactor.Compact(ctx, items, 35, est)
	out := outResult.Items
	require.NoError(t, err)
	require.Len(t, out, 3)
	assert.Equal(t, RoleSystem, out[0].Role)
	assert.Equal(t, summaryPlaceholder, out[1].Content)
	assert.True(t, out[1].IsCompaction)
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
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-write", Name: "write"}}},
		{Role: RoleTool, ToolCallID: "call-write", ToolName: "write", ToolResult: strings.Repeat("w", 400)},
		{Role: RoleUser, Content: "latest turn"},
	}

	compactor := NewSummaryCompactor(sum)
	outResult, err := compactor.Compact(ctx, items, 80, est)
	out := outResult.Items
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

func TestSummaryPromptPreservesChronologyAndAllToolReceipts(t *testing.T) {
	items := []TurnItem{
		{Role: RoleUser, Content: "first question"},
		{Role: RoleAssistant, Content: "I will inspect and search"},
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "read-1", Name: "read", Args: map[string]any{"path": "/private/canary"}}}},
		{Role: RoleTool, ToolCallID: "read-1", ToolName: "read", ReceiptOutcome: "success", ToolResult: "file output"},
		{Role: RoleUser, Content: "follow-up after read"},
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "search-1", Name: "web_search", Args: map[string]any{"query": "private canary"}}}},
		{Role: RoleTool, ToolCallID: "search-1", ToolName: "web_search", ReceiptOutcome: "error", ToolResult: "search failed: unavailable"},
		{Role: RoleAssistant, Content: "final old answer"},
	}

	prompt, err := NewSummaryCompactor(nil).buildSummaryPrompt(items, NewHeuristicTokenEstimator())
	require.NoError(t, err)
	positions := []int{
		strings.Index(prompt, "first question"),
		strings.Index(prompt, "I will inspect and search"),
		strings.Index(prompt, "file output"),
		strings.Index(prompt, "follow-up after read"),
		strings.Index(prompt, "search failed: unavailable"),
		strings.Index(prompt, "final old answer"),
	}
	for _, position := range positions {
		require.GreaterOrEqual(t, position, 0)
	}
	for i := 1; i < len(positions); i++ {
		assert.Less(t, positions[i-1], positions[i], "summary prompt must preserve transcript chronology")
	}
	assert.Contains(t, prompt, "web_search")
	assert.Contains(t, prompt, "error")
	assert.NotContains(t, prompt, "/private/canary", "raw tool arguments must not enter the summary prompt")
	assert.NotContains(t, prompt, "private canary", "raw tool arguments must not enter the summary prompt")
}

func TestSummaryPromptBoundsItemsAndWholePrompt(t *testing.T) {
	items := []TurnItem{{
		Role:           RoleTool,
		ToolName:       "shell",
		ReceiptOutcome: "error",
		ToolResult:     "HEAD-CANARY-" + strings.Repeat("x", 50000) + "-TAIL-CANARY",
	}}
	prompt, err := NewSummaryCompactor(nil).buildSummaryPrompt(items, runeCountEstimator{})
	require.NoError(t, err)
	tokens, err := (runeCountEstimator{}).Estimate(prompt)
	require.NoError(t, err)
	assert.LessOrEqual(t, tokens, 6000, "entire summary request including framing must fit its total budget")
	assert.LessOrEqual(t, strings.Count(prompt, "x"), 2048, "one rendered receipt must fit its per-item budget")
	assert.Contains(t, prompt, "[truncated]")
	assert.Contains(t, prompt, "HEAD-CANARY")
	assert.Contains(t, prompt, "TAIL-CANARY")
}

func TestSummaryPromptTotalBudgetTruncatesInHeadTailOrder(t *testing.T) {
	items := make([]TurnItem, 8)
	for i := range items {
		items[i] = TurnItem{
			Role:           RoleTool,
			ToolName:       "tool",
			ReceiptOutcome: "success",
			ToolResult:     "BEGIN-" + string(rune('0'+i)) + strings.Repeat("x", 3000) + "-END-" + string(rune('0'+i)),
		}
	}
	prompt, err := NewSummaryCompactor(nil).buildSummaryPrompt(items, NewHeuristicTokenEstimator())
	require.NoError(t, err)
	tokens, err := NewHeuristicTokenEstimator().Estimate(prompt)
	require.NoError(t, err)
	assert.LessOrEqual(t, tokens, 6000)
	assert.Contains(t, prompt, "BEGIN-0")
	assert.Contains(t, prompt, "-END-7")
	assert.Contains(t, prompt, "[truncated]")
}

func TestSummaryPromptItemLimitUsesProvidedEstimator(t *testing.T) {
	items := []TurnItem{{Role: RoleUser, Content: "HEAD-" + strings.Repeat("x", 200) + "-TAIL"}}
	compactor := NewSummaryCompactor(nil, WithMaxSummaryItemTokens(32))
	prompt, err := compactor.buildSummaryPrompt(items, runeCountEstimator{})
	require.NoError(t, err)
	itemText := strings.TrimPrefix(prompt, summaryPromptInstruction)
	assert.LessOrEqual(t, utf8.RuneCountInString(itemText), 32)
	assert.Contains(t, itemText, "HEAD")
	assert.Contains(t, itemText, "-TAIL")
	assert.Contains(t, itemText, "[truncated]")
}

func TestSummaryCompactorDoesNotCallSummarizerWhenPromptBudgetIsTooSmall(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newTracedCtx(t)
	summarizer := &fakeSummarizer{summary: "should not be returned"}
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("old ", 100)},
		{Role: RoleUser, Content: "keep"},
	}
	compactor := NewSummaryCompactor(summarizer, WithMaxSummaryPromptTokens(1))

	_, err := compactor.Compact(ctx, items, 50, runeCountEstimator{})
	require.ErrorIs(t, err, ErrSummaryPromptBudgetTooSmall)
	assert.Empty(t, summarizer.conversations(), "an unusably small prompt budget must fail before dispatch")
}

func TestSummaryPromptNonPositiveLimitsUseBoundedDefaults(t *testing.T) {
	compactor := NewSummaryCompactor(nil, WithMaxSummaryPromptTokens(0), WithMaxSummaryItemTokens(-1))
	assert.Equal(t, defaultMaxSummaryPromptTokens, compactor.maxSummaryPromptTokens)
	assert.Equal(t, defaultMaxSummaryItemTokens, compactor.maxSummaryItemTokens)
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

// TestSummarySmallModel verifies that when a small-model-backed Summarizer is
// injected, the UnifiedCompactor routes to the summary strategy (AC-5) and the
// summarizer is actually invoked. It also verifies that without a summarizer
// the router falls back to truncating and never selects summary (AC-6).
func TestSummarySmallModel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	// Items with large user/assistant messages and no tool results. Micro
	// cannot shrink these (it only placeholders tool results), so the
	// UnifiedCompactor must escalate to summary or truncating.
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("u", 500)},
		{Role: RoleAssistant, Content: strings.Repeat("a", 500)},
		{Role: RoleUser, Content: strings.Repeat("u", 500)},
		{Role: RoleAssistant, Content: strings.Repeat("a", 500)},
		{Role: RoleUser, Content: "recent question"},
		{Role: RoleAssistant, Content: "recent answer"},
	}

	// AC-5: When a small-model-backed summarizer is configured, compaction
	// routes to the summary strategy and the summarizer is invoked.
	smallSummarizer := &fakeSummarizer{summary: "small model summary"}
	summaryCompactor := NewSummaryCompactor(smallSummarizer)
	uni := NewUnifiedCompactor(WithSummary(summaryCompactor))

	outResult, err := uni.Compact(ctx, items, 30, est)

	out := outResult.Items
	require.NoError(t, err)
	assert.Equal(t, StrategySummary, uni.LastStrategy(),
		"should use summary strategy when small model is configured")

	// The small model summarizer was actually called.
	require.NotEmpty(t, smallSummarizer.conversations(),
		"small model summarizer must be invoked")

	// The summary entry is present at the head.
	require.NotEmpty(t, out)
	assert.True(t, out[0].IsCompaction)
	assert.Equal(t, "small model summary", out[0].Content)

	// AC-6: Without a small-model summarizer, compaction falls back to
	// truncating and never invokes summary.
	uniNoSmall := NewUnifiedCompactor()
	out2Result, err2 := uniNoSmall.Compact(ctx, items, 30, est)
	out2 := out2Result.Items
	require.NoError(t, err2)
	assert.NotEqual(t, StrategySummary, uniNoSmall.LastStrategy(),
		"should not use summary when no small model configured")
	assert.LessOrEqual(t, estimateTokens(out2, est), 30)
}
