package core

import (
	"context"
	"testing"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRealSubAgentRunner_CallsRealLLM(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"You are a helpful assistant.", "subagent-test",
		mock.ConversationTurn{AssistantContent: "real analysis result"},
	))

	runner := &realSubAgentRunner{
		model:   model,
		tools:   tools.NewDefaultToolRegistry(),
		maxIter: 5,
	}

	var events []AgentEvent
	emit := func(ev AgentEvent) { events = append(events, ev) }

	final, err := runner.Run(context.Background(), "analyze this", nil, emit)
	require.NoError(t, err)
	assert.Equal(t, "real analysis result", final.Content)
	assert.Equal(t, "assistant", final.Role)
	assert.NotEmpty(t, events)
}

func TestRealSubAgentRunner_NotSimulatedResponse(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "not-simulated",
		mock.ConversationTurn{AssistantContent: "genuine LLM output"},
	))

	runner := &realSubAgentRunner{
		model:   model,
		tools:   tools.NewDefaultToolRegistry(),
		maxIter: 3,
	}

	final, err := runner.Run(context.Background(), "test prompt", nil, func(AgentEvent) {})
	require.NoError(t, err)

	// Must NOT be the simulated "response-1" string.
	assert.NotEqual(t, "response-1", final.Content)
	assert.Equal(t, "genuine LLM output", final.Content)
}

func TestRealSubAgentRunner_IndependentHistory(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "independent-history",
		mock.ConversationTurn{AssistantContent: "first response"},
		mock.ConversationTurn{AssistantContent: "second response"},
	))

	// Run two sub-agents; each should get independent history.
	runner1 := &realSubAgentRunner{
		model:   model,
		tools:   tools.NewDefaultToolRegistry(),
		maxIter: 3,
	}
	runner2 := &realSubAgentRunner{
		model:   model,
		tools:   tools.NewDefaultToolRegistry(),
		maxIter: 3,
	}

	final1, err := runner1.Run(context.Background(), "first prompt", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "first response", final1.Content)

	final2, err := runner2.Run(context.Background(), "second prompt", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "second response", final2.Content)
}

func TestRealSubAgentRunner_ForwardsEvents(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "event-forwarding",
		mock.ConversationTurn{AssistantContent: "forwarded"},
	))

	runner := &realSubAgentRunner{
		model:   model,
		tools:   tools.NewDefaultToolRegistry(),
		maxIter: 3,
	}

	var collected []AgentEvent
	emit := func(ev AgentEvent) { collected = append(collected, ev) }

	_, err := runner.Run(context.Background(), "test", nil, emit)
	require.NoError(t, err)
	assert.NotEmpty(t, collected)
}

func TestRealSubAgentRunner_NilToolsWorks(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "nil-tools",
		mock.ConversationTurn{AssistantContent: "no tools needed"},
	))

	runner := &realSubAgentRunner{
		model:   model,
		tools:   nil,
		maxIter: 3,
	}

	final, err := runner.Run(context.Background(), "test", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "no tools needed", final.Content)
}

func TestNewRealSubAgentRunnerFactory_ProducesRealRunner(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "factory-test",
		mock.ConversationTurn{AssistantContent: "factory output"},
	))

	factory := NewRealSubAgentRunnerFactory(model, tools.NewDefaultToolRegistry())
	runner := factory(SubAgentConfig{Name: "test", MaxTurns: 5})
	require.NotNil(t, runner)

	final, err := runner.Run(context.Background(), "prompt", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "factory output", final.Content)
}

func TestNewRealSubAgentFactory_CreatesWorkingFactory(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "exported-factory-test",
		mock.ConversationTurn{AssistantContent: "exported factory works"},
	))

	f := NewRealSubAgentFactory(model, tools.NewDefaultToolRegistry())
	require.NotNil(t, f)

	sub, err := f.Create(context.Background(), "test-sub", SubAgentConfig{
		Name:     "test-sub",
		MaxTurns: 3,
	})
	require.NoError(t, err)
	require.NotNil(t, sub)

	evCh, err := sub.Run(context.Background(), "test prompt")
	require.NoError(t, err)

	// Drain events.
	go func() {
		for range evCh {
		}
	}()

	final, waitErr := sub.Wait(context.Background())
	require.NoError(t, waitErr)
	assert.Equal(t, "exported factory works", final.Content)
	// Must not be the simulated "response-1".
	assert.NotEqual(t, "response-1", final.Content)
}

func TestRealSubAgentRunner_SatisfiesInterface(t *testing.T) {
	var _ subAgentRunner = (*realSubAgentRunner)(nil)
}

func TestRealSubAgentRunner_ImplementsBaseChatModel(t *testing.T) {
	// Verify mock model satisfies the interface used by realSubAgentRunner.
	var _ llm.BaseChatModel = mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "interface-check",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
}
