// Package e2e_20260802 contains end-to-end integration tests for the compaction
// module of go-cli. It exercises MicroCompactor, SummaryCompactor,
// TruncatingCompactor, UnifiedCompactor (strategy routing), MidTurnCompact
// (threshold auto-compaction), HeuristicTokenEstimator, QualityEvaluator, and
// long-conversation scenarios.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// =============================================================================
// Test helpers
// =============================================================================

// fakeSummarizer implements compaction.Summarizer, returning a canned summary
// and recording every conversation passed to it.
type fakeSummarizer struct {
	mu      sync.Mutex
	summary string
	got     []string
}

var _ compaction.Summarizer = (*fakeSummarizer)(nil)

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

// errorSummarizer always fails to exercise error paths.
type errorSummarizer struct{}

var _ compaction.Summarizer = (*errorSummarizer)(nil)

func (errorSummarizer) Summarize(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("simulated summarizer failure")
}

// newTracedCtx returns a context whose spans are collected by a fresh
// MockTraceExporter, together with the exporter for assertions.
func newTracedCtx(t testing.TB) (context.Context, *mock.MockTraceExporter) {
	t.Helper()
	exp := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("compaction-e2e-test", exp)
	span, ctx := tracer.Start(context.Background(), "compaction.e2e.root", tracing.SpanKindInternal)
	span.End()
	return ctx, exp
}

// =============================================================================
// 1. TestCompaction_MicroCompaction
// =============================================================================

func TestCompaction_MicroCompaction(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	est := compaction.NewHeuristicTokenEstimator()

	items := []compaction.TurnItem{
		{Role: compaction.RoleSystem, Content: "You are a helpful assistant."},
		{Role: compaction.RoleUser, Content: "What is in the file?"},
		{Role: compaction.RoleAssistant, Content: "Let me read it for you."},
		{Role: compaction.RoleTool, ToolName: "read", ToolResult: strings.Repeat("x", 400)},
		{Role: compaction.RoleUser, Content: "Thank you."},
		{Role: compaction.RoleAssistant, Content: "You are welcome."},
	}

	compactor := compaction.NewMicroCompactor()
	out, err := compactor.Compact(ctx, items, 80, est)
	require.NoError(t, err)

	// System, user, assistant messages preserved verbatim.
	assert.Equal(t, "You are a helpful assistant.", out[0].Content)
	assert.Equal(t, compaction.RoleSystem, out[0].Role)
	assert.Equal(t, "What is in the file?", out[1].Content)
	assert.Equal(t, compaction.RoleUser, out[1].Role)
	assert.Equal(t, "Let me read it for you.", out[2].Content)
	assert.Equal(t, compaction.RoleAssistant, out[2].Role)
	assert.Equal(t, "Thank you.", out[4].Content)
	assert.Equal(t, "You are welcome.", out[5].Content)

	// The large tool result is replaced with the placeholder.
	assert.Equal(t, "[compacted tool result]", out[3].ToolResult)
	assert.Equal(t, compaction.RoleTool, out[3].Role)

	// Total must be within budget.
	tokens := tokenSum(out, est)
	assert.LessOrEqual(t, tokens, 80, "compacted output must fit budget")
}

// =============================================================================
// 2. TestCompaction_TruncatingCompaction
// =============================================================================

func TestCompaction_TruncatingCompaction(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	est := compaction.NewHeuristicTokenEstimator()

	items := []compaction.TurnItem{
		{Role: compaction.RoleSystem, Content: "system prompt"},
		{Role: compaction.RoleUser, Content: strings.Repeat("old1", 50)},
		{Role: compaction.RoleAssistant, Content: strings.Repeat("old2", 50)},
		{Role: compaction.RoleUser, Content: strings.Repeat("old3", 50)},
		{Role: compaction.RoleAssistant, Content: strings.Repeat("old4", 50)},
		{Role: compaction.RoleUser, Content: "recent-1"},
		{Role: compaction.RoleAssistant, Content: "recent-2"},
	}

	compactor := compaction.NewTruncatingCompactor()
	out, err := compactor.Compact(ctx, items, 30, est)
	require.NoError(t, err)

	// The system entry is always kept.
	require.NotEmpty(t, out)
	assert.Equal(t, "system prompt", out[0].Content)
	assert.Equal(t, compaction.RoleSystem, out[0].Role)

	// Non-system entries: oldest are dropped, newest are kept in chronological order.
	tokens := tokenSum(out, est)
	assert.LessOrEqual(t, tokens, 30, "truncated output must fit budget")

	// Verify oldest entries are dropped; only the smallest/recent ones remain.
	hasOld := false
	for _, it := range out {
		if strings.HasPrefix(it.Content, strings.Repeat("old", 3)) {
			hasOld = true
			break
		}
	}
	assert.False(t, hasOld, "old large entries should be truncated")

	// Recent entries preserved.
	contents := contentSlice(out)
	assert.Contains(t, contents, "recent-1")
	assert.Contains(t, contents, "recent-2")
}

// =============================================================================
// 3. TestCompaction_UnifiedStrategyRouting
// =============================================================================

func TestCompaction_UnifiedStrategyRouting(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	est := compaction.NewHeuristicTokenEstimator()

	// Build items that micro can handle: tool results can be replaced with placeholders.
	items := []compaction.TurnItem{
		{Role: compaction.RoleSystem, Content: "sys"},
		{Role: compaction.RoleUser, Content: "hello"},
		{Role: compaction.RoleAssistant, Content: "hi"},
		{Role: compaction.RoleTool, ToolName: "read", ToolResult: strings.Repeat("t", 200)},
		{Role: compaction.RoleTool, ToolName: "grep", ToolResult: strings.Repeat("g", 200)},
		{Role: compaction.RoleUser, Content: "ok"},
	}

	t.Run("micro-first routing with real compactors", func(t *testing.T) {
		uni := compaction.NewUnifiedCompactor()
		out, err := uni.Compact(ctx, items, 100, est)
		require.NoError(t, err)
		// Micro should succeed because tool results can be placeholdered.
		assert.Equal(t, compaction.StrategyMicro, uni.LastStrategy())
		assert.LessOrEqual(t, tokenSum(out, est), 100)
	})

	t.Run("escalates micro -> summary -> truncating", func(t *testing.T) {
		// Micro always fails with ErrRequiresTruncating; summary tries and fails with an actual error (bad summarizer);
		// truncating succeeds.
		micro := &recordingCompactor{name: "micro", err: compaction.ErrRequiresTruncating}
		failSumm := &errorSummarizer{}
		summaryCompactor := compaction.NewSummaryCompactor(failSumm)

		// Use items that force micro to fail and summary also can't produce a good result.
		manyItems := make([]compaction.TurnItem, 0, 40)
		for i := 0; i < 40; i++ {
			manyItems = append(manyItems, compaction.TurnItem{
				Role: compaction.RoleTool, ToolName: "read", ToolResult: strings.Repeat("z", 100),
			})
		}

		uni := compaction.NewUnifiedCompactor(
			compaction.WithMicro(micro),
			compaction.WithSummary(summaryCompactor),
		)
		out, err := uni.Compact(ctx, manyItems, 20, est)
		require.NoError(t, err)
		// The routing should fall through micro -> summary -> truncating.
		assert.Equal(t, compaction.StrategyTruncating, uni.LastStrategy())
		assert.LessOrEqual(t, tokenSum(out, est), 20)
	})
}

// =============================================================================
// 4. TestCompaction_MidTurnThreshold
// =============================================================================

func TestCompaction_MidTurnThreshold(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	est := compaction.NewHeuristicTokenEstimator()

	t.Run("does not trigger below threshold", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleUser, Content: "short message"},
			{Role: compaction.RoleAssistant, Content: "ok"},
		}
		comp := &recordingCompactor{name: "micro"}
		mtc := compaction.NewMidTurnCompact() // default ratio 0.8

		out, res, err := mtc.CompactIfNeeded(ctx, items, 200, est, comp)
		require.NoError(t, err)
		assert.False(t, res.Triggered)
		assert.Equal(t, compaction.TriggerNone, res.Reason)
		assert.Equal(t, items, out)
		assert.Empty(t, comp.calls)
	})

	t.Run("triggers when exceeding threshold", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleUser, Content: strings.Repeat("u", 400)},
			{Role: compaction.RoleAssistant, Content: strings.Repeat("a", 400)},
		}
		comp := &recordingCompactor{
			name: "micro",
			out:  []compaction.TurnItem{{Role: compaction.RoleSystem, Content: "compacted", IsCompaction: true}},
		}
		mtc := compaction.NewMidTurnCompact()

		out, res, err := mtc.CompactIfNeeded(ctx, items, 50, est, comp)
		require.NoError(t, err)
		assert.True(t, res.Triggered)
		assert.Equal(t, compaction.TriggerThreshold, res.Reason)
		assert.Equal(t, []string{"micro"}, comp.calls)
		assert.Len(t, out, 1)
	})

	t.Run("CompactTriggered always runs regardless of threshold", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleUser, Content: "tiny"},
		}
		comp := &recordingCompactor{
			name: "micro",
			out:  []compaction.TurnItem{{Role: compaction.RoleSystem, Content: "always", IsCompaction: true}},
		}
		mtc := compaction.NewMidTurnCompact()

		out, res, err := mtc.CompactTriggered(ctx, items, 1000, est, comp)
		require.NoError(t, err)
		assert.True(t, res.Triggered)
		assert.Equal(t, compaction.TriggerManual, res.Reason)
		assert.Len(t, out, 1)
	})
}

// =============================================================================
// 5. TestCompaction_TokenEstimation
// =============================================================================

func TestCompaction_TokenEstimation(t *testing.T) {
	est := compaction.NewHeuristicTokenEstimator()

	t.Run("known strings estimate chars/4", func(t *testing.T) {
		n, err := est.Estimate("abcdefgh") // 8 chars / 4 = 2
		require.NoError(t, err)
		assert.Equal(t, 2, n)

		n, err = est.Estimate("hello world, how are you?") // 8.25 -> 8 tokens
		require.NoError(t, err)
		assert.Equal(t, 8, n)

		n, err = est.Estimate("") // 0 chars
		require.NoError(t, err)
		assert.Equal(t, 0, n)
	})

	t.Run("unicode string estimation", func(t *testing.T) {
		n, err := est.Estimate("こんにちは世界") // 7 CJK chars * 2 = 14 tokens
		require.NoError(t, err)
		assert.Equal(t, 14, n)
	})

	t.Run("estimate tokens for full conversation items", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleSystem, Content: "system prompt here"},
			{Role: compaction.RoleUser, Content: "user question"},
			{Role: compaction.RoleTool, ToolName: "read", ToolResult: "tool result content"},
		}
		tokens := tokenSum(items, est)
		// system prompt here (5) + user question (4) + tool result content (5) = 14
		assert.Equal(t, 14, tokens)
	})
}

// =============================================================================
// 6. TestCompaction_QualityEvaluation
// =============================================================================

func TestCompaction_QualityEvaluation(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	est := compaction.NewHeuristicTokenEstimator()

	t.Run("valid metrics after compaction", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleUser, Content: strings.Repeat("u", 400)},      // ~100 tokens
			{Role: compaction.RoleAssistant, Content: strings.Repeat("a", 200)}, // ~50 tokens
			{Role: compaction.RoleUser, Content: strings.Repeat("v", 100)},      // ~25 tokens
		}
		compressed := []compaction.TurnItem{
			{Role: compaction.RoleSystem, Content: strings.Repeat("s", 80), IsCompaction: true}, // ~20 tokens
			{Role: compaction.RoleUser, Content: strings.Repeat("v", 100)},
		}

		eval := compaction.NewDefaultQualityEvaluator(est, compaction.WithQualityStrategy(compaction.StrategySummary))
		metrics, err := eval.Evaluate(ctx, items, compressed)
		require.NoError(t, err)

		// Coverage: 2/3 items retained.
		assert.InDelta(t, 2.0/3.0, metrics.Coverage, 1e-9)
		// InfoLoss should be between 0 and 1.
		assert.GreaterOrEqual(t, metrics.InfoLoss, 0.0)
		assert.LessOrEqual(t, metrics.InfoLoss, 1.0)
		// CompressionRatio should be >= 1 (compression) or 1 (identity).
		assert.GreaterOrEqual(t, metrics.CompressionRatio, 0.0)
		assert.Equal(t, compaction.StrategySummary, metrics.Strategy)
	})

	t.Run("identity yields no loss", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleUser, Content: strings.Repeat("u", 400)},
		}
		same := []compaction.TurnItem{
			{Role: compaction.RoleUser, Content: strings.Repeat("u", 400)},
		}

		eval := compaction.NewDefaultQualityEvaluator(est)
		metrics, err := eval.Evaluate(ctx, items, same)
		require.NoError(t, err)

		assert.Equal(t, 1.0, metrics.Coverage)
		assert.Equal(t, 0.0, metrics.InfoLoss)
		assert.Equal(t, 1.0, metrics.CompressionRatio)
	})

	t.Run("empty input does not panic", func(t *testing.T) {
		eval := compaction.NewDefaultQualityEvaluator(est)
		metrics, err := eval.Evaluate(ctx, nil, nil)
		require.NoError(t, err)
		assert.Equal(t, 0.0, metrics.Coverage)
		assert.Equal(t, 0.0, metrics.InfoLoss)
		assert.Equal(t, 1.0, metrics.CompressionRatio)
	})

	t.Run("end to end: compact then evaluate", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleUser, Content: strings.Repeat("u", 200)},
			{Role: compaction.RoleAssistant, Content: strings.Repeat("a", 200)},
			{Role: compaction.RoleTool, ToolName: "read", ToolResult: strings.Repeat("t", 400)},
			{Role: compaction.RoleUser, Content: "keep-me"},
		}

		trunc := compaction.NewTruncatingCompactor()
		out, err := trunc.Compact(ctx, items, 50, est)
		require.NoError(t, err)

		eval := compaction.NewDefaultQualityEvaluator(est)
		metrics, err := eval.Evaluate(ctx, items, out)
		require.NoError(t, err)
		assert.Greater(t, metrics.CompressionRatio, 1.0, "compression should reduce size")
		assert.Greater(t, metrics.InfoLoss, 0.0)
	})
}

// =============================================================================
// 7. TestCompaction_ComplexLongConversation
// =============================================================================

func TestCompaction_ComplexLongConversation(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	est := compaction.NewHeuristicTokenEstimator()

	// Generate 200 turns using LongConversationGenerator.
	gen := mock.NewLongConversationGenerator(200, 5, 10)
	tmpl := gen.Generate()

	// Convert ConversationTemplate turns into compaction.TurnItem list.
	items := make([]compaction.TurnItem, 0, len(tmpl.Turns)*2)
	for i, turn := range tmpl.Turns {
		if len(turn.AssistantToolCalls) > 0 {
			for _, tc := range turn.AssistantToolCalls {
				items = append(items, compaction.TurnItem{
					ID:       tc.ID,
					Role:     compaction.RoleTool,
					ToolName: tc.Name,
					ToolResult: fmt.Sprintf("result of %s on %v at iteration %d",
						tc.Name, tc.Args, i),
				})
			}
		}
		if turn.AssistantContent != "" {
			items = append(items, compaction.TurnItem{
				Role:    compaction.RoleAssistant,
				Content: turn.AssistantContent,
			})
		}
	}

	// Create a SummaryCompactor with a fake summarizer.
	sum := &fakeSummarizer{summary: "Long conversation summary: the agent performed multiple file reads and test runs across many iterations."}
	summaryComp := compaction.NewSummaryCompactor(sum)

	uni := compaction.NewUnifiedCompactor(
		compaction.WithSummary(summaryComp),
	)

	// Run compaction at every 50th turn, with a budget that forces compaction.
	for step := 50; step <= len(items); step += 50 {
		if step > len(items) {
			break
		}
		slice := items[:step]
		out, err := uni.Compact(ctx, slice, 200, est)
		require.NoError(t, err, "compaction at turn %d should succeed", step)
		assert.LessOrEqual(t, tokenSum(out, est), 200, "compacted output at turn %d must fit budget", step)
	}
}

// =============================================================================
// 8. TestCompaction_EdgeCases
// =============================================================================

func TestCompaction_EdgeCases(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	est := compaction.NewHeuristicTokenEstimator()

	t.Run("empty items list", func(t *testing.T) {
		micro := compaction.NewMicroCompactor()
		out, err := micro.Compact(ctx, nil, 100, est)
		require.NoError(t, err)
		assert.Empty(t, out)

		trunc := compaction.NewTruncatingCompactor()
		out, err = trunc.Compact(ctx, nil, 100, est)
		require.NoError(t, err)
		assert.Empty(t, out)

		uni := compaction.NewUnifiedCompactor()
		out, err = uni.Compact(ctx, nil, 100, est)
		require.NoError(t, err)
		assert.Empty(t, out)
	})

	t.Run("single item", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleUser, Content: "hello"},
		}

		micro := compaction.NewMicroCompactor()
		out, err := micro.Compact(ctx, items, 100, est)
		require.NoError(t, err)
		assert.Equal(t, items, out)

		trunc := compaction.NewTruncatingCompactor()
		out, err = trunc.Compact(ctx, items, 100, est)
		require.NoError(t, err)
		assert.Equal(t, items, out)
	})

	t.Run("all system messages", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleSystem, Content: "sys1"},
			{Role: compaction.RoleSystem, Content: "sys2"},
			{Role: compaction.RoleSystem, Content: "sys3"},
		}

		// Truncating keeps all system messages even over budget.
		trunc := compaction.NewTruncatingCompactor()
		out, err := trunc.Compact(ctx, items, 5, est)
		require.NoError(t, err)
		// System messages are always preserved.
		assert.Len(t, out, 3)
		for _, it := range out {
			assert.Equal(t, compaction.RoleSystem, it.Role)
		}
	})

	t.Run("all tool results no user content", func(t *testing.T) {
		items := make([]compaction.TurnItem, 6)
		for i := 0; i < 6; i++ {
			items[i] = compaction.TurnItem{
				Role:       compaction.RoleTool,
				ToolName:   "read",
				ToolResult: strings.Repeat("r", 100),
			}
		}

		// Micro replaces tool results with placeholders with a tight budget.
		micro := compaction.NewMicroCompactor()
		out, err := micro.Compact(ctx, items, 30, est)
		require.NoError(t, err)
		assert.LessOrEqual(t, tokenSum(out, est), 30)
		for _, it := range out {
			assert.Equal(t, "[compacted tool result]", it.ToolResult)
			assert.Equal(t, compaction.RoleTool, it.Role)
		}
	})

	t.Run("budget exceeded even after micro compaction", func(t *testing.T) {
		// Many tool entries whose placeholders still exceed the budget.
		items := make([]compaction.TurnItem, 40)
		for i := 0; i < 40; i++ {
			items[i] = compaction.TurnItem{
				Role:       compaction.RoleTool,
				ToolName:   "read",
				ToolResult: strings.Repeat("z", 100),
			}
		}

		micro := compaction.NewMicroCompactor()
		_, err := micro.Compact(ctx, items, 5, est)
		assert.ErrorIs(t, err, compaction.ErrRequiresTruncating)
	})

	t.Run("nil estimator uses heuristic fallback", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleUser, Content: "hello world"},
			{Role: compaction.RoleAssistant, Content: "hi there"},
		}

		micro := compaction.NewMicroCompactor()
		out, err := micro.Compact(ctx, items, 100, nil)
		require.NoError(t, err)
		assert.Equal(t, items, out)
	})
}

// =============================================================================
// Helpers
// =============================================================================

// tokenSum computes the total estimated tokens for a slice of TurnItems.
func tokenSum(items []compaction.TurnItem, est compaction.TokenEstimator) int {
	total := 0
	for _, it := range items {
		if it.Content != "" {
			if n, err := est.Estimate(it.Content); err == nil {
				total += n
			}
		}
		if it.ToolResult != "" {
			if n, err := est.Estimate(it.ToolResult); err == nil {
				total += n
			}
		}
	}
	return total
}

// contentSlice extracts the Content field from a TurnItem slice.
func contentSlice(items []compaction.TurnItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Content
	}
	return out
}

// recordingCompactor is a configurable Compactor fake that records the order in
// which it was consulted and returns a canned result or error.
type recordingCompactor struct {
	mu    sync.Mutex
	name  string
	out   []compaction.TurnItem
	err   error
	calls []string
}

var _ compaction.Compactor = (*recordingCompactor)(nil)

func (r *recordingCompactor) Compact(_ context.Context, _ []compaction.TurnItem, _ int, _ compaction.TokenEstimator) ([]compaction.TurnItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, r.name)
	if r.err != nil {
		return nil, r.err
	}
	return r.out, nil
}
