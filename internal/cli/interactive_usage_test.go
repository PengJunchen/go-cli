package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

// noopLoop is a minimal AgentLoop that does nothing, used only to satisfy
// NewAgentImpl's non-nil loop requirement.
type noopLoop struct{}

func (noopLoop) Run(_ context.Context, _ core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	return nil, nil
}

// TestEmitTokenUsageEvent_PrefersAPIUsage verifies that when the last assistant
// message carries API-reported Usage, emitTokenUsageEvent uses those values
// instead of the local estimation.
func TestEmitTokenUsageEvent_PrefersAPIUsage(t *testing.T) {
	agent := core.NewAgentImpl("test", noopLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "what is 2+2?"},
		{
			Role:    "assistant",
			Content: "4",
			Usage: &llm.Usage{
				InputTokens:  10,
				OutputTokens: 3,
				TotalTokens:  13,
			},
		},
	}))

	stream := core.NewEventStream(64)
	assembly := &AgentAssembly{
		CoreRuntime:       CoreRuntime{Agent: agent},
		SessionManagement: SessionManagement{Estimator: compaction.NewHeuristicTokenEstimator()},
		ModelConfig:       ModelConfig{ContextWindow: 8000},
	}

	emitTokenUsageEvent(stream, assembly)

	// Read the token_usage event from the stream.
	select {
	case ev := <-stream.Events():
		require.Equal(t, "token_usage", ev.Kind)
		require.NotNil(t, ev.TokenUsage)
		// API values should be used, not estimated.
		assert.Equal(t, 10, ev.TokenUsage.InputTokens)
		assert.Equal(t, 3, ev.TokenUsage.OutputTokens)
		assert.Equal(t, 8000, ev.TokenUsage.MaxTokens)
	default:
		t.Fatal("expected a token_usage event on the stream")
	}
}

// TestEmitTokenUsageEvent_FallbackToEstimate verifies that when no assistant
// message has API-reported Usage, emitTokenUsageEvent falls back to estimating
// tokens from message content via the estimator.
func TestEmitTokenUsageEvent_FallbackToEstimate(t *testing.T) {
	// "hello world" = 10 letters * 0.25 + 1 space * 0.5 = 3 tokens (input)
	// "hi there" = 7 letters * 0.25 + 1 space * 0.5 = 2.25 -> 2 tokens (output)
	agent := core.NewAgentImpl("test", noopLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "hello world"},
		{Role: "assistant", Content: "hi there"},
	}))

	stream := core.NewEventStream(64)
	assembly := &AgentAssembly{
		CoreRuntime:       CoreRuntime{Agent: agent},
		SessionManagement: SessionManagement{Estimator: compaction.NewHeuristicTokenEstimator()},
		ModelConfig:       ModelConfig{ContextWindow: 4096},
	}

	emitTokenUsageEvent(stream, assembly)

	select {
	case ev := <-stream.Events():
		require.Equal(t, "token_usage", ev.Kind)
		require.NotNil(t, ev.TokenUsage)
		// Estimated values: input=3, output=2.
		assert.Equal(t, 3, ev.TokenUsage.InputTokens)
		assert.Equal(t, 2, ev.TokenUsage.OutputTokens)
		assert.Equal(t, 4096, ev.TokenUsage.MaxTokens)
	default:
		t.Fatal("expected a token_usage event on the stream")
	}
}

// TestEmitTokenUsageEvent_NoEstimatorNoUsage verifies that when there is no
// API usage and no estimator, the function returns early without sending an
// event.
func TestEmitTokenUsageEvent_NoEstimatorNoUsage(t *testing.T) {
	agent := core.NewAgentImpl("test", noopLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi"},
	}))

	stream := core.NewEventStream(64)
	assembly := &AgentAssembly{
		CoreRuntime: CoreRuntime{Agent: agent},
		// no estimator, no API usage
	}

	emitTokenUsageEvent(stream, assembly)

	select {
	case ev := <-stream.Events():
		t.Fatalf("expected no event, got: %+v", ev)
	default:
		// Expected: no event sent.
	}
}

// TestLastAssistantAPIUsage verifies the helper that finds the last assistant
// message with non-nil Usage.
func TestLastAssistantAPIUsage(t *testing.T) {
	t.Run("finds last assistant with usage", func(t *testing.T) {
		msgs := []core.AgentMessage{
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1", Usage: &llm.Usage{InputTokens: 5, OutputTokens: 2}},
			{Role: "user", Content: "q2"},
			{Role: "assistant", Content: "a2", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 4}},
		}
		usage := lastAssistantAPIUsage(msgs)
		require.NotNil(t, usage)
		assert.Equal(t, 10, usage.InputTokens)
		assert.Equal(t, 4, usage.OutputTokens)
	})

	t.Run("returns nil when no assistant has usage", func(t *testing.T) {
		msgs := []core.AgentMessage{
			{Role: "user", Content: "q1"},
			{Role: "assistant", Content: "a1"},
		}
		usage := lastAssistantAPIUsage(msgs)
		assert.Nil(t, usage)
	})

	t.Run("returns nil for empty messages", func(t *testing.T) {
		usage := lastAssistantAPIUsage(nil)
		assert.Nil(t, usage)
	})
}
