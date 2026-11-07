package core

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestDurableImageCompactionRejectsCoveredImageBeforeCommit(t *testing.T) {
	snapshot, messages := durableLargeWriteFixture()
	messages[1].ToolCalls[0].Name = "read"
	messages[1].ToolCalls[0].Args = map[string]any{"path": "old.png"}
	messages[2].Name = "read"
	messages[2].ContentBlocks = []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:image/png;base64,YQ==", Detail: "high"}}}
	input := messagesToTurnItems(messages)
	through := llm.SourceThrough{PriorHistoryCount: 3}
	result := compaction.CompactionResult{
		Items:    []compaction.TurnItem{{ID: "summary", Role: "user", Content: "text summary cannot carry image", IsCompaction: true, Runtime: llm.RuntimeMetadata{Source: llm.SourceRef{Kind: llm.SourceRunSummary, RunID: "run-large-write-envelope", Through: through}}}, input[3]},
		Strategy: compaction.StrategySummary, NewProjection: &compaction.NewProjection{Summary: "text summary cannot carry image", Through: through},
	}
	sink := &compactionCommitCountingSink{}
	owner := newRunOwner(context.Background(), nil, RunClaim{ID: "run-large-write-envelope"}, nil)
	consumer := &runCommitConsumer{ctx: context.Background(), owner: owner, source: snapshot, runID: "run-large-write-envelope", sink: sink}
	_, _, err := preflightDurableRequest(context.Background(), messages, consumer, &fixedCompactionResult{result: result}, oneTokenPerNonEmptyText{}, 1, 3)
	require.ErrorContains(t, err, "protected image exchange")
	require.False(t, hasUnknownToolOutcome(err), "local capacity/projection failure must not become a tool-unknown outcome")
	require.Zero(t, sink.commits)
	require.Equal(t, "data:image/png;base64,YQ==", messages[2].ContentBlocks[0].ImageURL.URL)
	_, err = validateDurableCompactionResult(input, result, snapshot, "run-large-write-envelope")
	require.ErrorContains(t, err, "protected image exchange", "direct adoption cannot bypass the same protected boundary")
}

func TestImageCompactionCustomProjectionStopsBeforeProvider(t *testing.T) {
	model := &budgetPreflightRecordingModel{}
	loop := NewLoopAgent(WithLLM(model), WithSystemPrompt("s"))
	bad := budgetPreflightRewriteCompactor{rewrite: func(items []compaction.TurnItem) []compaction.TurnItem {
		return []compaction.TurnItem{items[0], items[len(items)-1]}
	}}
	loop.SetMidTurnCompaction(compaction.NewMidTurnCompact(compaction.WithThresholdRatio(1)), bad, budgetPreflightCharacterEstimator{}, 100)
	history := []AgentMessage{
		{Role: "user", Content: strings.Repeat("older text ", 100)},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "image-read", Name: "read", Args: map[string]any{"path": "fixture.png"}}}},
		{Role: "tool", ToolName: "read", ToolCallID: "image-read", ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:image/png;base64,YQ=="}}}},
	}
	_, err := loop.Run(t.Context(), Submission{Content: "now", History: history})
	require.ErrorContains(t, err, "protected image exchange")
	require.Empty(t, model.requestsSnapshot(), "invalid custom image projection cannot send the next provider request")
	require.Equal(t, "data:image/png;base64,YQ==", history[2].ContentBlocks[0].ImageURL.URL)
}
