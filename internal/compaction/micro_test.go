package compaction

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
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
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-read", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-read", ToolName: "read", ToolResult: strings.Repeat("x", 400)},
		{Role: RoleUser, Content: "thank you"},
		{Role: RoleAssistant, Content: "you are welcome"},
	}

	compactor := NewMicroCompactor()
	outResult, err := compactor.Compact(ctx, items, 80, est)
	out := outResult.Items
	require.NoError(t, err)

	// User/assistant/system content preserved verbatim.
	assert.Equal(t, "sys", out[0].Content)
	assert.Equal(t, "what is the file?", out[1].Content)
	assert.Equal(t, "let me read it", out[2].Content)
	assert.Equal(t, "thank you", out[5].Content)
	assert.Equal(t, "you are welcome", out[6].Content)

	// The large tool result is replaced with the placeholder.
	assert.Equal(t, compactedToolResult, out[4].ToolResult)
	assert.Equal(t, "call-read", out[4].ToolCallID, "compaction must retain the receipt's call pairing")

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
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-read", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-read", ToolName: "read", ToolResult: strings.Repeat("y", 40)},
		{Role: RoleUser, Content: "short"},
		{Role: RoleAssistant, Content: "ok"},
	}

	compactor := NewMicroCompactor()
	outResult, err := compactor.Compact(ctx, items, 1000, est)
	out := outResult.Items
	require.NoError(t, err)
	assert.Equal(t, items, out) // unchanged because it already fits
}

func TestMicroCompactorEscalatesWhenStillOver(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	est := tracedEstimator()

	// Many tool entries whose placeholders still exceed a tiny budget.
	items := make([]TurnItem, 0, 80)
	for i := 0; i < 40; i++ {
		callID := fmt.Sprintf("call-%d", i)
		items = append(items, TurnItem{
			Role:      RoleAssistant,
			ToolCalls: []llm.ToolCall{{ID: callID, Name: "read"}},
		}, TurnItem{
			Role:       RoleTool,
			ToolCallID: callID,
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
	outResult, err := compactor.Compact(ctx, nil, 100, est)
	out := outResult.Items
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestMicroCompactorNormalizesLegacySummaryWithoutMutatingInput(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	compactor, err := NewDefaultCompactorFactory().Create("micro")
	require.NoError(t, err)
	usage := &llm.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}
	items := []TurnItem{
		{ID: "system", Role: RoleSystem, Content: "mandatory system"},
		{
			ID:              "legacy-summary",
			ParentID:        "old-turn",
			Role:            RoleSystem,
			Content:         "prior summary",
			IsCompaction:    true,
			EstimatedTokens: 1,
			Usage:           usage,
			FinishReason:    "stop",
			InputKind:       "legacy",
			ToolCallID:      "historic-call",
			ToolName:        "read",
			ReceiptOutcome:  "success",
		},
		{Role: RoleUser, Content: strings.Repeat("u", 40)},
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-read", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-read", ToolName: "read", ToolResult: strings.Repeat("x", 400)},
	}
	before := append([]TurnItem(nil), items...)
	before[3].ToolCalls = append([]llm.ToolCall(nil), items[3].ToolCalls...)

	outResult, err := compactor.Compact(ctx, items, 180, runeCountEstimator{})

	out := outResult.Items
	require.NoError(t, err)
	require.Len(t, out, len(items))

	expectedSummary := before[1]
	expectedSummary.Role = RoleUser
	assert.Equal(t, before[0], out[0], "mandatory system metadata must be preserved")
	assert.Equal(t, expectedSummary, out[1], "the defensive output copy should retain legacy summary metadata while demoting its role")
	assert.Equal(t, compactedToolResult, out[4].ToolResult, "standalone micro compaction should retain its normal tool-result reduction")
	assert.Equal(t, "call-read", out[4].ToolCallID)
	assert.Equal(t, before, items, "legacy normalization and result replacement must not mutate caller-owned items")
}

func TestMicroCompactorRejectsMandatorySystemOverBudgetBeforeReplacement(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	items := []TurnItem{
		{Role: RoleSystem, Content: strings.Repeat("s", 100)},
		{Role: RoleUser, Content: "question"},
		{Role: RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "call-read", Name: "read"}}},
		{Role: RoleTool, ToolCallID: "call-read", ToolName: "read", ToolResult: strings.Repeat("x", 400)},
	}
	before := append([]TurnItem(nil), items...)

	outResult, err := NewMicroCompactor().Compact(ctx, items, 10, runeCountEstimator{})

	out := outResult.Items
	require.ErrorIs(t, err, ErrMandatorySystemPromptOverBudget)
	assert.Equal(t, before, out, "preflight failure should return the original input unchanged")
	assert.Equal(t, before, items, "preflight failure must not mutate caller-owned items")
	assert.Equal(t, strings.Repeat("x", 400), out[3].ToolResult, "preflight must run before replacing any tool result")
}

func TestMicroCompactorRejectsMalformedToolGroupWithoutMutation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, _ := newTracedCtx(t)
	items := []TurnItem{{Role: RoleTool, ToolCallID: "orphan", ToolName: "read", ToolResult: strings.Repeat("x", 400)}}
	before := append([]TurnItem(nil), items...)

	_, err := NewMicroCompactor().Compact(ctx, items, 10, runeCountEstimator{})
	require.ErrorIs(t, err, errMalformedToolGroup)
	assert.Equal(t, before, items, "malformed input must remain unchanged")
}

func TestMicroCompactorCompileGuard(t *testing.T) {
	var _ Compactor = (*MicroCompactor)(nil)
	assert.NotNil(t, NewMicroCompactor())
}
