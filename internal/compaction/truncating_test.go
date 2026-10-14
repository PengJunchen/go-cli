package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestTruncatingCompactorKeepsSystem(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()

	system := TurnItem{
		ID: "system", ParentID: "root", Role: RoleSystem,
		Content: "sys-prompt", FinishReason: "configured", InputKind: "policy",
	}
	items := []TurnItem{
		system,
		{Role: RoleUser, Content: strings.Repeat("u", 200)},
		{Role: RoleAssistant, Content: strings.Repeat("a", 200)},
		{Role: RoleUser, Content: strings.Repeat("v", 200)},
	}

	compactor := NewTruncatingCompactor()
	outResult, err := compactor.Compact(ctx, items, 40, est)
	out := outResult.Items
	require.NoError(t, err)

	// The system entry is always kept.
	require.NotEmpty(t, out)
	assert.Equal(t, "sys-prompt", out[0].Content)
	assert.Equal(t, RoleSystem, out[0].Role)
	assert.Equal(t, system, out[0], "truncation must preserve the original system item metadata")

	// No user/assistant entries should be retained beyond budget.
	assert.LessOrEqual(t, estimateTokens(out, est), 40)

	assertSpanEventually(t, exp, "compaction.truncating")
}

func TestTruncatingCompactorTruncatesOldestNonSystem(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	// Newest user entries are small; both must be kept, none dropped.
	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: strings.Repeat("old", 100)},
		{Role: RoleUser, Content: "newest-1"},
		{Role: RoleUser, Content: "newest-2"},
	}

	compactor := NewTruncatingCompactor()
	outResult, err := compactor.Compact(ctx, items, 20, est)
	out := outResult.Items
	require.NoError(t, err)

	// The oldest large entry is dropped; the newest ones remain, ordered by recency.
	contents := make([]string, len(out))
	for i, it := range out {
		contents[i] = it.Content
	}
	assert.Equal(t, "sys", contents[0])
	assert.Contains(t, contents, "newest-1")
	assert.Contains(t, contents, "newest-2")
	assert.NotContains(t, contents, strings.Repeat("old", 100))
}

func TestTruncatingCompactorCompressesToBudget(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	var items []TurnItem
	for i := 0; i < 60; i++ {
		items = append(items, TurnItem{Role: RoleUser, Content: strings.Repeat("m", 40)})
	}

	compactor := NewTruncatingCompactor()
	outResult, err := compactor.Compact(ctx, items, 50, est)
	out := outResult.Items
	require.NoError(t, err)
	assert.LessOrEqual(t, estimateTokens(out, est), 50)
	assert.NotEmpty(t, out)
}

func TestTruncatingCompactorEmptyInputNoPanic(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	compactor := NewTruncatingCompactor()
	outResult, err := compactor.Compact(ctx, nil, 100, est)
	out := outResult.Items
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestTruncatingCompactorRejectsMandatorySystemOverBudget(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleSystem, Content: strings.Repeat("s", 400)},
		{Role: RoleUser, Content: "tiny"},
	}

	compactor := NewTruncatingCompactor()
	_, err := compactor.Compact(ctx, items, 20, est)
	require.ErrorIs(t, err, ErrMandatorySystemPromptOverBudget)
}

func TestTruncatingCompactorPreservesChronologicalOrder(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	// Items are in chronological order: oldest first, newest last. Each is
	// small enough to fit a generous budget, so all should be retained.
	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "first"},
		{Role: RoleAssistant, Content: "second"},
		{Role: RoleUser, Content: "third"},
		{Role: RoleAssistant, Content: "fourth"},
	}

	compactor := NewTruncatingCompactor()
	outResult, err := compactor.Compact(ctx, items, 200, est)
	out := outResult.Items
	require.NoError(t, err)

	// The non-system entries must remain in their original chronological
	// order. A reverse-order bug would surface as newest-first.
	expected := []string{"sys", "first", "second", "third", "fourth"}
	actual := make([]string, len(out))
	for i, it := range out {
		actual[i] = it.Content
	}
	assert.Equal(t, expected, actual)
}

func TestTruncatingCompactorDropsOldestKeepsOrder(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	// The first non-system entry is large so it is dropped; the remaining
	// entries must still be in chronological order.
	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: strings.Repeat("old", 100)},
		{Role: RoleUser, Content: "keep-a"},
		{Role: RoleAssistant, Content: "keep-b"},
		{Role: RoleUser, Content: "keep-c"},
	}

	compactor := NewTruncatingCompactor()
	outResult, err := compactor.Compact(ctx, items, 30, est)
	out := outResult.Items
	require.NoError(t, err)

	expected := []string{"sys", "keep-a", "keep-b", "keep-c"}
	actual := make([]string, len(out))
	for i, it := range out {
		actual[i] = it.Content
	}
	assert.Equal(t, expected, actual)
}

func TestTruncatingCompactorCompileGuard(t *testing.T) {
	var _ Compactor = (*TruncatingCompactor)(nil)
	assert.NotNil(t, NewTruncatingCompactor())
}

func TestTruncatingCompactorRejectsDanglingToolResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: strings.Repeat("a", 200)},
		{Role: RoleTool, ToolCallID: "orphan", ToolName: "read", ToolResult: "tr1"},
		{Role: RoleUser, Content: "u2"},
		{Role: RoleAssistant, Content: "a2"},
	}

	compactor := NewTruncatingCompactor()
	_, err := compactor.Compact(ctx, items, 30, est)
	require.ErrorIs(t, err, errMalformedToolGroup, "malformed history must be rejected instead of dropping its receipt")
}

func TestTruncatingCompactorKeepsPairedToolResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: "a1", ToolCalls: []llm.ToolCall{{ID: "call-1", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: "tr1"},
	}

	compactor := NewTruncatingCompactor()
	outResult, err := compactor.Compact(ctx, items, 200, est)
	out := outResult.Items
	require.NoError(t, err)

	roles := make([]string, len(out))
	for i, it := range out {
		roles[i] = it.Role
	}
	assert.Equal(t, []string{RoleSystem, RoleUser, RoleAssistant, RoleTool}, roles)
}

func TestGroupTurnItemsRejectsUnpairedLegacyToolItems(t *testing.T) {
	tests := []struct {
		name  string
		input []TurnItem
	}{
		{
			name: "tool at start",
			input: []TurnItem{
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: "x"},
				{Role: RoleUser, Content: "u"},
			},
		},
		{
			name: "tool after user",
			input: []TurnItem{
				{Role: RoleUser, Content: "u"},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: "x"},
				{Role: RoleUser, Content: "v"},
			},
		},
		{
			name: "tool after plain assistant",
			input: []TurnItem{
				{Role: RoleUser, Content: "u"},
				{Role: RoleAssistant, Content: "a"},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "read", ToolResult: "x"},
			},
		},
		{
			name: "consecutive tools without assistant calls",
			input: []TurnItem{
				{Role: RoleUser, Content: "u"},
				{Role: RoleAssistant, Content: "a"},
				{Role: RoleTool, ToolCallID: "call-1", ToolName: "bash", ToolResult: "x"},
				{Role: RoleTool, ToolCallID: "call-2", ToolName: "read", ToolResult: "y"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := groupTurnItems(tt.input)
			require.ErrorIs(t, err, errMalformedToolGroup)
		})
	}
}
