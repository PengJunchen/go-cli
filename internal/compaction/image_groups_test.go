package compaction

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

type imageGroupSummarizer struct{ calls int }

func (s *imageGroupSummarizer) Summarize(_ context.Context, _ string) (string, error) {
	s.calls++
	return "old context", nil
}
func imageGroupHistory() []TurnItem {
	return []TurnItem{
		{ID: "old", Role: RoleUser, Content: strings.Repeat("prior words ", 300)},
		{ID: "assistant-image", Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "same-id", Name: "read", Args: map[string]any{"path": "fixture.png"}}, {ID: "text-id", Name: "read", Args: map[string]any{"path": "fixture.txt"}}}},
		{ID: "receipt-image", Role: RoleTool, ToolCallID: "same-id", ToolName: "read", ToolResult: "image", ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:image/png;base64,AA==", Detail: "high"}}}},
		{ID: "receipt-text", Role: RoleTool, ToolCallID: "text-id", ToolName: "read", ToolResult: "context that belongs to the same exchange"},
		{ID: "current", Role: RoleUser, Content: "continue"},
	}
}

func TestImageCompactionBlocksBeforeSummaryAndTruncation(t *testing.T) {
	items := imageGroupHistory()
	estimator := NewHeuristicTokenEstimator()
	summary := &imageGroupSummarizer{}
	_, err := NewSummaryCompactor(summary, WithMaxSummaryTokens(8)).Compact(context.Background(), items, 20, estimator)
	require.Error(t, err)
	require.Zero(t, summary.calls, "image bytes must not be omitted in a text-only summary request")
	_, err = NewTruncatingCompactor().Compact(context.Background(), items, 20, estimator)
	require.Error(t, err, "a complete image exchange must not disappear")
}

func TestImageCompactionAllowsEarlierTextPrefix(t *testing.T) {
	items := imageGroupHistory()
	summary := &imageGroupSummarizer{}
	result, err := NewSummaryCompactor(summary, WithMaxSummaryTokens(8)).Compact(context.Background(), items, 250, NewHeuristicTokenEstimator())
	require.NoError(t, err)
	require.Equal(t, 1, summary.calls)
	require.Len(t, result.Items, 5)
	for i := range result.Items {
		result.Items[i].EstimatedTokens = 0
	}
	require.Equal(t, items[1:], result.Items[1:])
}

func TestImageCompactionRejectsChangedOrMissingExchange(t *testing.T) {
	for _, name := range []string{"drop-group", "drop-assistant", "drop-text", "change-image", "change-detail", "change-call", "change-source", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			before := imageGroupHistory()
			raw, err := json.Marshal(before)
			require.NoError(t, err)
			var after []TurnItem
			require.NoError(t, json.Unmarshal(raw, &after))
			switch name {
			case "drop-group":
				after = append(after[:1], after[4:]...)
			case "drop-assistant":
				after = append(after[:1], after[2:]...)
			case "drop-text":
				after = append(after[:3], after[4:]...)
			case "change-image":
				after[2].ContentBlocks[0].ImageURL.URL = "data:image/png;base64,AQ=="
			case "change-detail":
				after[2].ContentBlocks[0].ImageURL.Detail = "low"
			case "change-call":
				after[1].ToolCalls[0].Args = map[string]any{"path": "different.png"}
			case "change-source":
				after[1].ID = "other-source"
			case "duplicate":
				after = append(after, after[1:4]...)
			}
			require.ErrorIs(t, ValidateImageGroupRetention(before, after), ErrImageCompactionBlocked)
		})
	}
	before := imageGroupHistory()
	second := imageGroupHistory()[1:4]
	for i := range second {
		second[i].ID += "-second"
	}
	before = append(before, second...)
	require.NoError(t, ValidateImageGroupRetention(before, before), "same tool-call IDs in separately identified closed groups remain legal")
}

func TestImageCompactionUsesSourceQualifiedExchangeIdentities(t *testing.T) {
	for _, kind := range []llm.SourceKind{llm.SourcePriorPublished, llm.SourceRunTranscript} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			before := imageGroupHistory()
			second := imageGroupHistory()[1:4]
			before = append(before, second...)
			for i := range before {
				source := llm.SourceRef{Kind: kind, RunID: "run-images", TranscriptIndex: uint64(i)}
				if kind == llm.SourcePriorPublished {
					source = llm.SourceRef{Kind: kind, HistoryRevision: "revision", BranchID: "main", SourceEntryID: fmt.Sprintf("entry-%d", i), PriorOrdinal: uint64(i)}
				}
				before[i].Runtime = llm.RuntimeMetadata{Source: source}
			}
			require.NoError(t, ValidateImageGroupRetention(before, before), "reused tool IDs in different source-qualified groups are allowed")
			after := append([]TurnItem(nil), before...)
			if kind == llm.SourcePriorPublished {
				after[1].Runtime.Source.SourceEntryID = "different-entry"
			} else {
				after[1].Runtime.Source.TranscriptIndex = 99
			}
			require.ErrorIs(t, ValidateImageGroupRetention(before, after), ErrImageCompactionBlocked)
		})
	}
}

func TestImageCompactionNoPriorImagesStillRejectsInjectedExchange(t *testing.T) {
	before := []TurnItem{{ID: "plain-user", Role: RoleUser, Content: "ordinary text"}}
	injected := append(append([]TurnItem(nil), before...), imageGroupHistory()[1:4]...)
	require.ErrorIs(t, ValidateImageGroupRetention(before, injected), ErrImageCompactionBlocked, "an empty original image inventory is not permission to add image evidence")
	malformed := injected[:len(injected)-1]
	require.ErrorIs(t, ValidateImageGroupRetention(before, malformed), errMalformedToolGroup, "a new incomplete image exchange is still rejected by the original structural guard")
	plainMalformed := []TurnItem{{ID: "assistant", Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "missing", Name: "read"}}}}
	err := ValidateImageGroupRetention(before, plainMalformed)
	require.ErrorIs(t, err, errMalformedToolGroup)
	require.NotErrorIs(t, err, ErrImageCompactionBlocked, "text-only structural errors must preserve their original classification")
}
