package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

// TestMessagesToTurnItems_PreservesToolCalls verifies that ToolCalls,
// ToolCallID, ToolName, and ContentBlocks on an AgentMessage are preserved
// when converted to a TurnItem.
func TestMessagesToTurnItems_PreservesToolCalls(t *testing.T) {
	msgs := []core.AgentMessage{
		{
			Role:    "assistant",
			Content: "I will call a tool",
			Usage:   &llm.Usage{InputTokens: 11, OutputTokens: 3, TotalTokens: 14},
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "read", Args: map[string]any{"path": "/tmp"}},
			},
			ContentBlocks: []llm.ContentBlock{
				{Type: "text", Text: "block"},
			},
		},
		{
			Role:           "tool",
			Content:        "tool result",
			ToolCallID:     "call-1",
			ToolName:       "read",
			ReceiptOutcome: core.ReceiptOutcomeUnknown,
		},
		{Role: "user", Content: "next", InputKind: core.InputKindFollowUp},
	}

	items := messagesToTurnItems(msgs)
	require.Len(t, items, 3)

	// Assistant message with ToolCalls and ContentBlocks.
	assert.Equal(t, "assistant", items[0].Role)
	require.NotNil(t, items[0].Usage)
	assert.Equal(t, *msgs[0].Usage, *items[0].Usage)
	require.Len(t, items[0].ToolCalls, 1)
	assert.Equal(t, "call-1", items[0].ToolCalls[0].ID)
	assert.Equal(t, "read", items[0].ToolCalls[0].Name)
	require.Len(t, items[0].ContentBlocks, 1)
	assert.Equal(t, "block", items[0].ContentBlocks[0].Text)

	// Tool message with ToolCallID and ToolName.
	assert.Equal(t, "tool", items[1].Role)
	assert.Equal(t, "call-1", items[1].ToolCallID)
	assert.Equal(t, "read", items[1].ToolName)
	assert.Equal(t, core.ReceiptOutcomeUnknown, items[1].ReceiptOutcome)
	assert.Equal(t, core.InputKindFollowUp, items[2].InputKind)
}

// TestTurnItemsToMessages_RoundTrip verifies that AgentMessage -> TurnItem ->
// AgentMessage preserves ContentBlocks and ToolCalls.
func TestTurnItemsToMessages_RoundTrip(t *testing.T) {
	original := []core.AgentMessage{
		{
			Role:    "assistant",
			Content: "calling tool",
			Usage:   &llm.Usage{InputTokens: 7, OutputTokens: 2, TotalTokens: 9},
			ToolCalls: []llm.ToolCall{
				{ID: "tc-1", Name: "write", Args: map[string]any{"path": "/tmp", "nested": map[string]any{"flag": true}}},
			},
			ContentBlocks: []llm.ContentBlock{
				{Type: "text", Text: "cb-text"},
			},
		},
		{
			Role:           "tool",
			Content:        "result",
			ToolCallID:     "tc-1",
			ToolName:       "write",
			ReceiptOutcome: core.ReceiptOutcomeUnknown,
		},
		{Role: "user", Content: "follow-up", InputKind: core.InputKindFollowUp},
	}

	items := messagesToTurnItems(original)
	result := turnItemsToMessages(items)
	require.Len(t, result, 3)
	assert.Equal(t, original, result)

	// Assistant fields preserved.
	assert.Equal(t, original[0].Role, result[0].Role)
	assert.Equal(t, original[0].Content, result[0].Content)
	require.NotNil(t, result[0].Usage)
	assert.Equal(t, original[0].Usage, result[0].Usage)
	require.Len(t, result[0].ToolCalls, 1)
	assert.Equal(t, "tc-1", result[0].ToolCalls[0].ID)
	assert.Equal(t, "write", result[0].ToolCalls[0].Name)
	require.Len(t, result[0].ContentBlocks, 1)
	assert.Equal(t, "cb-text", result[0].ContentBlocks[0].Text)

	// Tool fields preserved.
	assert.Equal(t, "tool", result[1].Role)
	assert.Equal(t, "tc-1", result[1].ToolCallID)
	assert.Equal(t, "write", result[1].ToolName)
	assert.Equal(t, core.ReceiptOutcomeUnknown, result[1].ReceiptOutcome)
	assert.Equal(t, core.InputKindFollowUp, result[2].InputKind)
	assert.Equal(t, original[0].ToolCalls[0].Args, result[0].ToolCalls[0].Args)
}

// TestCompactorAdapter_PreservesToolCalls verifies that the compactor adapter
// preserves ToolCalls through compaction by converting AgentMessage ->
// TurnItem -> compaction -> TurnItem -> AgentMessage.
func TestCompactorAdapter_PreservesToolCalls(t *testing.T) {
	adapter := &compactorAdapter{
		inner:     compaction.NewUnifiedCompactor(),
		estimator: compaction.NewHeuristicTokenEstimator(),
	}

	msgs := []core.AgentMessage{
		{Role: "user", Content: "request", InputKind: core.InputKindUser},
		{
			Role:    "assistant",
			Content: "I will call a tool",
			Usage:   &llm.Usage{InputTokens: 4, OutputTokens: 1, TotalTokens: 5},
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "write", Args: map[string]any{"nested": map[string]any{"flag": true}}},
			},
			ContentBlocks: []llm.ContentBlock{{Type: "text", Text: "call"}},
		},
		{Role: "tool", Content: "unknown result", ToolCallID: "call-1", ToolName: "write", ReceiptOutcome: core.ReceiptOutcomeUnknown},
	}

	result, err := adapter.Compact(context.Background(), msgs, 10000)
	require.NoError(t, err)
	require.Len(t, result, 3)
	assert.Equal(t, msgs, result)

	assert.Equal(t, "assistant", result[1].Role)
	require.Len(t, result[1].ToolCalls, 1)
	assert.Equal(t, "call-1", result[1].ToolCalls[0].ID)
	assert.Equal(t, "write", result[1].ToolCalls[0].Name)
	require.NotNil(t, result[1].Usage)
	assert.Equal(t, *msgs[1].Usage, *result[1].Usage)
	resultArgs := result[1].ToolCalls[0].Args.(map[string]any)
	resultArgs["new"] = true
	assert.NotContains(t, msgs[1].ToolCalls[0].Args.(map[string]any), "new")
}
