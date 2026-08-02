package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestTruncatingCompactorKeepsSystem(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys-prompt"},
		{Role: RoleUser, Content: strings.Repeat("u", 200)},
		{Role: RoleAssistant, Content: strings.Repeat("a", 200)},
		{Role: RoleUser, Content: strings.Repeat("v", 200)},
	}

	compactor := NewTruncatingCompactor()
	out, err := compactor.Compact(ctx, items, 40, est)
	require.NoError(t, err)

	// The system entry is always kept.
	require.NotEmpty(t, out)
	assert.Equal(t, "sys-prompt", out[0].Content)
	assert.Equal(t, RoleSystem, out[0].Role)

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
	out, err := compactor.Compact(ctx, items, 20, est)
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
	out, err := compactor.Compact(ctx, items, 50, est)
	require.NoError(t, err)
	assert.LessOrEqual(t, estimateTokens(out, est), 50)
	assert.NotEmpty(t, out)
}

func TestTruncatingCompactorEmptyInputNoPanic(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	compactor := NewTruncatingCompactor()
	out, err := compactor.Compact(ctx, nil, 100, est)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestTruncatingCompactorGracefulWhenSystemOverBudget(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleSystem, Content: strings.Repeat("s", 400)},
		{Role: RoleUser, Content: "tiny"},
	}

	compactor := NewTruncatingCompactor()
	out, err := compactor.Compact(ctx, items, 20, est)
	require.NoError(t, err)
	// Degrades gracefully: system entries are all we can keep.
	assert.Equal(t, 1, len(out))
	assert.Equal(t, RoleSystem, out[0].Role)
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
	out, err := compactor.Compact(ctx, items, 200, est)
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
	out, err := compactor.Compact(ctx, items, 30, est)
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

func TestTruncatingCompactorDropsDanglingToolResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: strings.Repeat("a", 200)},
		{Role: RoleTool, ToolName: "read", ToolResult: "tr1"},
		{Role: RoleUser, Content: "u2"},
		{Role: RoleAssistant, Content: "a2"},
	}

	compactor := NewTruncatingCompactor()
	out, err := compactor.Compact(ctx, items, 30, est)
	require.NoError(t, err)

	for i, it := range out {
		if it.Role == RoleTool {
			assert.NotEqual(t, RoleSystem, out[i-1].Role,
				"dangling tool result: RoleTool at index %d preceded by %s, not RoleAssistant", i, out[i-1].Role)
			assert.NotEqual(t, RoleUser, out[i-1].Role,
				"dangling tool result: RoleTool at index %d preceded by %s, not RoleAssistant", i, out[i-1].Role)
			assert.Equal(t, RoleAssistant, out[i-1].Role,
				"RoleTool at index %d must be preceded by RoleAssistant", i)
		}
	}
}

func TestTruncatingCompactorKeepsPairedToolResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: "a1"},
		{Role: RoleTool, ToolName: "read", ToolResult: "tr1"},
	}

	compactor := NewTruncatingCompactor()
	out, err := compactor.Compact(ctx, items, 200, est)
	require.NoError(t, err)

	roles := make([]string, len(out))
	for i, it := range out {
		roles[i] = it.Role
	}
	assert.Equal(t, []string{RoleSystem, RoleUser, RoleAssistant, RoleTool}, roles)
}

func TestRemoveDanglingToolResults(t *testing.T) {
	tests := []struct {
		name  string
		input []TurnItem
		roles []string
	}{
		{
			name:  "empty",
			input: nil,
			roles: []string{},
		},
		{
			name: "tool at start removed",
			input: []TurnItem{
				{Role: RoleTool, ToolName: "read", ToolResult: "x"},
				{Role: RoleUser, Content: "u"},
			},
			roles: []string{RoleUser},
		},
		{
			name: "tool after user removed",
			input: []TurnItem{
				{Role: RoleUser, Content: "u"},
				{Role: RoleTool, ToolName: "read", ToolResult: "x"},
				{Role: RoleUser, Content: "v"},
			},
			roles: []string{RoleUser, RoleUser},
		},
		{
			name: "tool after assistant kept",
			input: []TurnItem{
				{Role: RoleUser, Content: "u"},
				{Role: RoleAssistant, Content: "a"},
				{Role: RoleTool, ToolName: "read", ToolResult: "x"},
			},
			roles: []string{RoleUser, RoleAssistant, RoleTool},
		},
		{
			name: "consecutive tools first removed second kept",
			input: []TurnItem{
				{Role: RoleUser, Content: "u"},
				{Role: RoleAssistant, Content: "a"},
				{Role: RoleTool, ToolName: "bash", ToolResult: "x"},
				{Role: RoleTool, ToolName: "read", ToolResult: "y"},
			},
			roles: []string{RoleUser, RoleAssistant, RoleTool, RoleTool},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := removeDanglingToolResults(tt.input)
			roles := make([]string, len(out))
			for i, it := range out {
				roles[i] = it.Role
			}
			assert.Equal(t, tt.roles, roles)
		})
	}
}
