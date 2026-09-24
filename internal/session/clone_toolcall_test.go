package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// newToolCallEntry builds an assistant entry with the given tool calls.
func newToolCallEntry(id, parentID string, toolCalls []llm.ToolCall) *SessionEntry {
	return &SessionEntry{
		ID:        id,
		ParentID:  parentID,
		Type:      EntryTypeAssistant,
		Content:   "content-" + id,
		ToolCalls: toolCalls,
		Timestamp: time.Date(2024, 5, 1, 12, 0, int(id[0]), 0, time.UTC),
	}
}

// newToolResultEntry builds a tool-result entry referencing the given tool call id.
func newToolResultEntry(id, parentID, toolCallID, toolName string) *SessionEntry {
	return &SessionEntry{
		ID:         id,
		ParentID:   parentID,
		Type:       EntryTypeTool,
		Content:    "content-" + id,
		ToolCallID: toolCallID,
		ToolName:   toolName,
		Timestamp:  time.Date(2024, 5, 1, 12, 0, int(id[0]), 0, time.UTC),
	}
}

// TestCloneSharesCanonicalPrefixWithoutRewritingToolCallIDs verifies a branch
// clone reuses the canonical tool-call group and preserves all identities.
func TestCloneSharesCanonicalPrefixWithoutRewritingToolCallIDs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()

	require.NoError(t, tree.Append(ctx, newTestEntry("u1", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newToolCallEntry("a1", "u1", []llm.ToolCall{
		{ID: "tc-1", Name: "search", Args: map[string]any{"q": "hello"}},
	})))
	toolReceipt := newToolResultEntry("t1", "a1", "tc-1", "search")
	toolReceipt.ActionID = "action-1"
	require.NoError(t, tree.Append(ctx, toolReceipt))
	require.NoError(t, tree.MoveTo(ctx, "t1"))

	require.NoError(t, tree.Clone(ctx, "t1", "copy"))

	// Inspect the cloned branch. The source rows are shared and only one
	// checkpoint is appended.
	branch, err := tree.GetBranch(ctx, "copy-t1")
	require.NoError(t, err)
	require.Len(t, branch, 4)

	// The assistant entry's provider ToolCallID remains unchanged.
	assistant := branch[1]
	require.Len(t, assistant.ToolCalls, 1)
	assert.Equal(t, "tc-1", assistant.ToolCalls[0].ID)

	// The tool entry's ToolCallID should match the remapped ID.
	tool := branch[2]
	assert.Equal(t, "tc-1", tool.ToolCallID)
	assert.Equal(t, "action-1", tool.ActionID)
	original, err := tree.GetBranch(ctx, "t1")
	require.NoError(t, err)
	assert.Same(t, original[1], branch[1])
	assert.Same(t, original[2], branch[2])
}

// TestCloneNoIDCollision verifies the cloned view references the same
// canonical provider ID and source rows instead of manufacturing another ID.
func TestCloneNoIDCollision(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()

	require.NoError(t, tree.Append(ctx, newTestEntry("u1", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newToolCallEntry("a1", "u1", []llm.ToolCall{
		{ID: "call-abc", Name: "calc", Args: nil},
	})))
	require.NoError(t, tree.Append(ctx, newToolResultEntry("t1", "a1", "call-abc", "calc")))
	require.NoError(t, tree.MoveTo(ctx, "t1"))

	require.NoError(t, tree.Clone(ctx, "t1", "dup"))

	orig, err := tree.GetBranch(ctx, "t1")
	require.NoError(t, err)
	clone, err := tree.GetBranch(ctx, "dup-t1")
	require.NoError(t, err)

	origAssistant := orig[1]
	cloneAssistant := clone[1]
	require.Len(t, origAssistant.ToolCalls, 1)
	require.Len(t, cloneAssistant.ToolCalls, 1)

	// Both branches share the one canonical tool-call occurrence.
	assert.Equal(t, origAssistant.ToolCalls[0].ID, cloneAssistant.ToolCalls[0].ID)

	assert.Equal(t, orig[2].ToolCallID, clone[2].ToolCallID)
	assert.Same(t, origAssistant, cloneAssistant)
}

// TestCloneMultipleToolCallsPreserved verifies multiple provider IDs remain
// intact when a branch shares the canonical assistant row.
func TestCloneMultipleToolCallsPreserved(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()

	require.NoError(t, tree.Append(ctx, newTestEntry("u1", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newToolCallEntry("a1", "u1", []llm.ToolCall{
		{ID: "tc-a", Name: "search", Args: nil},
		{ID: "tc-b", Name: "calc", Args: nil},
		{ID: "tc-c", Name: "read", Args: nil},
	})))
	require.NoError(t, tree.Append(ctx, newToolResultEntry("t1", "a1", "tc-a", "search")))
	require.NoError(t, tree.Append(ctx, newToolResultEntry("t2", "t1", "tc-b", "calc")))
	require.NoError(t, tree.Append(ctx, newToolResultEntry("t3", "t2", "tc-c", "read")))
	require.NoError(t, tree.MoveTo(ctx, "t3"))

	require.NoError(t, tree.Clone(ctx, "t3", "copy"))

	branch, err := tree.GetBranch(ctx, "copy-t3")
	require.NoError(t, err)
	require.Len(t, branch, 6)

	assistant := branch[1]
	require.Len(t, assistant.ToolCalls, 3)

	// All original provider IDs remain distinct and unchanged.
	seen := make(map[string]bool, 3)
	for i, tc := range assistant.ToolCalls {
		assert.False(t, seen[tc.ID], "remapped tool call ID %q is duplicated", tc.ID)
		seen[tc.ID] = true
		assert.Equal(t, []string{"tc-a", "tc-b", "tc-c"}[i], tc.ID)
	}

	// Each tool result's ToolCallID must match the corresponding remapped ID.
	for i, origTCID := range []string{"tc-a", "tc-b", "tc-c"} {
		toolEntry := branch[2+i]
		assert.Equal(t, origTCID, toolEntry.ToolCallID, "tool result %d ToolCallID changed", i)
	}
}

// TestClonePreservesToolCallContent verifies shared tool calls retain their
// provider ID, Name, and Args.
func TestClonePreservesToolCallContent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()

	args := map[string]any{"query": "golang tests", "limit": float64(10)}
	require.NoError(t, tree.Append(ctx, newTestEntry("u1", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newToolCallEntry("a1", "u1", []llm.ToolCall{
		{ID: "tc-1", Name: "search", Args: args},
	})))
	require.NoError(t, tree.Append(ctx, newToolResultEntry("t1", "a1", "tc-1", "search")))
	require.NoError(t, tree.MoveTo(ctx, "t1"))

	require.NoError(t, tree.Clone(ctx, "t1", "copy"))

	orig, err := tree.GetBranch(ctx, "t1")
	require.NoError(t, err)
	clone, err := tree.GetBranch(ctx, "copy-t1")
	require.NoError(t, err)

	origTC := orig[1].ToolCalls[0]
	cloneTC := clone[1].ToolCalls[0]

	assert.Equal(t, origTC.ID, cloneTC.ID)
	assert.Equal(t, origTC.Name, cloneTC.Name)
	assert.Equal(t, origTC.Args, cloneTC.Args)

	// Tool result preserves ToolName.
	assert.Equal(t, orig[2].ToolName, clone[2].ToolName)
	assert.Equal(t, "search", clone[2].ToolName)
}
