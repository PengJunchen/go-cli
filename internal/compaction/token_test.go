package compaction

import (
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestHeuristicTokenEstimatorCharsOverFour(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	est := NewHeuristicTokenEstimator()

	n, err := est.Estimate("abcdefgh") // 8 chars / 4
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestHeuristicTokenEstimatorEmpty(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	n, err := est.Estimate("")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestHeuristicTokenEstimatorTruncates(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	n, err := est.Estimate("hello") // 5 chars / 4 = 1
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestHeuristicTokenEstimatorNeverErrors(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	for _, text := range []string{"", " ", "こんにちは世界", "a longer english sentence"} {
		_, err := est.Estimate(text)
		assert.NoError(t, err, "estimate should never error for %q", text)
	}
}

func TestHeuristicTokenEstimatorCompileGuard(t *testing.T) {
	var _ TokenEstimator = (*HeuristicTokenEstimator)(nil)
	assert.NotNil(t, NewHeuristicTokenEstimator())
}

func TestHeuristicEstimateASCII(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	// Pure ASCII alphanumerics: ~0.25 tokens per char.
	n, err := est.Estimate("abcdefgh") // 8 ASCII letters
	require.NoError(t, err)
	assert.Equal(t, 2, n) // 8 * 0.25 = 2.0
}

func TestHeuristicEstimateCJK(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	// CJK runes count as 2 tokens each.
	n, err := est.Estimate("你好世界") // 4 CJK chars
	require.NoError(t, err)
	assert.Equal(t, 8, n) // 4 * 2 = 8
}

func TestHeuristicEstimateMixed(t *testing.T) {
	est := NewHeuristicTokenEstimator()
	// 3 ASCII letters (0.25 each = 0.75) + 2 CJK (2 each = 4) = 4.75 -> 5
	n, err := est.Estimate("abc你好")
	require.NoError(t, err)
	assert.Equal(t, 5, n)
}

func TestEstimateItemTokensCountsProjectedSummaryWrapperAndIgnoresOldCache(t *testing.T) {
	item := TurnItem{
		Role: RoleUser, Content: "summary", IsCompaction: true,
		EstimatedTokens: 1,
	}

	got := estimateItemTokens(&item, runeCountEstimator{})
	assert.Equal(t, len(RoleUser)+len(UntrustedSummaryContext("summary")), got)
	assert.Equal(t, got, item.EstimatedTokens)

	empty := TurnItem{Role: RoleUser, IsCompaction: true}
	assert.Equal(t, len(RoleUser)+len(UntrustedSummaryContext("[compacted]")), estimateItemTokens(&empty, runeCountEstimator{}))
}

func TestUnserializableToolArgumentsRemainOverBudgetInSummaryCutSearch(t *testing.T) {
	args := map[string]any{}
	args["self"] = args
	items := []TurnItem{{
		Role:      RoleAssistant,
		ToolCalls: []llm.ToolCall{{ID: "call-cycle", Name: "write", Args: args}},
	}}

	assert.Equal(t, maxTokenEstimate, EstimateTokens(items, runeCountEstimator{}),
		"unserializable provider arguments must not collapse to a small estimate")
	cut := NewSummaryCompactor(nil).findCutPointAtGroups(
		items, []itemGroup{{start: 0, end: 1}}, math.MaxInt-1, runeCountEstimator{},
	)
	assert.Equal(t, 1, cut,
		"the cut search must not let the over-budget suffix wrap into a fitting prefix")
}

func TestSummaryCutSearchCountsProjectedUserRoleInPlaceholder(t *testing.T) {
	items := []TurnItem{
		{Role: RoleUser, Content: strings.Repeat("x", 200)},
		{Role: RoleUser, Content: "current"},
	}
	normalized, groups, err := groupTurnItems(items)
	require.NoError(t, err)

	// Text-only placeholder87 + current user11 would fit 101, but the projected
	// summary also includes the four-character RoleUser term (total 102).
	cut := NewSummaryCompactor(nil).findCutPointAtGroups(normalized, groups, 101, runeCountEstimator{})
	assert.Equal(t, len(items), cut,
		"a candidate suffix that only fits by omitting the summary role must be cut as a whole")
}

func TestEstimateTokensCountsProviderVisibleMessageAndToolCallFields(t *testing.T) {
	items := []TurnItem{
		{
			Role:    RoleAssistant,
			Content: "ok",
			ToolCalls: []llm.ToolCall{{
				ID: "call1", Name: "read", Args: map[string]any{"x": "y"},
			}},
		},
		{Role: RoleTool, ToolName: "read", ToolCallID: "call1", ToolResult: "ok"},
		{Role: RoleUser, Content: "x"},
	}

	// The OpenAI-compatible message envelope includes each role, tool name,
	// tool-call ID, and the JSON arguments. With the rune-count estimator these
	// fixture terms total 29 + 15 + 5 tokens respectively.
	assert.Equal(t, 49, EstimateTokens(items, runeCountEstimator{}))

	assistant := items[0]
	assert.Equal(t, 29, estimateItemTokens(&assistant, runeCountEstimator{}),
		"the item estimate used by UnifiedCompactor must use the same budget units")
}

func TestEstimateTokensUsesTextBlocksInsteadOfShadowContent(t *testing.T) {
	item := TurnItem{
		Role:          RoleUser,
		Content:       "shadow content is not sent when blocks are present",
		ContentBlocks: []llm.ContentBlock{{Type: "text", Text: "block"}},
	}

	assert.Equal(t, 9, EstimateTokens([]TurnItem{item}, runeCountEstimator{}),
		"provider-visible role plus text block should be estimated once")
}
