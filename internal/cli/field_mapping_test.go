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
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "read", Args: map[string]any{"path": "/tmp"}},
			},
			ContentBlocks: []llm.ContentBlock{
				{Type: "text", Text: "block"},
			},
		},
		{
			Role:       "tool",
			Content:    "tool result",
			ToolCallID: "call-1",
			ToolName:   "read",
		},
	}

	items := messagesToTurnItems(msgs)
	require.Len(t, items, 2)

	// Assistant message with ToolCalls and ContentBlocks.
	assert.Equal(t, "assistant", items[0].Role)
	require.Len(t, items[0].ToolCalls, 1)
	assert.Equal(t, "call-1", items[0].ToolCalls[0].ID)
	assert.Equal(t, "read", items[0].ToolCalls[0].Name)
	require.Len(t, items[0].ContentBlocks, 1)
	assert.Equal(t, "block", items[0].ContentBlocks[0].Text)

	// Tool message with ToolCallID and ToolName.
	assert.Equal(t, "tool", items[1].Role)
	assert.Equal(t, "call-1", items[1].ToolCallID)
	assert.Equal(t, "read", items[1].ToolName)
}

// TestTurnItemsToMessages_RoundTrip verifies that AgentMessage -> TurnItem ->
// AgentMessage preserves ContentBlocks and ToolCalls.
func TestTurnItemsToMessages_RoundTrip(t *testing.T) {
	original := []core.AgentMessage{
		{
			Role:    "assistant",
			Content: "calling tool",
			ToolCalls: []llm.ToolCall{
				{ID: "tc-1", Name: "write"},
			},
			ContentBlocks: []llm.ContentBlock{
				{Type: "text", Text: "cb-text"},
			},
		},
		{
			Role:       "tool",
			Content:    "result",
			ToolCallID: "tc-1",
			ToolName:   "write",
		},
	}

	items := messagesToTurnItems(original)
	result := turnItemsToMessages(items)
	require.Len(t, result, 2)

	// Assistant fields preserved.
	assert.Equal(t, original[0].Role, result[0].Role)
	assert.Equal(t, original[0].Content, result[0].Content)
	require.Len(t, result[0].ToolCalls, 1)
	assert.Equal(t, "tc-1", result[0].ToolCalls[0].ID)
	assert.Equal(t, "write", result[0].ToolCalls[0].Name)
	require.Len(t, result[0].ContentBlocks, 1)
	assert.Equal(t, "cb-text", result[0].ContentBlocks[0].Text)

	// Tool fields preserved.
	assert.Equal(t, "tool", result[1].Role)
	assert.Equal(t, "tc-1", result[1].ToolCallID)
	assert.Equal(t, "write", result[1].ToolName)
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
		{
			Role:    "assistant",
			Content: "I will call a tool",
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "read"},
			},
		},
	}

	result, err := adapter.Compact(context.Background(), msgs, 10000)
	require.NoError(t, err)
	require.Len(t, result, 1)

	assert.Equal(t, "assistant", result[0].Role)
	require.Len(t, result[0].ToolCalls, 1)
	assert.Equal(t, "call-1", result[0].ToolCalls[0].ID)
	assert.Equal(t, "read", result[0].ToolCalls[0].Name)
}
