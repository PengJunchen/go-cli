package production

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
)

func TestCostTrackerRecordAttemptRetainsKnownSubtotalAndUnknownCounts(t *testing.T) {
	tracker := NewCostTracker([]CostTier{{Model: "priced-model", InputPer1K: 2, OutputPer1K: 4}})

	known := attemptResult("operation-a", "attempt-a", "priced-model", llm.UsageEvidence{
		State:  llm.UsageKnown,
		Usage:  &llm.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150},
		Source: llm.UsageProviderResponse,
	})
	knownRecord, err := tracker.RecordAttempt(known)
	require.NoError(t, err)
	assert.Equal(t, AttemptCostEstimated, knownRecord.Status)
	assert.Equal(t, AttemptCostFromConfiguredTier, knownRecord.PriceSource)
	assert.InDelta(t, 0.4, knownRecord.CostUSD, 0.000001)

	unknown := attemptResult("operation-a", "attempt-b", "priced-model", llm.UnknownUsageEvidence())
	unknownRecord, err := tracker.RecordAttempt(unknown)
	require.NoError(t, err)
	assert.Equal(t, AttemptCostUnknown, unknownRecord.Status)
	assert.Equal(t, AttemptCostPriceUnknown, unknownRecord.PriceSource)
	assert.Zero(t, unknownRecord.CostUSD)

	assert.InDelta(t, 0.4, tracker.Total(), 0.000001, "Total is the known subtotal and must not charge unknown usage as zero-cost evidence")
	assert.Equal(t, 2, tracker.Calls(), "unknown attempts still count as actual calls")
	rows, summary := tracker.AttemptCostSnapshot()
	require.Len(t, rows, 2)
	assert.Equal(t, 2, summary.Attempts)
	assert.InDelta(t, 0.4, summary.KnownCostSubtotal, 0.000001)
	assert.Equal(t, 1, summary.UnknownUsageAttempts)
	assert.Equal(t, 1, summary.UnknownCostAttempts)
	assert.Equal(t, 100, summary.KnownInputTokens)
	assert.Equal(t, 50, summary.KnownOutputTokens)
}

func TestCostTrackerMixedLegacyAndAttemptSnapshotSeparatesAndCombinesTotals(t *testing.T) {
	tracker := NewCostTracker(nil)
	legacyCost, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)
	known := attemptResult("mixed-operation", "mixed-known", "gpt-4o", llm.UsageEvidence{
		State:  llm.UsageKnown,
		Usage:  &llm.Usage{InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500},
		Source: llm.UsageProviderResponse,
	})
	_, err = tracker.RecordAttempt(known)
	require.NoError(t, err)
	_, err = tracker.RecordAttempt(known)
	require.NoError(t, err, "duplicate attempt evidence must not affect combined totals")
	unknown := attemptResult("mixed-operation", "mixed-unknown", "unpriced-model", llm.UnknownUsageEvidence())
	unknown.Outcome = llm.AttemptTransportFailure
	unknown.Failure = llm.FailureTransientProvider
	_, err = tracker.RecordAttempt(unknown)
	require.NoError(t, err)

	rows, summary := tracker.AttemptCostSnapshot()
	require.Len(t, rows, 2)
	assert.InDelta(t, legacyCost, summary.LegacyKnownCostSubtotal, 0.000001)
	assert.Equal(t, 1, summary.LegacyCalls)
	assert.Equal(t, AttemptCostFromDefaultTier, summary.LegacyPriceSource)
	assert.InDelta(t, 0.0075, summary.KnownCostSubtotal, 0.000001,
		"the attempt-only subtotal remains scoped to attempt records")
	assert.InDelta(t, 0.015, summary.CombinedKnownCostSubtotal, 0.000001,
		"combined subtotal must include the legacy record and known attempt exactly once")
	assert.InDelta(t, summary.CombinedKnownCostSubtotal, tracker.Total(), 0.000001)
	assert.Equal(t, 1, summary.UnknownCostAttempts, "unknown attempt cost must not enter the combined subtotal")
	assert.Equal(t, 3, tracker.Calls())
}

func TestCostTrackerRecordAttemptDistinguishesProviderCostAndUnknownPrice(t *testing.T) {
	tracker := NewCostTracker([]CostTier{{Model: "known-model", InputPer1K: 1, OutputPer1K: 1}})
	reportedCost := 0.125
	providerReported := attemptResult("operation-a", "attempt-reported", "unpriced-model", llm.UnknownUsageEvidence())
	providerReported.ProviderReportedCostUSD = &reportedCost
	reportedRecord, err := tracker.RecordAttempt(providerReported)
	require.NoError(t, err)
	assert.Equal(t, AttemptCostProviderReported, reportedRecord.Status)
	assert.Equal(t, AttemptCostFromProvider, reportedRecord.PriceSource)
	assert.InDelta(t, reportedCost, tracker.Total(), 0.000001)

	knownButUnpriced := attemptResult("operation-a", "attempt-unpriced", "unpriced-model", llm.UsageEvidence{
		State:  llm.UsageKnown,
		Usage:  &llm.Usage{InputTokens: 12, OutputTokens: 7, TotalTokens: 19},
		Source: llm.UsageProviderResponse,
	})
	unpricedRecord, err := tracker.RecordAttempt(knownButUnpriced)
	require.NoError(t, err)
	assert.Equal(t, AttemptCostUnknown, unpricedRecord.Status)
	assert.Equal(t, AttemptCostPriceUnknown, unpricedRecord.PriceSource)

	_, summary := tracker.AttemptCostSnapshot()
	assert.Equal(t, 2, summary.Attempts)
	assert.Equal(t, 1, summary.UnknownUsageAttempts)
	assert.Equal(t, 1, summary.UnknownCostAttempts, "known usage without a tier has unknown cost")
	assert.InDelta(t, reportedCost, summary.KnownCostSubtotal, 0.000001)
}

func TestCostTrackerRecordAttemptIsIdempotentByAttemptID(t *testing.T) {
	tracker := NewCostTracker([]CostTier{{Model: "priced-model", InputPer1K: 1, OutputPer1K: 2}})
	result := attemptResult("operation-a", "attempt-one", "priced-model", llm.UsageEvidence{
		State:  llm.UsageKnown,
		Usage:  &llm.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7},
		Source: llm.UsageProviderResponse,
	})
	first, err := tracker.RecordAttempt(result)
	require.NoError(t, err)
	second, err := tracker.RecordAttempt(result)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, tracker.Calls())
	assert.InDelta(t, first.CostUSD, tracker.Total(), 0.000001)

	changed := result
	changed.Usage = llm.UsageEvidence{
		State:  llm.UsageKnown,
		Usage:  &llm.Usage{InputTokens: 30, OutputTokens: 40, TotalTokens: 70},
		Source: llm.UsageProviderResponse,
	}
	_, err = tracker.RecordAttempt(changed)
	require.ErrorIs(t, err, ErrAttemptResultConflict)
	assert.Equal(t, 1, tracker.Calls(), "reusing an AttemptID with different terminal evidence must not alter totals")

	rows, summary := tracker.AttemptCostSnapshot()
	require.Len(t, rows, 1)
	assert.Equal(t, 3, rows[0].Result.Usage.Usage.InputTokens)
	assert.Equal(t, 1, summary.Attempts)
}

func TestCostTrackerRecordAttemptConcurrentDuplicateIsIdempotent(t *testing.T) {
	tracker := NewCostTracker([]CostTier{{Model: "priced-model", InputPer1K: 1, OutputPer1K: 2}})
	result := attemptResult("operation-concurrent", "attempt-concurrent", "priced-model", llm.UsageEvidence{
		State:  llm.UsageKnown,
		Usage:  &llm.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7},
		Source: llm.UsageProviderResponse,
	})
	const callers = 16
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tracker.RecordAttempt(result)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, 1, tracker.Calls())
	assert.InDelta(t, 0.011, tracker.Total(), 0.000001)
	_, summary := tracker.AttemptCostSnapshot()
	assert.Equal(t, 1, summary.Attempts)
}

func TestCostTrackerRecordAttemptRejectsImplicitUnknownAndInvalidCost(t *testing.T) {
	tracker := NewCostTracker(nil)
	missingUsageState := attemptResult("operation-a", "attempt-missing-state", "gpt-4o-mini", llm.UsageEvidence{})
	_, err := tracker.RecordAttempt(missingUsageState)
	require.ErrorIs(t, err, ErrInvalidAttemptResult)

	negativeUsage := attemptResult("operation-a", "attempt-negative-usage", "gpt-4o-mini", llm.UsageEvidence{
		State:  llm.UsageKnown,
		Usage:  &llm.Usage{InputTokens: -1, OutputTokens: 0},
		Source: llm.UsageProviderResponse,
	})
	_, err = tracker.RecordAttempt(negativeUsage)
	require.ErrorIs(t, err, ErrInvalidAttemptResult)

	negativeCost := 0.1
	negativeCost = -negativeCost
	invalidCost := attemptResult("operation-a", "attempt-negative-cost", "gpt-4o-mini", llm.UnknownUsageEvidence())
	invalidCost.ProviderReportedCostUSD = &negativeCost
	_, err = tracker.RecordAttempt(invalidCost)
	require.ErrorIs(t, err, ErrInvalidAttemptResult)
	assert.Zero(t, tracker.Calls())
}

func attemptResult(operation, attempt, model string, usage llm.UsageEvidence) llm.ProviderAttemptResult {
	return llm.ProviderAttemptResult{
		OperationID: llm.ProviderOperationID(operation),
		AttemptID:   llm.ProviderAttemptID(attempt),
		Identity: llm.AttemptIdentity{
			ProviderID: "provider",
			ModelID:    model,
			ProtocolID: llm.ProtocolOpenAICompatible,
			OriginKind: llm.OriginResolverDefaultEndpoint,
			Candidate:  llm.CandidateID("candidate-" + attempt),
		},
		Outcome: llm.AttemptSuccess,
		Usage:   usage,
	}
}
