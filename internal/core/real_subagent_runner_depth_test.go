package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestRealSubAgentRunnerDepthLimitBlocksExceeded(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "depth-test",
		mock.ConversationTurn{AssistantContent: "should not reach"},
	))

	runner := &realSubAgentRunner{
		model:    model,
		tools:    tools.NewDefaultToolRegistry(),
		maxIter:  5,
		maxDepth: 2,
	}

	// Simulate being at depth 2 (the max). The runner should refuse to run.
	ctx := context.WithValue(context.Background(), subagentDepthKey{}, 2)
	final, err := runner.Run(ctx, "test", nil, func(AgentEvent) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "depth limit")
	assert.Empty(t, final.Content)
}

func TestRealSubAgentRunnerDepthLimitAllowsWithinLimit(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "depth-ok",
		mock.ConversationTurn{AssistantContent: "ok result"},
	))

	runner := &realSubAgentRunner{
		model:    model,
		tools:    tools.NewDefaultToolRegistry(),
		maxIter:  5,
		maxDepth: 3,
	}

	// Depth 1 should be allowed when maxDepth is 3.
	ctx := context.WithValue(context.Background(), subagentDepthKey{}, 1)
	final, err := runner.Run(ctx, "test", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "ok result", final.Content)
}

func TestRealSubAgentRunnerDepthPropagatesThroughContext(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "depth-propagation",
		mock.ConversationTurn{AssistantContent: "propagated"},
	))

	runner := &realSubAgentRunner{
		model:    model,
		tools:    tools.NewDefaultToolRegistry(),
		maxIter:  5,
		maxDepth: 3,
	}

	// Depth 0 (top-level). After Run, the runner should have depth=0 set.
	ctx := context.Background()
	final, err := runner.Run(ctx, "test", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "propagated", final.Content)
	assert.Equal(t, 0, runner.depth)
}

func TestDefaultSubAgentCompactionHookPreservesSystemMessage(t *testing.T) {
	msgs := make([]AgentMessage, 50)
	msgs[0] = AgentMessage{Role: "system", Content: "system prompt"}
	for i := 1; i < 50; i++ {
		msgs[i] = AgentMessage{Role: "user", Content: "msg"}
	}

	compacted, err := defaultSubAgentCompactionHook(context.Background(), msgs)
	require.NoError(t, err)
	assert.Less(t, len(compacted), len(msgs), "compaction should reduce message count")
	assert.Equal(t, "system prompt", compacted[0].Content, "first message must be preserved")
	assert.Equal(t, subagentMaxHistory, len(compacted))
}

func TestDefaultSubAgentCompactionHookNoOpWhenSmall(t *testing.T) {
	msgs := []AgentMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hi"},
	}
	compacted, err := defaultSubAgentCompactionHook(context.Background(), msgs)
	require.NoError(t, err)
	assert.Equal(t, len(msgs), len(compacted), "small history should not be compacted")
}
