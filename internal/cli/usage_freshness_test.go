package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestEmitTokenUsageLatestAssistantFreshness(t *testing.T) {
	old := &llm.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, CacheReadInputTokens: 80, CacheReadInputTokensReported: true, ReasoningOutputTokens: 7, ReasoningOutputTokensReported: true}
	for _, test := range []struct {
		name                             string
		messages                         []core.AgentMessage
		estimator                        bool
		source                           core.TokenUsageSource
		input, output, cache, reasoning  int
		cacheReported, reasoningReported bool
	}{
		{name: "old-known-latest-nil", messages: []core.AgentMessage{{Role: "assistant", Usage: old}, {Role: "user", Content: "question"}, {Role: "assistant", Content: "answer"}, {Role: "user", Content: "next"}}, source: core.TokenUsageSourceUnknown},
		{name: "old-known-latest-nil-estimated", messages: []core.AgentMessage{{Role: "assistant", Usage: old}, {Role: "assistant", Content: "answer"}}, estimator: true, source: core.TokenUsageSourceEstimated, output: 2},
		{name: "no-assistant", messages: []core.AgentMessage{{Role: "user", Content: "question"}}, source: core.TokenUsageSourceUnknown},
		{name: "latest-known-nonzero", messages: []core.AgentMessage{{Role: "assistant", Usage: old}, {Role: "user", Content: "next"}}, source: core.TokenUsageSourceReported, input: 100, output: 20, cache: 80, reasoning: 7, cacheReported: true, reasoningReported: true},
		{name: "latest-known-zero", messages: []core.AgentMessage{{Role: "assistant", Usage: old}, {Role: "assistant", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CacheReadInputTokensReported: true, ReasoningOutputTokensReported: true}}}, source: core.TokenUsageSourceReported, input: 10, output: 5, cacheReported: true, reasoningReported: true},
		{name: "latest-details-missing", messages: []core.AgentMessage{{Role: "assistant", Usage: old}, {Role: "assistant", Usage: &llm.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}}}, source: core.TokenUsageSourceReported, input: 10, output: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := core.NewAgentImpl("freshness", noopLoop{}, core.WithHistory(test.messages))
			assembly := &AgentAssembly{CoreRuntime: CoreRuntime{Agent: agent}}
			if test.estimator {
				assembly.Estimator = compaction.NewHeuristicTokenEstimator()
			}
			stream := core.NewEventStream(8)
			emitTokenUsageEvent(stream, assembly)
			event := <-stream.Events()
			require.NotNil(t, event.TokenUsage)
			assert.Equal(t, test.source, event.TokenUsage.UsageSource)
			assert.Equal(t, test.input, event.TokenUsage.InputTokens)
			assert.Equal(t, test.output, event.TokenUsage.OutputTokens)
			assert.Equal(t, test.cache, event.TokenUsage.CacheReadInputTokens)
			assert.Equal(t, test.reasoning, event.TokenUsage.ReasoningOutputTokens)
			assert.Equal(t, test.cacheReported, event.TokenUsage.CacheReadInputTokensReported)
			assert.Equal(t, test.reasoningReported, event.TokenUsage.ReasoningOutputTokensReported)
		})
	}
}

func TestUsageLocalRetryKeepsDistinctAttemptEvidence(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "scripted temporary failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"retry succeeded"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":7}}}`)
	}))
	defer server.Close()
	cfg := newE2ETestConfig(t, server.URL)
	assembly, err := AssembleAgent(context.Background(), cfg, cfg.Provider.Name, cfg.Provider.Model, io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	defer assembly.Cleanup()
	result, err := assembly.TurnRunner.RunTurn(context.Background(), core.Submission{Content: "retry fixture"})
	require.NoError(t, err)
	assert.Equal(t, "retry succeeded", result.Message)
	assert.EqualValues(t, 2, calls.Load())
	rows, summary := assembly.CostTracker.AttemptCostSnapshot()
	require.Len(t, rows, 2)
	assert.NotEmpty(t, rows[0].Result.AttemptID)
	assert.NotEmpty(t, rows[1].Result.AttemptID)
	assert.NotEqual(t, rows[0].Result.AttemptID, rows[1].Result.AttemptID)
	var failedID, successfulID llm.ProviderAttemptID
	for _, row := range rows {
		switch row.Result.Outcome {
		case llm.AttemptProviderFailure:
			failedID = row.Result.AttemptID
			assert.Equal(t, llm.UsageUnknown, row.Result.Usage.State)
		case llm.AttemptSuccess:
			successfulID = row.Result.AttemptID
			assert.Equal(t, llm.UsageKnown, row.Result.Usage.State)
		default:
			t.Errorf("unexpected retry attempt outcome %v", row.Result.Outcome)
		}
	}
	require.NotEmpty(t, failedID)
	require.NotEmpty(t, successfulID)
	assert.Equal(t, 1, summary.UnknownUsageAttempts)
	assert.Equal(t, 100, summary.KnownInputTokens)
	assert.Equal(t, 20, summary.KnownOutputTokens)
	stream := core.NewEventStream(8)
	emitTokenUsageEvent(stream, assembly)
	event := <-stream.Events()
	require.NotNil(t, event.TokenUsage)
	assert.Equal(t, core.TokenUsageScopeObservedAttempts, event.TokenUsage.UsageScope)
	assert.Equal(t, llm.UsageCoveragePartial, event.TokenUsage.PrimaryCoverage)
	assert.Equal(t, 1, event.TokenUsage.UnknownUsageAttempts)
	assert.Equal(t, 100, event.TokenUsage.InputTokens)
	assert.Equal(t, 20, event.TokenUsage.OutputTokens)
	t.Logf("local retry failed_attempt=%s usage=unknown successful_attempt=%s usage=known", failedID, successfulID)
}
