package mock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// contract providers table so the same contract tests can later be reused by
// real providers.
type llmContractCase struct {
	name     string
	provider llm.ModelProvider
}

func mockLLMProvider(t *testing.T) llm.ModelProvider {
	t.Helper()
	server := NewMockLLMServer(NewConversationTemplate("CTX", "contract",
		ConversationTurn{AssistantContent: "contract reply"},
	))
	return server
}

func llmContractCases(t *testing.T) []llmContractCase {
	return []llmContractCase{
		{"mock", mockLLMProvider(t)},
	}
}

func TestLLMContractGenerateResponseFormat(t *testing.T) {
	for _, tc := range llmContractCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			model, cleanup, err := tc.provider.Build(context.Background(), llm.ModelConfig{Model: "test"})
			require.NoError(t, err)
			defer cleanup()

			resp, err := model.Generate(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
			require.NoError(t, err)

			// Contract: Role must be assistant.
			assert.Equal(t, llm.RoleAssistant, resp.Role)
			// Contract: Content or ToolCalls must be non-empty.
			assert.True(t, resp.Content != "" || len(resp.ToolCalls) > 0,
				"expected non-empty content or tool_calls")
		})
	}
}

func TestLLMContractGenerateCancellation(t *testing.T) {
	for _, tc := range llmContractCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			model, cleanup, err := tc.provider.Build(context.Background(), llm.ModelConfig{Model: "test"})
			require.NoError(t, err)
			defer cleanup()

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_, err = model.Generate(ctx, []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
			assert.ErrorIs(t, err, context.Canceled)
		})
	}
}

func TestLLMContractStreamClosesUnlessError(t *testing.T) {
	for _, tc := range llmContractCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			model, cleanup, err := tc.provider.Build(context.Background(), llm.ModelConfig{Model: "test"})
			require.NoError(t, err)
			defer cleanup()

			ch, err := model.Stream(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "hi"}})
			require.NoError(t, err)

			count := 0
			for range ch {
				count++
			}
			// Contract: channel closes and yields at least one chunk.
			assert.GreaterOrEqual(t, count, 1)
		})
	}
}
