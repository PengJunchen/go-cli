package cli

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
)

func TestEmitPrimaryCoverageScope(t *testing.T) {
	parse := func(s string) *llm.Usage {
		if s == "" {
			return nil
		}
		var u llm.Usage
		require.NoError(t, json.Unmarshal([]byte(s), &u))
		return &u
	}
	complete := `{"input_tokens":100,"output_tokens":20,"total_tokens":120,"primary_coverage":"complete"}`
	partial := `{"input_tokens":50,"output_tokens":10,"total_tokens":60,"primary_coverage":"partial"}`
	legacy := `{"input_tokens":3,"output_tokens":1,"total_tokens":4}`
	for _, test := range []struct {
		name, latest, coverage, scope string
		attempts                      []string
		input, output, unknown        int
	}{
		{"latest-complete", complete, "complete", "latest_assistant_message", nil, 100, 20, 0},
		{"latest-partial", partial, "partial", "latest_assistant_message", nil, 50, 10, 0},
		{"latest-legacy", legacy, "unknown", "latest_assistant_message", nil, 3, 1, 0},
		{"latest-missing", "", "unknown", "unknown", nil, 0, 0, 0},
		{"attempt-complete", partial, "complete", "observed_attempts", []string{complete}, 100, 20, 0},
		{"retry-missing-then-known", complete, "partial", "observed_attempts", []string{"", complete}, 100, 20, 1},
		{"attempt-legacy-plus-known", partial, "unknown", "observed_attempts", []string{legacy, complete}, 103, 21, 0},
		{"attempt-partial-plus-known", complete, "partial", "observed_attempts", []string{partial, complete}, 150, 30, 0},
		{"attempt-all-missing", complete, "unknown", "observed_attempts", []string{""}, 0, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := core.NewAgentImpl("primary", noopLoop{}, core.WithHistory([]core.AgentMessage{{Role: "assistant", Usage: parse(test.latest)}}))
			tracker := production.NewCostTracker(nil)
			for i, segment := range test.attempts {
				evidence := llm.UnknownUsageEvidence()
				if usage := parse(segment); usage != nil {
					evidence = llm.UsageEvidence{State: llm.UsageKnown, Source: llm.UsageProviderResponse, Usage: usage}
				}
				_, err := tracker.RecordAttempt(llm.ProviderAttemptResult{
					OperationID: "op", AttemptID: llm.ProviderAttemptID(string(rune('a' + i))),
					Identity: llm.AttemptIdentity{ProviderID: "fixture", ModelID: "model", ProtocolID: llm.ProtocolOpenAICompatible, OriginKind: llm.OriginExplicitRequestEndpoint, Candidate: "candidate"},
					Outcome:  llm.AttemptSuccess, Usage: evidence,
				})
				require.NoError(t, err)
			}
			assembly := &AgentAssembly{CoreRuntime: CoreRuntime{Agent: agent}, ProductionResilience: ProductionResilience{CostTracker: tracker}}
			stream := core.NewEventStream(8)
			emitTokenUsageEvent(stream, assembly)
			event := <-stream.Events()
			require.NotNil(t, event.TokenUsage)
			assert.Equal(t, test.input, event.TokenUsage.InputTokens)
			assert.Equal(t, test.output, event.TokenUsage.OutputTokens)
			assert.Equal(t, test.unknown, event.TokenUsage.UnknownUsageAttempts)
			data, err := json.Marshal(event.TokenUsage)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, json.Unmarshal(data, &got))
			assert.Equal(t, test.coverage, got["primary_coverage"])
			assert.Equal(t, test.scope, got["usage_scope"])
		})
	}
}

func TestLatestPrimaryCoverageDoesNotBorrowLegacyAttemptCount(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)
	usage := &llm.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, PrimaryCoverage: llm.UsageCoverageComplete}
	agent := core.NewAgentImpl("primary", noopLoop{}, core.WithHistory([]core.AgentMessage{{Role: "assistant", Usage: usage}}))
	stream := core.NewEventStream(8)
	emitTokenUsageEvent(stream, &AgentAssembly{CoreRuntime: CoreRuntime{Agent: agent}, ProductionResilience: ProductionResilience{CostTracker: tracker}})
	event := <-stream.Events()
	require.NotNil(t, event.TokenUsage)
	assert.Equal(t, 1, event.TokenUsage.UnknownUsageAttempts)
	assert.Equal(t, core.TokenUsageScopeLatestAssistantMessage, event.TokenUsage.UsageScope)
	assert.Equal(t, llm.UsageCoverageComplete, event.TokenUsage.PrimaryCoverage)
	assert.Equal(t, 100, event.TokenUsage.InputTokens)
}

func TestUnrepresentableAttemptDoesNotReportZero(t *testing.T) {
	tracker := production.NewCostTracker(nil)
	_, err := tracker.RecordAttempt(llm.ProviderAttemptResult{
		OperationID: "op", AttemptID: "attempt",
		Identity: llm.AttemptIdentity{ProviderID: "fixture", ModelID: "gpt-4o", ProtocolID: llm.ProtocolOpenAICompatible, OriginKind: llm.OriginExplicitRequestEndpoint, Candidate: "c"},
		Outcome:  llm.AttemptSuccess,
		Usage:    llm.UsageEvidence{State: llm.UsageKnown, Source: llm.UsageProviderResponse, Usage: &llm.Usage{InputTokens: math.MaxInt, OutputTokens: 1, TotalTokens: math.MaxInt, PrimaryCoverage: llm.UsageCoverageComplete}},
	})
	require.NoError(t, err)
	agent := core.NewAgentImpl("primary", noopLoop{})
	stream := core.NewEventStream(8)
	emitTokenUsageEvent(stream, &AgentAssembly{CoreRuntime: CoreRuntime{Agent: agent}, ProductionResilience: ProductionResilience{CostTracker: tracker}})
	event := <-stream.Events()
	require.NotNil(t, event.TokenUsage)
	assert.Equal(t, core.TokenUsageScopeObservedAttempts, event.TokenUsage.UsageScope)
	assert.Equal(t, core.TokenUsageSourceUnknown, event.TokenUsage.UsageSource)
	assert.Equal(t, llm.UsageCoverageUnknown, event.TokenUsage.PrimaryCoverage)
}
