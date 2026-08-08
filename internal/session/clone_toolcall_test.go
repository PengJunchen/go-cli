package session

import (
	"context"
	"strings"
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

// TestCloneRemapsToolCallID verifies that Clone remaps ToolCall IDs so the
// cloned branch does not share IDs with the source branch.
func TestCloneRemapsToolCallID(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	tree := newConcreteTree()

	require.NoError(t, tree.Append(ctx, newTestEntry("u1", "", EntryTypeUser)))
	require.NoError(t, tree.Append(ctx, newToolCallEntry("a1", "u1", []llm.ToolCall{
		{ID: "tc-1", Name: "search", Args: map[string]any{"q": "hello"}},
	})))
	require.NoError(t, tree.Append(ctx, newToolResultEntry("t1", "a1", "tc-1", "search")))
	require.NoError(t, tree.MoveTo(ctx, "t1"))

	require.NoError(t, tree.Clone(ctx, "t1", "copy"))

	// Inspect the cloned branch.
	branch, err := tree.GetBranch(ctx, "copy-t1")
	require.NoError(t, err)
	require.Len(t, branch, 3)

	// The assistant entry's ToolCall ID should be remapped.
	assistant := branch[1]
	require.Len(t, assistant.ToolCalls, 1)
	assert.NotEqual(t, "tc-1", assistant.ToolCalls[0].ID)
	assert.Contains(t, assistant.ToolCalls[0].ID, "tc-1")
	assert.Contains(t, assistant.ToolCalls[0].ID, "cloned")

	// The tool entry's ToolCallID should match the remapped ID.
	tool := branch[2]
	assert.Equal(t, assistant.ToolCalls[0].ID, tool.ToolCallID)
}

// TestCloneNoIDCollision verifies that the cloned branch's tool call IDs are
// distinct from the original branch's IDs.
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

	// IDs must differ.
	assert.NotEqual(t, origAssistant.ToolCalls[0].ID, cloneAssistant.ToolCalls[0].ID)

	// Tool result IDs must also differ.
	assert.NotEqual(t, orig[2].ToolCallID, clone[2].ToolCallID)
}

// TestCloneMultipleToolCallsRemapped verifies that multiple tool calls in a
// single assistant entry are each remapped to distinct new IDs.
func TestCloneMultipleToolCallsRemapped(t *testing.T) {
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
	require.Len(t, branch, 5)

	assistant := branch[1]
	require.Len(t, assistant.ToolCalls, 3)

	// All remapped IDs must be distinct and contain the original ID + "cloned".
	seen := make(map[string]bool, 3)
	for _, tc := range assistant.ToolCalls {
		assert.False(t, seen[tc.ID], "remapped tool call ID %q is duplicated", tc.ID)
		seen[tc.ID] = true
		assert.Contains(t, tc.ID, "cloned")
	}

	// Build a map of original ID -> remapped ID for verification.
	remapped := make(map[string]string, 3)
	for _, tc := range assistant.ToolCalls {
		for _, origID := range []string{"tc-a", "tc-b", "tc-c"} {
			if strings.Contains(tc.ID, origID) {
				remapped[origID] = tc.ID
			}
		}
	}

	// Each tool result's ToolCallID must match the corresponding remapped ID.
	for i, origTCID := range []string{"tc-a", "tc-b", "tc-c"} {
		toolEntry := branch[2+i]
		assert.Equal(t, remapped[origTCID], toolEntry.ToolCallID,
			"tool result %d ToolCallID mismatch", i)
	}
}

// TestClonePreservesToolCallContent verifies that the remapped tool calls
// preserve their Name and Args from the original.
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

	// ID changed but Name and Args are preserved.
	assert.NotEqual(t, origTC.ID, cloneTC.ID)
	assert.Equal(t, origTC.Name, cloneTC.Name)
	assert.Equal(t, origTC.Args, cloneTC.Args)

	// Tool result preserves ToolName.
	assert.Equal(t, orig[2].ToolName, clone[2].ToolName)
	assert.Equal(t, "search", clone[2].ToolName)
}
