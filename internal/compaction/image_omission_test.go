package compaction

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func imageOmissionFixture(t *testing.T) []TurnItem {
	t.Helper()
	var pngBytes bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.RGBA{R: 128, G: 32, B: 8, A: 255})
	require.NoError(t, png.Encode(&pngBytes, img))
	imageURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	items := []TurnItem{
		{ID: "system", Role: RoleSystem, Content: "instructions", Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceEphemeral}}},
		{ID: "current", Role: RoleUser, Content: "current input"},
		{ID: "image-call-1", Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "reused", Name: "read"}, {ID: "text", Name: "read"}}},
		{ID: "image-1", Role: RoleTool, ToolCallID: "reused", ToolName: "read", ReceiptOutcome: "success", ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: imageURL, Detail: "high"}}}},
		{ID: "text-1", Role: RoleTool, ToolCallID: "text", ToolName: "read", ToolResult: "same exchange text", ReceiptOutcome: "success"},
		{ID: "old-call", Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "old", Name: "read"}}},
		{ID: "old-text", Role: RoleTool, ToolCallID: "old", ToolName: "read", ToolResult: strings.Repeat("large old diagnostic ", 200), ReceiptOutcome: "success"},
		{ID: "image-call-2", Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "reused", Name: "read"}}},
		{ID: "image-2", Role: RoleTool, ToolCallID: "reused", ToolName: "read", ReceiptOutcome: "success", ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: imageURL, Detail: "low"}}}},
		{ID: "newest", Role: RoleAssistant, Content: "latest diagnostic"},
	}
	for i := 1; i < len(items); i++ {
		items[i].Runtime = llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "synthetic-image-run", TranscriptIndex: uint64(i - 1)}, ReceiptOutcome: items[i].ReceiptOutcome}
	}
	return items
}

func TestImageOmissionRetainsCompleteSourceQualifiedGroups(t *testing.T) {
	items := imageOmissionFixture(t)
	require.NoError(t, ValidateCompactionEligible(items))
	const budget = 600
	require.Greater(t, EstimateTokens(items, runeCountEstimator{}), budget)
	result, err := NewTruncatingCompactor().Compact(t.Context(), items, budget, runeCountEstimator{})
	require.NoError(t, err)
	require.NotNil(t, result.NewProjection)
	require.NoError(t, ValidateRunRequestIntegrity(result.Items))
	require.NoError(t, ValidateImageGroupRetention(items, result.Items))
	require.LessOrEqual(t, EstimateTokens(result.Items, runeCountEstimator{}), budget)
	var ids []string
	for _, item := range result.Items {
		ids = append(ids, item.ID)
	}
	require.Equal(t, []string{"system", "current", "", "image-call-1", "image-1", "text-1", "image-call-2", "image-2", "newest"}, ids)
}

func TestImageOmissionRoutesWithoutAuxiliarySummary(t *testing.T) {
	items := imageOmissionFixture(t)
	summarizer := &imageGroupSummarizer{}
	router := NewUnifiedCompactor(WithSummary(NewSummaryCompactor(summarizer)))
	result, err := router.Compact(t.Context(), items, 600, runeCountEstimator{})
	require.NoError(t, err)
	require.Zero(t, summarizer.calls)
	require.NotEmpty(t, result.NewProjection.RetainedSourceRanges)
	require.NoError(t, ValidateRetainedOmission(items, result.Items, result.NewProjection))
	_, err = router.Compact(t.Context(), items, 20, runeCountEstimator{})
	require.ErrorIs(t, err, ErrImageCompactionBlocked)
	require.Zero(t, summarizer.calls, "an impossible protected floor must not invoke a summary model")
}

func TestImageOmissionRejectsForgedAndPartialRanges(t *testing.T) {
	items := imageOmissionFixture(t)
	result, err := NewTruncatingCompactor().Compact(t.Context(), items, 600, runeCountEstimator{})
	require.NoError(t, err)
	for _, change := range []func(*CompactionResult){
		func(r *CompactionResult) { r.NewProjection.RetainedSourceRanges[0].Digest = strings.Repeat("f", 64) },
		func(r *CompactionResult) { r.NewProjection.RetainedSourceRanges[0].Start.RunID = "foreign" },
		func(r *CompactionResult) {
			r.NewProjection.RetainedSourceRanges[0].End = r.NewProjection.RetainedSourceRanges[0].Start
		},
		func(r *CompactionResult) {
			r.NewProjection.RetainedSourceRanges = append(r.NewProjection.RetainedSourceRanges, r.NewProjection.RetainedSourceRanges[0])
		},
		func(r *CompactionResult) { r.Items = append(r.Items[:4], r.Items[5:]...) },
		func(r *CompactionResult) { r.Items[3], r.Items[6] = r.Items[6], r.Items[3] },
		func(r *CompactionResult) { r.Items[len(r.Items)-1].Content = "changed" },
	} {
		candidate := result
		candidate.Items = append([]TurnItem(nil), result.Items...)
		projection := *result.NewProjection
		projection.RetainedSourceRanges = append([]llm.RetainedSourceRange(nil), projection.RetainedSourceRanges...)
		candidate.NewProjection = &projection
		change(&candidate)
		require.Error(t, ValidateRetainedOmission(items, candidate.Items, candidate.NewProjection))
	}
}

func TestImageOmissionSupportsSecondCompaction(t *testing.T) {
	items := imageOmissionFixture(t)
	first, err := NewTruncatingCompactor().Compact(t.Context(), items, 600, runeCountEstimator{})
	require.NoError(t, err)
	next := append([]TurnItem(nil), first.Items...)
	for i, item := range []TurnItem{
		{ID: "new-old", Role: RoleUser, Content: strings.Repeat("another diagnostic ", 500)},
		{ID: "last-image-call", Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "reused", Name: "read"}}},
		{ID: "last-image", Role: RoleTool, ToolCallID: "reused", ToolName: "read", ReceiptOutcome: "success", ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://fixture.invalid/last.png", Detail: "high"}}}},
	} {
		item.Runtime = llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunTranscript, RunID: "synthetic-image-run", TranscriptIndex: uint64(9 + i)}, ReceiptOutcome: item.ReceiptOutcome}
		next = append(next, item)
	}
	require.NoError(t, ValidateRunRequestIntegrity(next))
	second, err := NewTruncatingCompactor().Compact(t.Context(), next, 700, runeCountEstimator{})
	require.NoError(t, err)
	require.NoError(t, ValidateRetainedOmission(next, second.Items, second.NewProjection))
	require.NoError(t, ValidateImageGroupRetention(next, second.Items))
	require.Greater(t, second.NewProjection.Through.TranscriptCount, first.NewProjection.Through.TranscriptCount)
}

func TestImageOmissionRejectsUnsupportedModality(t *testing.T) {
	items := imageOmissionFixture(t)
	items[3].ContentBlocks = append(items[3].ContentBlocks, llm.ContentBlock{Type: "audio", Text: "unsupported"})
	summarizer := &imageGroupSummarizer{}
	_, err := NewUnifiedCompactor(WithSummary(NewSummaryCompactor(summarizer))).Compact(t.Context(), items, 600, runeCountEstimator{})
	require.ErrorIs(t, err, ErrImageCompactionBlocked)
	require.Zero(t, summarizer.calls)
}

func TestImageOmissionDoesNotHideLatestToolReceipt(t *testing.T) {
	fixture := imageOmissionFixture(t)
	items := append([]TurnItem(nil), fixture[:5]...)
	items = append(items, fixture[7:]...)
	items = append(items, fixture[5:7]...)
	for i := 1; i < len(items); i++ {
		items[i].Runtime.Source.TranscriptIndex = uint64(i - 1)
	}
	require.NoError(t, ValidateRunRequestIntegrity(items))
	_, err := NewUnifiedCompactor().Compact(t.Context(), items, 600, runeCountEstimator{})
	require.ErrorIs(t, err, ErrImageCompactionBlocked, "the latest tool result must remain available to the next provider request")
}

func TestImageOmissionValidatorIndependentlyRejectsDroppedLatestReceipt(t *testing.T) {
	fixture := imageOmissionFixture(t)
	items := append([]TurnItem(nil), fixture[:5]...)
	items = append(items, fixture[7:]...)
	items = append(items, fixture[5:7]...)
	for i := 1; i < len(items); i++ {
		items[i].Runtime.Source.TranscriptIndex = uint64(i - 1)
	}
	var ranges []llm.RetainedSourceRange
	for _, members := range [][]TurnItem{items[2:5], items[5:7]} {
		digest, err := RetainedExchangeDigest(members)
		require.NoError(t, err)
		ranges = append(ranges, llm.RetainedSourceRange{Start: members[0].Runtime.Source, End: members[len(members)-1].Runtime.Source, Digest: digest})
	}
	encoded, err := EncodeRetainedSourceRanges(ranges)
	require.NoError(t, err)
	through := llm.SourceThrough{TranscriptCount: uint64(len(items) - 1)}
	marker := TurnItem{Role: RoleUser, Content: DurableTruncationMarker, IsCompaction: true, Runtime: llm.RuntimeMetadata{CompactionMarker: true, Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "synthetic-image-run", Through: through, RetainCurrentInput: true, RetainedSourceRanges: encoded}}}
	after := append([]TurnItem{items[0], items[1], marker}, items[2:7]...)
	projection := &NewProjection{Summary: DurableTruncationMarker, Through: through, RetainCurrentInput: true, RetainedSourceRanges: ranges}
	require.NoError(t, ValidateRunRequestIntegrity(after))
	require.NoError(t, ValidateImageGroupRetention(items, after))
	require.ErrorContains(t, ValidateRetainedOmission(items, after, projection), "latest complete tool exchange")
}
