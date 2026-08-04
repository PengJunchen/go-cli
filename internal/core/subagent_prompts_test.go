package core

import (
	"context"
	"testing"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRolePromptReturnsTemplates(t *testing.T) {
	assert.Equal(t, ResearcherPrompt, RolePrompt("researcher"))
	assert.Equal(t, ImplementerPrompt, RolePrompt("implementer"))
	assert.Equal(t, ReviewerPrompt, RolePrompt("reviewer"))
	assert.Equal(t, TesterPrompt, RolePrompt("tester"))
}

func TestRolePromptUnknownRoleIsEmpty(t *testing.T) {
	assert.Empty(t, RolePrompt("architect"))
	assert.Empty(t, RolePrompt(""))
}

func TestResolveSubAgentSystemPromptExplicitWins(t *testing.T) {
	task := SubagentTask{SystemPrompt: "custom instructions", Role: "researcher"}
	assert.Equal(t, "custom instructions", resolveSubAgentSystemPrompt(task))
}

func TestResolveSubAgentSystemPromptRoleTemplate(t *testing.T) {
	task := SubagentTask{Role: "reviewer"}
	assert.Equal(t, ReviewerPrompt, resolveSubAgentSystemPrompt(task))
}

func TestResolveSubAgentSystemPromptDefault(t *testing.T) {
	task := SubagentTask{}
	assert.Equal(t, DefaultSubAgentPrompt, resolveSubAgentSystemPrompt(task))
}

func TestResolveSubAgentSystemPromptUnknownRoleFallsBackToDefault(t *testing.T) {
	task := SubagentTask{Role: "architect"}
	assert.Equal(t, DefaultSubAgentPrompt, resolveSubAgentSystemPrompt(task))
}

// TestLoopAgentWithSystemPromptOverrides proves WithSystemPrompt replaces the
// tool-aware default system prompt sent to the model.
func TestLoopAgentWithSystemPromptOverrides(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SYS", "override",
		mock.ConversationTurn{AssistantContent: "ok"},
	))

	loop := NewLoopAgent(WithLLM(model), WithSystemPrompt("OVERRIDE-PROMPT"))
	_, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	require.Equal(t, 1, model.CallCount())
	msgs := model.CallLog()[0].Messages
	require.NotEmpty(t, msgs)
	assert.Equal(t, llm.RoleSystem, msgs[0].Role)
	assert.Equal(t, "OVERRIDE-PROMPT", msgs[0].Content)
}

// TestLoopAgentDefaultSystemPromptWhenNoOverride proves that without an
// override the loop still sends its built-in tool-aware system prompt.
func TestLoopAgentDefaultSystemPromptWhenNoOverride(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SYS", "default",
		mock.ConversationTurn{AssistantContent: "ok"},
	))

	loop := NewLoopAgent(WithLLM(model))
	_, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	require.Equal(t, 1, model.CallCount())
	msgs := model.CallLog()[0].Messages
	require.NotEmpty(t, msgs)
	assert.Equal(t, llm.RoleSystem, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "helpful AI assistant")
}

// TestLoopAgentEmptySystemPromptFallsBackToDefault proves an empty override is
// treated as "not set" so the default is preserved (backward compatible).
func TestLoopAgentEmptySystemPromptFallsBackToDefault(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SYS", "empty-override",
		mock.ConversationTurn{AssistantContent: "ok"},
	))

	loop := NewLoopAgent(WithLLM(model), WithSystemPrompt(""))
	_, err := loop.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	msgs := model.CallLog()[0].Messages
	assert.Contains(t, msgs[0].Content, "helpful AI assistant")
}
