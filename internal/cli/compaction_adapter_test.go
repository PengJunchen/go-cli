package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

// TestCompactionHook_ReducesHistory verifies that the hook, when given a
// history far exceeding the token budget, returns fewer messages than it
// received. Each message is ~100 chars (~25 tokens with the heuristic
// estimator); 50 messages total ~1250 tokens against a 100-token budget.
func TestCompactionHook_ReducesHistory(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	hook := newCompactionHook(compactor, estimator, 100)

	msgs := make([]core.AgentMessage, 50)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = core.AgentMessage{
			Role:    role,
			Content: strings.Repeat("x", 100),
		}
	}

	result, err := hook(context.Background(), msgs)
	require.NoError(t, err)
	assert.Less(t, len(result), len(msgs), "compaction should reduce the number of messages")
}

// TestCompactionHook_EmptyHistory verifies that an empty input produces an
// empty output with no error.
func TestCompactionHook_EmptyHistory(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	hook := newCompactionHook(compactor, estimator, 100)

	result, err := hook(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, result)
}

// TestCompactionHook_PreservesRoles verifies that every message surviving
// compaction still carries a valid conversation role.
func TestCompactionHook_PreservesRoles(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	hook := newCompactionHook(compactor, estimator, 100)

	msgs := []core.AgentMessage{
		{Role: "system", Content: strings.Repeat("s", 100)},
		{Role: "user", Content: strings.Repeat("u", 100)},
		{Role: "assistant", Content: strings.Repeat("a", 100)},
		{Role: "user", Content: strings.Repeat("u", 100)},
		{Role: "assistant", Content: strings.Repeat("a", 100)},
		{Role: "user", Content: strings.Repeat("u", 100)},
		{Role: "assistant", Content: strings.Repeat("a", 100)},
		{Role: "user", Content: strings.Repeat("u", 100)},
	}

	result, err := hook(context.Background(), msgs)
	require.NoError(t, err)
	assert.NotEmpty(t, result)

	validRoles := map[string]bool{
		"user":      true,
		"assistant": true,
		"system":    true,
	}
	for _, m := range result {
		assert.True(t, validRoles[m.Role], "invalid role after compaction: %q", m.Role)
	}
}

func TestCompactionHook_PreservesCanonicalMetadataWithoutCompacting(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	hook := newCompactionHook(compactor, estimator, 10000)

	messages := []core.AgentMessage{
		{
			Role:      "user",
			Content:   "follow up",
			InputKind: core.InputKindFollowUp,
		},
		{
			Role: "assistant",
			ToolCalls: []llm.ToolCall{{
				ID: "call-1", Name: "write", Args: map[string]any{"path": "/tmp", "nested": map[string]any{"enabled": true}},
			}},
			ContentBlocks: []llm.ContentBlock{{Type: "image_url", ImageURL: &llm.ImageURL{URL: "data:image/png;base64,abc", Detail: "low"}}},
			Usage:         &llm.Usage{InputTokens: 17, OutputTokens: 4, TotalTokens: 21},
		},
		{
			Role:           "tool",
			Content:        "uncertain",
			ToolCallID:     "call-1",
			ToolName:       "write",
			ReceiptOutcome: core.ReceiptOutcomeUnknown,
		},
	}

	result, err := hook(context.Background(), messages)
	require.NoError(t, err)
	require.Equal(t, messages, result)

	args := result[1].ToolCalls[0].Args.(map[string]any)
	args["new-key"] = "result mutation"
	nested := args["nested"].(map[string]any)
	nested["enabled"] = false
	result[1].ToolCalls[0].Args = args
	result[1].Usage.InputTokens = 100
	assert.NotContains(t, messages[1].ToolCalls[0].Args.(map[string]any), "new-key")
	assert.Equal(t, true, messages[1].ToolCalls[0].Args.(map[string]any)["nested"].(map[string]any)["enabled"])
	assert.Equal(t, 17, messages[1].Usage.InputTokens)
	result[1].ContentBlocks[0].ImageURL.URL = "changed"
	assert.Equal(t, "data:image/png;base64,abc", messages[1].ContentBlocks[0].ImageURL.URL)
}
