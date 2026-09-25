package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestGroupTurnItemsUsesCurrentAssistantCallScope(t *testing.T) {
	items := []TurnItem{
		{
			Role: RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "reused", Name: "read"},
				{ID: "second", Name: "grep"},
			},
		},
		{Role: RoleTool, ToolCallID: "second", ToolName: "grep", ToolResult: "grep result"},
		{Role: RoleTool, ToolCallID: "reused", ToolName: "read", ToolResult: "read result"},
		{Role: RoleUser, Content: "next turn"},
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "reused", Name: "write"}}},
		{Role: RoleTool, ToolCallID: "reused", ToolName: "write", ToolResult: "write result"},
	}

	normalized, groups, err := groupTurnItems(items)
	require.NoError(t, err, "closed groups may reuse a provider ToolCallID")
	assert.Equal(t, items, normalized)
	require.Len(t, groups, 3)
	assert.Equal(t, itemGroup{start: 0, end: 3}, groups[0])
	assert.Equal(t, itemGroup{start: 3, end: 4}, groups[1])
	assert.Equal(t, itemGroup{start: 4, end: 6}, groups[2])
}

func TestGroupTurnItemsFillsOnlyMissingReceiptNameOnCopy(t *testing.T) {
	args := map[string]any{"path": "/tmp/example"}
	items := []TurnItem{
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "read-1", Name: "read", Args: args}}},
		{Role: RoleTool, ToolCallID: "read-1", ToolResult: "contents"},
	}

	normalized, groups, err := groupTurnItems(items)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	assert.Equal(t, "read", normalized[1].ToolName)
	assert.Empty(t, items[1].ToolName, "helper must not normalize the caller's input")
	assert.Equal(t, args, normalized[0].ToolCalls[0].Args, "nested call arguments remain unchanged")
	assert.Equal(t, "read", normalized[0].ToolCalls[0].Name)
}

func TestGroupTurnItemsDemotesLegacyCompactionSystemOnCopy(t *testing.T) {
	items := []TurnItem{
		{Role: RoleSystem, Content: "legacy summary", IsCompaction: true},
		{Role: RoleSystem, Content: "mandatory instructions"},
	}

	normalized, groups, err := groupTurnItems(items)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.Equal(t, RoleUser, normalized[0].Role)
	assert.True(t, normalized[0].IsCompaction)
	assert.False(t, groups[0].system)
	assert.Equal(t, RoleSystem, normalized[1].Role)
	assert.True(t, groups[1].system)
	assert.Equal(t, RoleSystem, items[0].Role, "normalization must not mutate the caller's slice")
}

func TestGroupTurnItemsRejectsMalformedClosure(t *testing.T) {
	tests := []struct {
		name  string
		items []TurnItem
	}{
		{
			name:  "empty call id",
			items: []TurnItem{{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{Name: "read"}}}},
		},
		{
			name:  "empty call name",
			items: []TurnItem{{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1"}}}},
		},
		{
			name: "duplicate call ids in group",
			items: []TurnItem{{Role: RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "read"}, {ID: "call-1", Name: "grep"},
			}}},
		},
		{
			name:  "orphan receipt at start",
			items: []TurnItem{{Role: RoleTool, ToolCallID: "call-1", ToolName: "read"}},
		},
		{
			name:  "missing receipt at end",
			items: []TurnItem{{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}}},
		},
		{
			name: "missing receipt before next message",
			items: []TurnItem{
				{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
				{Role: RoleUser, Content: "next"},
			},
		},
		{
			name: "unknown id inside group",
			items: []TurnItem{
				{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
				{Role: RoleTool, ToolCallID: "other", ToolName: "read"},
			},
		},
		{
			name: "duplicate receipt inside parallel group",
			items: []TurnItem{
				{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}, {ID: "call-2", Name: "grep"}}},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "read"},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "read"},
			},
		},
		{
			name: "conflicting tool name",
			items: []TurnItem{
				{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "write"},
			},
		},
		{
			name: "empty receipt id",
			items: []TurnItem{
				{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
				{Role: RoleTool, ToolName: "read"},
			},
		},
		{
			name: "extra receipt after group closure",
			items: []TurnItem{
				{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "read"},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "read"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := groupTurnItems(tt.items)
			require.ErrorIs(t, err, errMalformedToolGroup)
		})
	}
}

func TestTruncatingCompactorRejectsMalformedToolGroups(t *testing.T) {
	tests := []struct {
		name  string
		items []TurnItem
	}{
		{
			name: "orphan receipt",
			items: []TurnItem{{
				Role: RoleTool, ToolCallID: "stray", ToolName: "read", ToolResult: "result",
			}},
		},
		{
			name: "missing receipt",
			items: []TurnItem{{
				Role:      RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}},
			}},
		},
		{
			name: "duplicate call id",
			items: []TurnItem{{
				Role:      RoleAssistant,
				ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}, {ID: "call-1", Name: "grep"}},
			}},
		},
		{
			name: "duplicate receipt",
			items: []TurnItem{
				{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: "first"},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: "second"},
			},
		},
		{
			name: "conflicting tool name",
			items: []TurnItem{
				{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "write", ToolResult: "result"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer verify.AssertNoGoroutineLeak(t)()
			ctx, _ := newTracedCtx(t)
			_, err := NewTruncatingCompactor().Compact(ctx, tt.items, 1000, tracedEstimator())
			require.Error(t, err)
		})
	}
}

func TestTruncatingCompactorKeepsOrDropsToolGroupAtomically(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()
	items := []TurnItem{
		{
			Role: RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "read"},
				{ID: "call-2", Name: "grep"},
			},
		},
		// Receipts are deliberately in the reverse order from the calls.
		{Role: RoleTool, ToolCallID: "call-2", ToolName: "grep", ToolResult: "bbbb"},
		{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: "aaaa"},
		{Role: RoleUser, Content: "new!"},
	}

	t.Run("drop the complete group when only one receipt could fit", func(t *testing.T) {
		out, err := NewTruncatingCompactor().Compact(ctx, items, 2, est)
		require.NoError(t, err)
		require.Len(t, out, 1)
		assert.Equal(t, RoleUser, out[0].Role)
		assert.Equal(t, "new!", out[0].Content)
	})

	t.Run("retain the complete group when it fits", func(t *testing.T) {
		out, err := NewTruncatingCompactor().Compact(ctx, items, 3, est)
		require.NoError(t, err)
		require.Len(t, out, len(items))
		assert.Equal(t, items, out)
	})
}

func TestTruncatingCompactorFillsUniqueMissingReceiptToolNameWithoutMutation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newTracedCtx(t)
	items := []TurnItem{
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-read", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-read", ToolResult: "contents"},
	}

	out, err := NewTruncatingCompactor().Compact(ctx, items, 100, tracedEstimator())
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, "read", out[1].ToolName)
	assert.Empty(t, items[1].ToolName, "normalization must not mutate the input slice")
}

func TestTruncatingCompactorAllowsCallIDReuseAfterGroupClosure(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newTracedCtx(t)
	items := []TurnItem{
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: "first"},
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "grep"}}},
		{Role: RoleTool, ToolCallID: "call-1", ToolName: "grep", ToolResult: "second"},
	}

	out, err := NewTruncatingCompactor().Compact(ctx, items, 100, tracedEstimator())
	require.NoError(t, err)
	assert.Equal(t, items, out)
}

func TestSummaryCompactorRejectsMalformedGroupBeforeCallingSummarizer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newTracedCtx(t)
	summarizer := &fakeSummarizer{summary: "summary"}
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("old ", 100)},
		{Role: RoleTool, ToolCallID: "orphan", ToolName: "read", ToolResult: "orphan result"},
		{Role: RoleUser, Content: "recent"},
	}

	_, err := NewSummaryCompactor(summarizer).Compact(ctx, items, 10, tracedEstimator())
	require.Error(t, err)
	assert.Empty(t, summarizer.conversations(), "invalid history must fail before any summarizer call")
}

func TestSummaryCompactorCutsOnlyAtClosedToolGroupBoundaries(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, _ := newTracedCtx(t)
	summarizer := &fakeSummarizer{summary: "summary"}
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("old ", 100)},
		{
			Role: RoleAssistant,
			ToolCalls: []llm.ToolCall{
				{ID: "call-1", Name: "read"},
				{ID: "call-2", Name: "grep"},
			},
		},
		{Role: RoleTool, ToolCallID: "call-2", ToolName: "grep", ToolResult: "grep-result"},
		{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: "read-result"},
		{Role: RoleUser, Content: "recent"},
	}

	out, err := NewSummaryCompactor(summarizer).Compact(ctx, items, 120, tracedEstimator())
	require.NoError(t, err)
	require.NotEmpty(t, out)
	assert.Equal(t, RoleUser, out[0].Role, "derived summary is projected as user-role context")
	assert.True(t, out[0].IsCompaction)
	assert.Empty(t, out[0].ToolCalls)
	assert.Empty(t, out[0].ToolCallID)

	keptCalls := 0
	keptReceipts := 0
	for _, item := range out {
		if item.Role == RoleAssistant && len(item.ToolCalls) > 0 {
			keptCalls += len(item.ToolCalls)
		}
		if item.Role == RoleTool {
			keptReceipts++
		}
	}
	assert.True(t, keptCalls == 0 && keptReceipts == 0 || keptCalls == 2 && keptReceipts == 2,
		"summary projection must replace or retain the whole two-call group: calls=%d receipts=%d", keptCalls, keptReceipts)
}
