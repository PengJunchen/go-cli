package production

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestAttemptUsageAggregateOverflowKeepsRowsAndCost(t *testing.T) {
	tracker := NewCostTracker(nil)
	for i, count := range []int{math.MaxInt, 1} {
		result := llm.ProviderAttemptResult{
			OperationID: "op", AttemptID: llm.ProviderAttemptID(string(rune('a' + i))),
			Identity: llm.AttemptIdentity{ProviderID: "fixture", ModelID: "gpt-4o", ProtocolID: llm.ProtocolOpenAICompatible, OriginKind: llm.OriginExplicitRequestEndpoint, Candidate: "c"},
			Outcome:  llm.AttemptSuccess,
			Usage:    llm.UsageEvidence{State: llm.UsageKnown, Source: llm.UsageProviderResponse, Usage: &llm.Usage{InputTokens: count, TotalTokens: count, PrimaryCoverage: llm.UsageCoverageComplete, CacheReadInputTokens: count, CacheReadInputTokensReported: true}},
		}
		_, err := tracker.RecordAttempt(result)
		require.NoError(t, err)
	}
	rows, summary := tracker.AttemptCostSnapshot()
	require.Len(t, rows, 2)
	assert.Equal(t, llm.UsageKnown, rows[0].Result.Usage.State)
	assert.Equal(t, llm.UsageKnown, rows[1].Result.Usage.State)
	assert.Equal(t, math.MaxInt, rows[0].Result.Usage.Usage.InputTokens)
	assert.Equal(t, 1, rows[1].Result.Usage.Usage.InputTokens)
	assert.Equal(t, math.MaxInt, summary.KnownInputTokens)
	assert.Zero(t, summary.UnknownUsageAttempts, "original attempts remain known")
	assert.Equal(t, 2, summary.Attempts)
	assert.Equal(t, math.MaxInt, summary.KnownCacheReadInputTokens)
	assert.Equal(t, 1, summary.CacheReadKnownAttempts)
	assert.Equal(t, 1, summary.CacheReadUnknownAttempts)
	assert.InDelta(t, rows[0].CostUSD+rows[1].CostUSD, summary.KnownCostSubtotal, 1)
	data, err := json.Marshal(summary)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, true, got["PrimarySubtotalIncomplete"])
}

func TestAttemptUsageUnrepresentableOnlyHasNoKnownSubtotal(t *testing.T) {
	tracker := NewCostTracker(nil)
	result := llm.ProviderAttemptResult{
		OperationID: "op", AttemptID: "attempt",
		Identity: llm.AttemptIdentity{ProviderID: "fixture", ModelID: "gpt-4o", ProtocolID: llm.ProtocolOpenAICompatible, OriginKind: llm.OriginExplicitRequestEndpoint, Candidate: "c"},
		Outcome:  llm.AttemptSuccess,
		Usage:    llm.UsageEvidence{State: llm.UsageKnown, Source: llm.UsageProviderResponse, Usage: &llm.Usage{InputTokens: math.MaxInt, OutputTokens: 1, TotalTokens: math.MaxInt, PrimaryCoverage: llm.UsageCoverageComplete}},
	}
	_, err := tracker.RecordAttempt(result)
	require.NoError(t, err)
	rows, summary := tracker.AttemptCostSnapshot()
	require.Len(t, rows, 1)
	assert.Equal(t, llm.UsageKnown, rows[0].Result.Usage.State)
	assert.Equal(t, math.MaxInt, rows[0].Result.Usage.Usage.InputTokens)
	assert.Zero(t, summary.KnownInputTokens)
	assert.Zero(t, summary.KnownOutputTokens)
	assert.True(t, summary.PrimarySubtotalIncomplete)
	assert.Zero(t, summary.UnknownUsageAttempts)
	data, err := json.Marshal(summary)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, float64(0), got["KnownPrimarySubtotalAttempts"])
}
