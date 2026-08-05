package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
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

	factory := NewRealSubAgentRunnerFactory(model, nil, tools.NewDefaultToolRegistry())
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

	f := NewRealSubAgentFactory(model, nil, tools.NewDefaultToolRegistry())
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

// TestRealSubAgentRunner_UsesSystemPrompt proves the runner injects its
// systemPrompt into the LoopAgent so the model receives it as the system
// message (overriding the tool-aware default).
func TestRealSubAgentRunner_UsesSystemPrompt(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "sys-prompt",
		mock.ConversationTurn{AssistantContent: "done"},
	))

	runner := &realSubAgentRunner{
		model:        model,
		tools:        tools.NewDefaultToolRegistry(),
		maxIter:      3,
		systemPrompt: "ROLE-PROMPT-FROM-CONFIG",
	}

	_, err := runner.Run(context.Background(), "test", nil, func(AgentEvent) {})
	require.NoError(t, err)

	require.Equal(t, 1, model.CallCount())
	msgs := model.CallLog()[0].Messages
	require.NotEmpty(t, msgs)
	assert.Equal(t, llm.RoleSystem, msgs[0].Role)
	assert.Equal(t, "ROLE-PROMPT-FROM-CONFIG", msgs[0].Content)
}

// TestRealSubAgentRunner_NoSystemPromptUsesDefault proves that when the runner
// has no systemPrompt the LoopAgent falls back to its built-in default.
func TestRealSubAgentRunner_NoSystemPromptUsesDefault(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "no-sys-prompt",
		mock.ConversationTurn{AssistantContent: "done"},
	))

	runner := &realSubAgentRunner{
		model:   model,
		tools:   tools.NewDefaultToolRegistry(),
		maxIter: 3,
	}

	_, err := runner.Run(context.Background(), "test", nil, func(AgentEvent) {})
	require.NoError(t, err)

	msgs := model.CallLog()[0].Messages
	assert.Contains(t, msgs[0].Content, "helpful AI assistant")
}

// TestNewRealSubAgentRunnerFactory_CapturesSystemPrompt proves the factory
// closure threads cfg.SystemPrompt into the produced runner.
func TestNewRealSubAgentRunnerFactory_CapturesSystemPrompt(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "factory-sys",
		mock.ConversationTurn{AssistantContent: "ok"},
	))

	factory := NewRealSubAgentRunnerFactory(model, nil, tools.NewDefaultToolRegistry())
	runner := factory(SubAgentConfig{Name: "test", MaxTurns: 5, SystemPrompt: "FROM-FACTORY"})
	require.NotNil(t, runner)

	_, err := runner.Run(context.Background(), "prompt", nil, func(AgentEvent) {})
	require.NoError(t, err)

	msgs := model.CallLog()[0].Messages
	assert.Equal(t, "FROM-FACTORY", msgs[0].Content)
}

// TestRealSubAgentRunner_ConsumesInbox proves the runner consumes messages
// from the inbox channel and emits them as "user" events alongside the
// agent's own event stream.
func TestRealSubAgentRunner_ConsumesInbox(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "inbox-test",
		mock.ConversationTurn{AssistantContent: "done"},
	))

	inbox := make(chan string, 1)
	inbox <- "follow-up message"

	runner := &realSubAgentRunner{
		model:   model,
		tools:   tools.NewDefaultToolRegistry(),
		maxIter: 3,
	}

	var events []AgentEvent
	var mu sync.Mutex
	emit := func(ev AgentEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}

	_, err := runner.Run(context.Background(), "test", inbox, emit)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, ev := range events {
		if ev.Kind == "user" && ev.Content == "follow-up message" {
			found = true
			break
		}
	}
	assert.True(t, found, "inbox message should be emitted as a user event")
}

// TestRealSubAgentRunner_ModelOverride proves the runner uses the
// ProviderRegistry to build a model when cfg.Model is set, instead of the
// parent model.
func TestRealSubAgentRunner_ModelOverride(t *testing.T) {
	parentModel := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "parent-model",
		mock.ConversationTurn{AssistantContent: "parent response"},
	))

	registryModel := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "registry-model",
		mock.ConversationTurn{AssistantContent: "registry response"},
	))

	reg := llm.NewProviderRegistry()
	require.NoError(t, reg.Register(registryModel))

	factory := NewRealSubAgentRunnerFactory(parentModel, reg, tools.NewDefaultToolRegistry())
	runner := factory(SubAgentConfig{Name: "test", MaxTurns: 3, Model: "mock"})

	final, err := runner.Run(context.Background(), "test", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "registry response", final.Content,
		"runner should use the registry model when cfg.Model is set")
}

// TestRealSubAgentRunner_ModelOverrideFallsBackToParent proves the runner
// uses the parent model when cfg.Model is empty.
func TestRealSubAgentRunner_ModelOverrideFallsBackToParent(t *testing.T) {
	parentModel := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"System", "parent-fallback",
		mock.ConversationTurn{AssistantContent: "parent response"},
	))

	reg := llm.NewProviderRegistry()

	factory := NewRealSubAgentRunnerFactory(parentModel, reg, tools.NewDefaultToolRegistry())
	runner := factory(SubAgentConfig{Name: "test", MaxTurns: 3})

	final, err := runner.Run(context.Background(), "test", nil, func(AgentEvent) {})
	require.NoError(t, err)
	assert.Equal(t, "parent response", final.Content)
}

// stubToolDef is a minimal ToolDefinition for testing.
type stubToolDef struct {
	name string
}

func (s *stubToolDef) Name() string                                             { return s.name }
func (s *stubToolDef) Description() string                                      { return "stub" }
func (s *stubToolDef) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return nil, nil
}
func (s *stubToolDef) PromptGuidelines() []string { return nil }

// TestFilteredToolRegistry verifies the filteredToolRegistry only exposes
// tools whose names are in the allowed list.
func TestFilteredToolRegistry(t *testing.T) {
	inner := tools.NewDefaultToolRegistry()
	_ = inner.Register(context.Background(), &stubToolDef{name: "alpha"}) //nolint:errcheck
	_ = inner.Register(context.Background(), &stubToolDef{name: "beta"})  //nolint:errcheck
	_ = inner.Register(context.Background(), &stubToolDef{name: "gamma"}) //nolint:errcheck

	filtered := newFilteredToolRegistry(inner, []string{"alpha", "beta"})

	// Allowed tools are accessible.
	_, err := filtered.Get(context.Background(), "alpha")
	require.NoError(t, err)

	_, err = filtered.Get(context.Background(), "beta")
	require.NoError(t, err)

	// Disallowed tools are not accessible.
	_, err = filtered.Get(context.Background(), "gamma")
	assert.ErrorIs(t, err, tools.ErrToolNotFound)

	// List only returns allowed tools.
	list, err := filtered.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

// TestFilteredToolRegistryEmptyAllowed verifies the filteredToolRegistry with
// an empty allowed list blocks all tools.
func TestFilteredToolRegistryEmptyAllowed(t *testing.T) {
	inner := tools.NewDefaultToolRegistry()
	_ = inner.Register(context.Background(), &stubToolDef{name: "alpha"}) //nolint:errcheck

	filtered := newFilteredToolRegistry(inner, nil)

	_, err := filtered.Get(context.Background(), "alpha")
	assert.ErrorIs(t, err, tools.ErrToolNotFound)

	list, err := filtered.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
}
