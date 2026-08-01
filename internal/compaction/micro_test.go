package compaction

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestMicroCompactorReplacesLargeToolResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, exp := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleUser, Content: "what is the file?"},
		{Role: RoleAssistant, Content: "let me read it"},
		{Role: RoleTool, ToolName: "read", ToolResult: strings.Repeat("x", 400)},
		{Role: RoleUser, Content: "thank you"},
		{Role: RoleAssistant, Content: "you are welcome"},
	}

	compactor := NewMicroCompactor()
	out, err := compactor.Compact(ctx, items, 80, est)
	require.NoError(t, err)

	// User/assistant/system content preserved verbatim.
	assert.Equal(t, "sys", out[0].Content)
	assert.Equal(t, "what is the file?", out[1].Content)
	assert.Equal(t, "let me read it", out[2].Content)
	assert.Equal(t, "thank you", out[4].Content)
	assert.Equal(t, "you are welcome", out[5].Content)

	// The large tool result is replaced with the placeholder.
	assert.Equal(t, compactedToolResult, out[3].ToolResult)

	// Total must be within budget.
	assert.LessOrEqual(t, estimateTokens(out, est), 80)

	// Span was emitted.
	assertSpanEventually(t, exp, "compaction.micro")
}

func TestMicroCompactorKeepsUserAssistantWhenUnderBudget(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	items := []TurnItem{
		{Role: RoleTool, ToolName: "read", ToolResult: strings.Repeat("y", 40)},
		{Role: RoleUser, Content: "short"},
		{Role: RoleAssistant, Content: "ok"},
	}

	compactor := NewMicroCompactor()
	out, err := compactor.Compact(ctx, items, 1000, est)
	require.NoError(t, err)
	assert.Equal(t, items, out) // unchanged because it already fits
}

func TestMicroCompactorEscalatesWhenStillOver(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	// Many tool entries whose placeholders still exceed a tiny budget.
	items := make([]TurnItem, 0, 40)
	for i := 0; i < 40; i++ {
		items = append(items, TurnItem{
			Role:       RoleTool,
			ToolName:   "read",
			ToolResult: strings.Repeat("z", 100),
		})
	}

	compactor := NewMicroCompactor()
	_, err := compactor.Compact(ctx, items, 5, est)
	require.ErrorIs(t, err, ErrRequiresTruncating)
}

func TestMicroCompactorEmptyInput(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	compactor := NewMicroCompactor()
	out, err := compactor.Compact(ctx, nil, 100, est)
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestMicroCompactorCompileGuard(t *testing.T) {
	var _ Compactor = (*MicroCompactor)(nil)
	assert.NotNil(t, NewMicroCompactor())
}
