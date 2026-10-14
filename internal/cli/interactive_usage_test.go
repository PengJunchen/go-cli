package cli

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
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
		assert.Equal(t, "reported", string(ev.TokenUsage.UsageSource))
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
		assert.Equal(t, "estimated", string(ev.TokenUsage.UsageSource))
	default:
		t.Fatal("expected a token_usage event on the stream")
	}
}

func TestEmitTokenUsageEventCarriesActualAttemptProvenance(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	result := llm.ProviderAttemptResult{
		OperationID: "op-display-provenance",
		AttemptID:   "attempt-display-provenance",
		Identity: llm.AttemptIdentity{
			ProviderID: "actual-provider",
			ModelID:    "gpt-4o",
			ProtocolID: llm.ProtocolOpenAICompatible,
			OriginKind: llm.OriginExplicitRequestEndpoint,
			Candidate:  "candidate-private-marker",
		},
		Outcome: llm.AttemptSuccess,
		Usage: llm.UsageEvidence{
			State:  llm.UsageKnown,
			Usage:  &llm.Usage{InputTokens: 12, OutputTokens: 5, TotalTokens: 17},
			Source: llm.UsageProviderResponse,
		},
	}
	_, err := tracker.RecordAttempt(result)
	require.NoError(t, err)
	reportedCost := 0.02
	reported := result
	reported.OperationID = "op-provider-cost"
	reported.AttemptID = "attempt-provider-cost"
	reported.Identity.ProviderID = "reported-provider"
	reported.Identity.ModelID = "custom-model"
	reported.Identity.Candidate = "candidate-provider-cost"
	reported.Usage = llm.UnknownUsageEvidence()
	reported.ProviderReportedCostUSD = &reportedCost
	_, err = tracker.RecordAttempt(reported)
	require.NoError(t, err)
	agent := core.NewAgentImpl("provenance", noopLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "request"},
		{Role: "assistant", Content: "response"},
	}))
	assembly := &AgentAssembly{
		CoreRuntime:          CoreRuntime{Agent: agent},
		ProductionResilience: ProductionResilience{CostTracker: tracker},
		SessionManagement:    SessionManagement{Estimator: compaction.NewHeuristicTokenEstimator()},
		ModelConfig:          ModelConfig{ContextWindow: 8192},
	}
	stream := core.NewEventStream(8)
	emitTokenUsageEvent(stream, assembly)

	select {
	case event := <-stream.Events():
		require.NotNil(t, event.TokenUsage)
		assert.Equal(t, "reported", string(event.TokenUsage.UsageSource))
		assert.Equal(t, []core.TokenUsageProviderModel{
			{ProviderID: "actual-provider", ModelID: "gpt-4o"},
			{ProviderID: "reported-provider", ModelID: "custom-model"},
		}, event.TokenUsage.ProviderModels)
		assert.Equal(t, []core.TokenUsageCostSource{core.TokenUsageCostEstimated, core.TokenUsageCostReported}, event.TokenUsage.CostSources)
		assert.Equal(t, []core.TokenUsagePriceSource{core.TokenUsagePriceDefaultTier, core.TokenUsagePriceProvider}, event.TokenUsage.PriceSources)
		assert.Equal(t, 1, event.TokenUsage.UnknownUsageAttempts)
		encoded, marshalErr := json.Marshal(event.TokenUsage)
		require.NoError(t, marshalErr)
		assert.NotContains(t, string(encoded), "candidate-private-marker")
		assert.NotContains(t, string(encoded), "origin")
	default:
		t.Fatal("expected a token_usage event")
	}
}

func TestEmitTokenUsageEventCombinesLegacyAndAttemptCosts(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)
	_, err = tracker.RecordAttempt(llm.ProviderAttemptResult{
		OperationID: "op-mixed-cost",
		AttemptID:   "attempt-mixed-cost",
		Identity: llm.AttemptIdentity{
			ProviderID: "actual-provider",
			ModelID:    "gpt-4o",
			ProtocolID: llm.ProtocolOpenAICompatible,
			OriginKind: llm.OriginResolverDefaultEndpoint,
			Candidate:  "candidate-mixed-cost",
		},
		Outcome: llm.AttemptSuccess,
		Usage: llm.UsageEvidence{
			State:  llm.UsageKnown,
			Usage:  &llm.Usage{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
			Source: llm.UsageProviderResponse,
		},
	})
	require.NoError(t, err)

	agent := core.NewAgentImpl("mixed-cost", noopLoop{}, core.WithHistory([]core.AgentMessage{
		{Role: "user", Content: "request"},
		{Role: "assistant", Content: "response"},
	}))
	stream := core.NewEventStream(8)
	emitTokenUsageEvent(stream, &AgentAssembly{
		CoreRuntime:          CoreRuntime{Agent: agent},
		ProductionResilience: ProductionResilience{CostTracker: tracker},
		ModelConfig:          ModelConfig{ContextWindow: 8192},
	})

	select {
	case event := <-stream.Events():
		require.NotNil(t, event.TokenUsage)
		assert.InDelta(t, 0.015, event.TokenUsage.Cost, 0.000001,
			"the token event must include the legacy estimate and the attempt subtotal once each")
		assert.Equal(t, 1, event.TokenUsage.UnknownUsageAttempts,
			"legacy rows have no typed usage provenance in the attempt ledger")
		assert.Equal(t, "reported", string(event.TokenUsage.UsageSource))
		assert.Equal(t, []core.TokenUsageCostSource{core.TokenUsageCostEstimated}, event.TokenUsage.CostSources)
		assert.Equal(t, []core.TokenUsagePriceSource{core.TokenUsagePriceDefaultTier}, event.TokenUsage.PriceSources)
	default:
		t.Fatal("expected a token_usage event")
	}
}

// TestEmitTokenUsageEvent_NoEstimatorNoUsage verifies that when there is no
// API usage and no estimator, the function emits explicit unknown provenance.
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
		require.NotNil(t, ev.TokenUsage)
		assert.Equal(t, "unknown", string(ev.TokenUsage.UsageSource))
	default:
		t.Fatal("expected an explicit unknown token_usage event")
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
