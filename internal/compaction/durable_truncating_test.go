package compaction

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func durableTruncatingItems() []TurnItem {
	items := []TurnItem{{ID: "system", Role: RoleSystem, Content: "system", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}}}
	for index, text := range []string{"small old item", strings.Repeat("large old group ", 200), "retained recent item"} {
		items = append(items, TurnItem{ID: []string{"old-small", "old-large", "recent"}[index], Role: RoleUser, Content: text, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourcePriorPublished, HistoryRevision: "revision", BranchID: "main", SourceEntryID: []string{"real-small", "real-large", "real-recent"}[index], PriorOrdinal: uint64(index)}}})
	}
	return append(items, TurnItem{ID: "current", Role: RoleUser, Content: "current input", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "current-run", PriorSourceCount: 3, TranscriptIndex: 0}}})
}

func TestDurableTruncatingProducesExactPrefixProjectionAndWholeSuffix(t *testing.T) {
	items := durableTruncatingItems()
	result, err := NewTruncatingCompactor().Compact(t.Context(), items, 300, runeCountEstimator{})
	require.NoError(t, err)
	require.Equal(t, StrategyTruncating, result.Strategy)
	require.NotNil(t, result.NewProjection)
	require.Equal(t, llm.SourceThrough{PriorHistoryCount: 2}, result.NewProjection.Through)
	require.Contains(t, result.NewProjection.Summary, "omitted")
	require.Len(t, result.Items, 4)
	require.Equal(t, items[0], result.Items[0])
	require.True(t, result.Items[1].IsCompaction)
	require.Equal(t, result.NewProjection.Summary, result.Items[1].Content)
	require.Equal(t, llm.SourceRunSummary, result.Items[1].Runtime.Source.Kind)
	require.Equal(t, items[3:], result.Items[2:], "must retain an unchanged continuous suffix; it cannot skip a large newer group and keep an older small item")
	require.LessOrEqual(t, EstimateTokens(result.Items, runeCountEstimator{}), 300)
	require.NoError(t, ValidateRunRequestIntegrity(result.Items))
	require.Equal(t, "small old item", items[1].Content)
}

func TestDurableTruncatingProtectsCurrentInputSystemAndUnknownHistory(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func([]TurnItem)
	}{
		{"current input too large", func(items []TurnItem) { items[len(items)-1].Content = strings.Repeat("current must stay ", 200) }},
		{"source backed system cannot be omitted", func(items []TurnItem) { items[1].Role = RoleSystem }},
		{"unknown source", func(items []TurnItem) { items[1].Runtime.Source = llm.SourceRef{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			items := durableTruncatingItems()
			test.change(items)
			_, err := NewTruncatingCompactor().Compact(t.Context(), items, 300, runeCountEstimator{})
			require.Error(t, err)
		})
	}
	items := durableTruncatingItems()
	unchanged, err := NewTruncatingCompactor().Compact(t.Context(), items, 20000, runeCountEstimator{})
	require.NoError(t, err)
	require.Nil(t, unchanged.NewProjection)
	require.Equal(t, items, unchanged.Items)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = NewTruncatingCompactor().Compact(canceled, items, 300, runeCountEstimator{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestDurableTruncatingKeepsToolGroupsWhole(t *testing.T) {
	items := durableTruncatingItems()
	items[1].Content = "old question"
	items[2].Role = RoleAssistant
	items[2].ToolCalls = []llm.ToolCall{{ID: "call", Name: "read"}}
	items[3].Role = RoleTool
	items[3].Content = ""
	items[3].ToolResult = strings.Repeat("large result ", 300)
	items[3].ToolCallID, items[3].ToolName = "call", "read"
	items[3].ReceiptOutcome, items[3].Runtime.ReceiptOutcome = "success", "success"
	result, err := NewTruncatingCompactor().Compact(t.Context(), items, 300, runeCountEstimator{})
	require.NoError(t, err)
	require.NotNil(t, result.NewProjection)
	require.Equal(t, llm.SourceThrough{PriorHistoryCount: 3}, result.NewProjection.Through)
	require.Len(t, result.Items, 3)
	require.Equal(t, items[len(items)-1], result.Items[2])
	require.NoError(t, ValidateRunRequestIntegrity(result.Items))
	items[3].ReceiptOutcome, items[3].Runtime.ReceiptOutcome = "unknown", "unknown"
	_, err = NewTruncatingCompactor().Compact(t.Context(), items, 300, runeCountEstimator{})
	require.ErrorIs(t, err, ErrRunHistoryNotCompactionEligible)
}

type borrowedSummaryFailure struct{ err error }

func (s borrowedSummaryFailure) Summarize(context.Context, string) (string, error) { return "", s.err }

func TestUnifiedDurableUsesTypedTruncationForKnownLocalSummaryLimit(t *testing.T) {
	for _, summary := range []Compactor{nil, NewSummaryCompactor(nil), NewSummaryCompactor(&fakeSummarizer{summary: strings.Repeat("summary ", 200)}, WithMaxSummaryTokens(1000))} {
		router := NewUnifiedCompactor(WithSummary(summary))
		result, err := router.Compact(t.Context(), durableTruncatingItems(), 300, runeCountEstimator{})
		require.NoError(t, err)
		require.Equal(t, StrategyTruncating, result.Strategy)
		require.NotNil(t, result.NewProjection)
		require.Contains(t, result.NewProjection.Summary, "omitted")
		require.Equal(t, llm.SourceThrough{PriorHistoryCount: 2}, result.NewProjection.Through)
		require.LessOrEqual(t, EstimateTokens(result.Items, runeCountEstimator{}), 300)
	}
}

func TestUnifiedDurableDoesNotTrustSelfReportedOrBorrowedLocalFailure(t *testing.T) {
	_, borrowed := NewSummaryCompactor(&fakeSummarizer{summary: strings.Repeat("large ", 200)}, WithMaxSummaryTokens(1000)).Compact(t.Context(), durableTruncatingItems(), 30, runeCountEstimator{})
	require.ErrorIs(t, borrowed, ErrRequiresTruncating)
	for _, summary := range []Compactor{NewRecordingCompactor("custom").WithError(ErrRequiresTruncating), NewSummaryCompactor(borrowedSummaryFailure{err: borrowed}), NewSummaryCompactor(borrowedSummaryFailure{err: context.Canceled})} {
		truncating := NewRecordingCompactor("must-not-fallback")
		_, err := NewUnifiedCompactor(WithSummary(summary), WithTruncating(truncating)).Compact(t.Context(), durableTruncatingItems(), 300, runeCountEstimator{})
		require.Error(t, err)
		require.Empty(t, truncating.Called(), "foreign summary errors must not be relabeled as local completion")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	fallback := NewRecordingCompactor("canceled")
	_, err := NewUnifiedCompactor(WithTruncating(fallback)).Compact(canceled, durableTruncatingItems(), 300, runeCountEstimator{})
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, fallback.Called())
}

type truncatingCountingEstimator struct{ calls int }

func (e *truncatingCountingEstimator) Estimate(text string) (int, error) {
	e.calls++
	return len([]rune(text)), nil
}

func TestDurableTruncatingPricesLargeHistoryOnce(t *testing.T) {
	const count = 2000
	items := make([]TurnItem, 0, count+1)
	for index := 0; index < count; index++ {
		items = append(items, TurnItem{Role: RoleUser, Content: strings.Repeat("old ", 10), Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourcePriorPublished, HistoryRevision: "large-revision", BranchID: "main", SourceEntryID: fmt.Sprintf("real-%d", index), PriorOrdinal: uint64(index)}}})
	}
	items = append(items, TurnItem{Role: RoleUser, Content: "current", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "run", PriorSourceCount: count, TranscriptIndex: 0}}})
	estimator := &truncatingCountingEstimator{}
	result, err := NewTruncatingCompactor().Compact(t.Context(), items, 200, estimator)
	require.NoError(t, err)
	require.NotNil(t, result.NewProjection)
	require.Less(t, estimator.calls, 12*len(items), "finding a prefix must not repeatedly reprice each remaining suffix")
}

func TestDurableTruncatingAdvancesBeyondExistingSameRunSummary(t *testing.T) {
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("old summary ", 200), IsCompaction: true, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "run", Through: llm.SourceThrough{PriorHistoryCount: 1}}}},
		{Role: RoleAssistant, Content: "newer prior source", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourcePriorPublished, HistoryRevision: "revision", BranchID: "main", SourceEntryID: "real-1", PriorOrdinal: 1}}},
		{Role: RoleUser, Content: "current", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "run", PriorSourceCount: 2, TranscriptIndex: 0}}},
	}
	result, err := NewTruncatingCompactor().Compact(t.Context(), items, 300, runeCountEstimator{})
	require.NoError(t, err)
	require.NotNil(t, result.NewProjection)
	require.Equal(t, llm.SourceThrough{PriorHistoryCount: 2}, result.NewProjection.Through, "a new projection must advance beyond its same-run predecessor")
	require.Equal(t, items[2], result.Items[len(result.Items)-1])
}

func TestUnifiedDurablePreservesProtectedSummaryFailures(t *testing.T) {
	for _, sourceErr := range []error{
		context.Canceled, context.DeadlineExceeded,
		&llm.DispatchDeniedError{Code: llm.DeniedBudget},
		&llm.DispatchDeniedError{Code: llm.DeniedSafetyFact},
		&llm.FinishedProviderAttemptError{Result: llm.ProviderAttemptResult{OperationID: "self-reported-op", AttemptID: "self-reported-attempt", Outcome: llm.AttemptProviderFailure, Failure: llm.FailureTransientProvider}},
		fmt.Errorf("provider cleanup outcome remains unknown"),
		fmt.Errorf("provider operation accounting persistence failed"),
	} {
		fallback := NewRecordingCompactor("must-not-truncate")
		_, err := NewUnifiedCompactor(WithSummary(NewSummaryCompactor(borrowedSummaryFailure{err: sourceErr})), WithTruncating(fallback)).Compact(t.Context(), durableTruncatingItems(), 300, runeCountEstimator{})
		require.ErrorIs(t, err, sourceErr)
		require.Empty(t, fallback.Called(), "finished-error fields alone must not authorize fallback or extra main work")
	}
}
