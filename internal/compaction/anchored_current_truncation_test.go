package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func anchoredCurrentFixtureItems() []TurnItem {
	const runID = "synthetic-anchored-run"
	callOld := llm.ToolCall{ID: "old-call", Name: "fixture_read", Args: map[string]any{}}
	callNew := llm.ToolCall{ID: "new-call", Name: "fixture_read", Args: map[string]any{}}
	source := func(index uint64, outcome string) llm.RuntimeMetadata {
		return llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: runID, TranscriptIndex: index}, ReceiptOutcome: outcome}
	}
	return []TurnItem{
		{ID: "system", Role: RoleSystem, Content: "system", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}},
		{ID: "input", Role: RoleUser, Content: "original input", Runtime: source(0, "")},
		{ID: "old-assistant", Role: RoleAssistant, ToolCalls: []llm.ToolCall{callOld}, Runtime: source(1, "")},
		{ID: "old-receipt", Role: RoleTool, ToolCallID: callOld.ID, ToolName: callOld.Name, ToolResult: strings.Repeat("synthetic data ", 2000), ReceiptOutcome: "success", Runtime: source(2, "success")},
		{ID: "new-assistant", Role: RoleAssistant, ToolCalls: []llm.ToolCall{callNew}, Runtime: source(3, "")},
		{ID: "new-receipt", Role: RoleTool, ToolCallID: callNew.ID, ToolName: callNew.Name, ToolResult: "recent", ReceiptOutcome: "success", Runtime: source(4, "success")},
	}
}

func TestAnchoredCurrentRunTruncationKeepsInputAndNewestGroup(t *testing.T) {
	items := anchoredCurrentFixtureItems()
	require.NoError(t, ValidateRunRequestIntegrity(items))
	require.NoError(t, ValidateCompactionEligible(items))
	estimator := NewHeuristicTokenEstimator()
	const budget = 512
	require.Greater(t, EstimateTokens(items, estimator), budget)
	result, err := NewUnifiedCompactor().Compact(t.Context(), items, budget, estimator)
	require.NoError(t, err, "a completed older group should be replaceable without dropping the original input")
	require.NotNil(t, result.NewProjection)
	require.Equal(t, llm.SourceThrough{TranscriptCount: 3}, result.NewProjection.Through)
	require.Equal(t, DurableTruncationMarker, result.NewProjection.Summary)
	require.Len(t, result.Items, 5)
	require.Equal(t, items[0], result.Items[0])
	input := result.Items[1]
	input.EstimatedTokens = 0
	require.Equal(t, items[1], input)
	suffix := append([]TurnItem(nil), result.Items[3:]...)
	for i := range suffix {
		suffix[i].EstimatedTokens = 0
	}
	require.Equal(t, items[4:], suffix)
	require.LessOrEqual(t, EstimateTokens(result.Items, estimator), budget)
	require.NoError(t, ValidateRunRequestIntegrity(result.Items))
}

func TestAnchoredCurrentRunRejectsUnsafeOrUnfittableGroups(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func([]TurnItem) []TurnItem
		want   error
	}{
		{name: "unknown receipt", mutate: func(items []TurnItem) []TurnItem {
			items[3].ReceiptOutcome, items[3].Runtime.ReceiptOutcome = "unknown", "unknown"
			return items
		}, want: ErrRunHistoryNotCompactionEligible},
		{name: "open tool group", mutate: func(items []TurnItem) []TurnItem {
			return append(items[:3], items[4:]...)
		}, want: ErrInvalidRunRequestHistory},
		{name: "protected image", mutate: func(items []TurnItem) []TurnItem {
			items[1].ContentBlocks = []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://fixture.invalid/image"}}}
			return items
		}, want: ErrImageCompactionBlocked},
		{name: "input cannot fit", mutate: func(items []TurnItem) []TurnItem {
			items[1].Content = strings.Repeat("required input ", 2000)
			return items
		}, want: ErrRequiresTruncating},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := tc.mutate(anchoredCurrentFixtureItems())
			_, err := NewUnifiedCompactor().Compact(t.Context(), items, 512, NewHeuristicTokenEstimator())
			require.ErrorIs(t, err, tc.want)
		})
	}
}
