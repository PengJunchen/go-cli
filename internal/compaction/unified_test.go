package compaction

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
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
		callID := fmt.Sprintf("call-%d", i)
		items = append(items, TurnItem{
			Role:      RoleAssistant,
			ToolCalls: []llm.ToolCall{{ID: callID, Name: "read"}},
		})
		items = append(items, TurnItem{
			Role:       RoleTool,
			ToolCallID: callID,
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
	outResult, err := uni.Compact(ctx, items, 523, est)
	out := outResult.Items
	require.NoError(t, err)
	assert.Equal(t, StrategyMicro, uni.LastStrategy())
	assert.LessOrEqual(t, estimateTokens(out, est), 523)

	// A single compaction span is emitted for the routing decision.
	assertSpanEventually(t, exp, "compaction")
}

func TestUnifiedCompactorProducesTypedProjectionForDurableHistory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	const revision = "revision-durable-unified"
	const branch = "branch-main"
	const runID = "run-durable-unified"
	priorRef := func(ordinal uint64, entryID string) llm.SourceRef {
		return llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: revision, BranchID: branch,
			SourceEntryID: entryID, PriorOrdinal: ordinal,
		}
	}
	runtime := func(source llm.SourceRef, receipt string) llm.RuntimeMetadata {
		return llm.RuntimeMetadata{Source: source, ReceiptOutcome: receipt}
	}
	callID := "call-large-write"
	items := []TurnItem{
		{
			Role: RoleUser, Content: "older user context",
			Runtime: runtime(priorRef(0, "entry-user"), ""),
		},
		{
			Role: RoleAssistant,
			ToolCalls: []llm.ToolCall{{
				ID: callID, Name: "write",
				Args: map[string]any{"path": "old.go", "content": strings.Repeat("x", 500)},
			}},
			Runtime: runtime(priorRef(1, "entry-write-call"), ""),
		},
		{
			Role: RoleTool, ToolCallID: callID, ToolName: "write",
			ToolResult: strings.Repeat("r", 8000), ReceiptOutcome: "success",
			Runtime: runtime(priorRef(2, "entry-write-receipt"), "success"),
		},
		{
			Role: RoleUser, Content: "current user request",
			Runtime: runtime(llm.SourceRef{
				Kind: llm.SourceRunTranscript, RunID: runID, PriorSourceCount: 3, TranscriptIndex: 0,
			}, ""),
		},
	}
	const summaryText = "typed summary of the prior write"
	summarizer := &fakeSummarizer{summary: summaryText}
	microResult, err := NewMicroCompactor().Compact(ctx, items, 1000, runeCountEstimator{})
	require.NoError(t, err, "the micro strategy can fit this history by shrinking the receipt")
	assert.LessOrEqual(t, EstimateTokens(microResult.Items, runeCountEstimator{}), 1000)
	compactor := NewUnifiedCompactor(WithSummary(NewSummaryCompactor(summarizer, WithMaxSummaryTokens(len(summaryText)))))

	result, err := compactor.Compact(ctx, items, 1000, runeCountEstimator{})
	require.NoError(t, err)
	assert.Equal(t, StrategySummary, result.Strategy,
		"a micro result that fits by replacing a durable tool receipt still cannot be projected")
	require.NotNil(t, result.NewProjection,
		"a micro or truncating result cannot establish a durable summary projection")
	assert.Equal(t, llm.SourceThrough{PriorHistoryCount: 3}, result.NewProjection.Through)
	require.NotEmpty(t, summarizer.conversations(), "the configured summary strategy should run")
	assert.Equal(t, items[3].Runtime.Source, result.Items[len(result.Items)-1].Runtime.Source,
		"the current source-backed user input must remain unchanged at the suffix")
}

func TestUnifiedCompactorDoesNotForceSummaryForFittingDurableHistory(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	items := []TurnItem{{
		Role: RoleUser, Content: "already fits",
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: "revision-fits", BranchID: "branch-main",
			SourceEntryID: "entry-fits", PriorOrdinal: 0,
		}},
	}}
	summarizer := &fakeSummarizer{summary: "should not be used"}
	compactor := NewUnifiedCompactor(WithSummary(NewSummaryCompactor(summarizer)))

	result, err := compactor.Compact(ctx, items, 1000, runeCountEstimator{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, items[0].Content, result.Items[0].Content)
	assert.Equal(t, items[0].Runtime.Source, result.Items[0].Runtime.Source)
	assert.Nil(t, result.NewProjection)
	assert.Empty(t, summarizer.conversations(), "an already fitting durable request needs no forced projection")
}

func TestUnifiedCompactorReestimatesDurableHistoryInsteadOfTrustingOldCache(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	const revision = "revision-stale-cache"
	const branch = "branch-main"
	priorSource := func(ordinal uint64, entryID string) llm.RuntimeMetadata {
		return llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: revision, BranchID: branch,
			SourceEntryID: entryID, PriorOrdinal: ordinal,
		}}
	}
	callID := "call-large-args"
	args := map[string]any{"path": "old.go", "content": strings.Repeat("x", 2000)}
	writeCall := TurnItem{
		Role: RoleAssistant, EstimatedTokens: 1,
		ToolCalls: []llm.ToolCall{{ID: callID, Name: "write", Args: args}},
		Runtime:   priorSource(1, "entry-write-call"),
	}
	current := TurnItem{
		Role: RoleUser, Content: "fresh request",
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourceRunTranscript, RunID: "run-stale-cache", PriorSourceCount: 3, TranscriptIndex: 0,
		}},
	}
	items := []TurnItem{
		{Role: RoleUser, Content: "older context", Runtime: priorSource(0, "entry-user")},
		writeCall,
		{
			Role: RoleTool, ToolCallID: callID, ToolName: "write", ToolResult: "ok", ReceiptOutcome: "success",
			Runtime: llm.RuntimeMetadata{
				Source: priorSource(2, "entry-write-receipt").Source, ReceiptOutcome: "success",
			},
		},
		current,
	}
	micro := NewRecordingCompactor("micro").WithError(ErrRequiresTruncating)
	const summaryText = "summary of prior context"
	summarizer := &fakeSummarizer{summary: summaryText}
	compactor := NewUnifiedCompactor(
		WithMicro(micro),
		WithSummary(NewSummaryCompactor(summarizer, WithMaxSummaryTokens(len(summaryText)))),
	)

	result, err := compactor.Compact(ctx, items, 119, runeCountEstimator{})
	require.NoError(t, err)
	assert.Equal(t, StrategySummary, result.Strategy)
	assert.Empty(t, micro.Called(), "fresh provider-visible size should select the durable summary path directly")
	assert.NotEmpty(t, summarizer.conversations())
	assert.Equal(t, current.Runtime.Source, result.Items[len(result.Items)-1].Runtime.Source,
		"the cache regression must still keep the current source-backed input whole")
	assert.Equal(t, 1, writeCall.EstimatedTokens, "re-estimation must not mutate caller-owned cache hints")
}

func TestUnifiedCompactorDoesNotTruncateOverBudgetDurableHistoryWithoutSummary(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	items := []TurnItem{{
		Role: RoleUser, Content: strings.Repeat("x", 2000),
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: "revision-no-summary", BranchID: "branch-main",
			SourceEntryID: "entry-no-summary", PriorOrdinal: 0,
		}},
	}}
	truncating := NewRecordingCompactor("truncating").WithResult([]TurnItem{{Role: RoleUser, Content: "truncated"}})
	compactor := NewUnifiedCompactor(WithTruncating(truncating))

	_, err := compactor.Compact(ctx, items, 100, runeCountEstimator{})
	require.Error(t, err, "durable source cannot use a projection-free truncating winner")
	assert.Empty(t, truncating.Called(), "durable history must fail closed when no summary strategy exists")
}

func TestUnifiedCompactorDoesNotFallBackAfterDurableSummaryFailure(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	items := []TurnItem{{
		Role: RoleUser, Content: strings.Repeat("x", 2000),
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: "revision-summary-fails", BranchID: "branch-main",
			SourceEntryID: "entry-summary-fails", PriorOrdinal: 0,
		}},
	}}
	summary := NewRecordingCompactor("summary").WithError(errors.New("summary service unavailable"))
	truncating := NewRecordingCompactor("truncating").WithResult([]TurnItem{{Role: RoleUser, Content: "truncated"}})
	compactor := NewUnifiedCompactor(WithSummary(summary), WithTruncating(truncating))

	_, err := compactor.Compact(ctx, items, 100, runeCountEstimator{})
	require.EqualError(t, err, "summary service unavailable")
	assert.Equal(t, []string{"summary"}, summary.Called())
	assert.Empty(t, truncating.Called(), "a failed typed-summary path cannot silently use truncating")
}

func TestUnifiedCompactorRejectsDurableSummaryWithoutProjection(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	items := []TurnItem{{
		Role: RoleUser, Content: strings.Repeat("x", 2000),
		Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{
			Kind: llm.SourcePriorPublished, HistoryRevision: "revision-unprojected", BranchID: "branch-main",
			SourceEntryID: "entry-unprojected", PriorOrdinal: 0,
		}},
	}}
	summary := NewRecordingCompactor("summary").WithResult([]TurnItem{{Role: RoleUser, Content: "summary without projection"}})
	truncating := NewRecordingCompactor("truncating").WithResult([]TurnItem{{Role: RoleUser, Content: "truncated"}})
	compactor := NewUnifiedCompactor(WithSummary(summary), WithTruncating(truncating))

	_, err := compactor.Compact(ctx, items, 100, runeCountEstimator{})
	require.ErrorIs(t, err, ErrDurableSummaryProjectionRequired)
	assert.Equal(t, []string{"summary"}, summary.Called())
	assert.Empty(t, truncating.Called(), "a projectionless summary cannot fall through to truncation")
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
	outResult, err := uni.Compact(ctx, items, 100, est)
	out := outResult.Items
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
	outResult, err := uni.Compact(ctx, items, 100, est)
	out := outResult.Items
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
	outResult, err := uni.Compact(ctx, items, 40, est)
	out := outResult.Items
	require.NoError(t, err)
	assert.Equal(t, StrategyTruncating, uni.LastStrategy())
	assert.LessOrEqual(t, estimateTokens(out, est), 40)

	// All system entries survive truncation.
	for _, it := range out {
		assert.NotEqual(t, RoleTool, it.Role, "tools older than budget are dropped")
	}
}

func TestUnifiedCompactorPropagatesMalformedToolGroupWithoutFallback(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	summarizer := &fakeSummarizer{summary: "must not run"}
	micro := NewRecordingCompactor("micro").WithError(ErrRequiresTruncating)
	truncating := NewRecordingCompactor("truncating").WithResult([]TurnItem{{Role: RoleUser, Content: "incorrect fallback"}})
	uni := NewUnifiedCompactor(
		WithMicro(micro),
		WithSummary(NewSummaryCompactor(summarizer)),
		WithTruncating(truncating),
	)

	items := []TurnItem{{
		Role:       RoleTool,
		ToolCallID: "orphan",
		ToolName:   "read",
		ToolResult: "orphan result",
	}}
	_, err := uni.Compact(ctx, items, 20, est)
	require.ErrorIs(t, err, errMalformedToolGroup)
	assert.Empty(t, summarizer.conversations())
	assert.Empty(t, truncating.Called(), "malformed transcript is not a strategy escalation reason")
}

func TestUnifiedCompactorRejectsMandatorySystemOverBudgetBeforeStrategies(t *testing.T) {
	ctx, _ := newTracedCtx(t)
	est := runeCountEstimator{}
	micro := NewRecordingCompactor("micro").WithResult([]TurnItem{{Role: RoleUser, Content: "unsafe"}})
	summary := NewRecordingCompactor("summary").WithResult([]TurnItem{{Role: RoleUser, Content: "unsafe"}})
	truncating := NewRecordingCompactor("truncating").WithResult([]TurnItem{{Role: RoleUser, Content: "unsafe"}})
	uni := NewUnifiedCompactor(
		WithMicro(micro),
		WithSummary(summary),
		WithTruncating(truncating),
	)
	items := []TurnItem{
		{Role: RoleSystem, Content: "mandatory-system"},
		{Role: RoleUser, Content: "question"},
	}

	_, err := uni.Compact(ctx, items, 5, est)
	require.ErrorIs(t, err, ErrMandatorySystemPromptOverBudget)
	assert.Empty(t, micro.Called())
	assert.Empty(t, summary.Called())
	assert.Empty(t, truncating.Called())
}

func TestUnifiedCompactorEmptyInput(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	uni := NewUnifiedCompactor()
	outResult, err := uni.Compact(ctx, nil, 100, est)
	out := outResult.Items
	require.NoError(t, err)
	assert.Empty(t, out)
	assert.Equal(t, StrategyMicro, uni.LastStrategy())
}

func TestUnifiedCompactorCompileGuard(t *testing.T) {
	var _ Compactor = (*UnifiedCompactor)(nil)
	assert.NotNil(t, NewUnifiedCompactor())
	assert.Equal(t, StrategyNone, NewUnifiedCompactor().LastStrategy())
}

// recordingEvaluator is a QualityEvaluator fake that records every call so
// tests can assert the evaluator was invoked with the right before/after items.
type recordingEvaluator struct {
	called  bool
	before  []TurnItem
	after   []TurnItem
	metrics *QualityMetrics
	evalErr error
}

var _ QualityEvaluator = (*recordingEvaluator)(nil)

func (r *recordingEvaluator) Evaluate(_ context.Context, items []TurnItem, compressed []TurnItem) (*QualityMetrics, error) {
	r.called = true
	r.before = items
	r.after = compressed
	if r.metrics != nil {
		return r.metrics, nil
	}
	return &QualityMetrics{Coverage: 1.0, CompressionRatio: 1.0}, nil
}

// TestQualityAutoEvaluate verifies that a UnifiedCompactor wired with
// WithQualityEvaluator invokes the evaluator after a successful compaction
// (AC-2), passing the original and compacted items.
func TestQualityAutoEvaluate(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	items := largeToolItems()

	// Use a recording evaluator to verify the call contract.
	rec := &recordingEvaluator{}
	uni := NewUnifiedCompactor(WithQualityEvaluator(rec))

	outResult, err := uni.Compact(ctx, items, 523, est)

	out := outResult.Items
	require.NoError(t, err)
	assert.Equal(t, StrategyMicro, uni.LastStrategy())
	assert.LessOrEqual(t, estimateTokens(out, est), 523)

	// The evaluator was called with the original and compacted items.
	assert.True(t, rec.called, "evaluator must be invoked after compaction")
	assert.Equal(t, items, rec.before, "evaluator receives the original items")
	assert.Equal(t, out, rec.after, "evaluator receives the compacted items")
}

// TestQualityAutoEvaluateWithDefaultEvaluator verifies the real
// DefaultQualityEvaluator, when wired through WithQualityEvaluator, produces a
// "compaction.quality" child span carrying the coverage/info_loss/
// compression_ratio attributes (AC-4).
func TestQualityAutoEvaluateWithDefaultEvaluator(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()
	items := largeToolItems()

	uni := NewUnifiedCompactor(
		WithQualityEvaluator(NewDefaultQualityEvaluator(est, WithQualityStrategy(StrategyMicro))),
	)
	outResult, err := uni.Compact(ctx, items, 523, est)
	out := outResult.Items
	require.NoError(t, err)
	assert.Equal(t, StrategyMicro, uni.LastStrategy())

	// AC-4: a "compaction.quality" child span is emitted carrying the
	// coverage/info_loss/compression_ratio attributes.
	qualitySpan := findSpanEventually(t, exp, "compaction.quality")
	keys := spanAttributeKeys(qualitySpan)
	assert.Contains(t, keys, "coverage")
	assert.Contains(t, keys, "info_loss")
	assert.Contains(t, keys, "compression_ratio")

	// Compaction reduced tokens, so compression_ratio > 1.
	assert.True(t, estimateTokens(items, est) > estimateTokens(out, est),
		"compaction must reduce token count")
}

// TestQualityEvaluatorNilSkips verifies that a UnifiedCompactor without an
// evaluator does not emit a "compaction.quality" span (grayscale safety).
func TestQualityEvaluatorNilSkips(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()
	items := largeToolItems()

	uni := NewUnifiedCompactor() // no evaluator
	_, err := uni.Compact(ctx, items, 300, est)
	require.NoError(t, err)

	// Wait briefly for any async span export, then confirm no quality span.
	time.Sleep(50 * time.Millisecond)
	for _, s := range exp.Spans() {
		assert.NotEqual(t, "compaction.quality", s.Name,
			"no quality span should be emitted when evaluator is nil")
	}
}
