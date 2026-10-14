package production

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// AttemptCostStatus distinguishes reported, estimated, and unknown attempt cost.
type AttemptCostStatus uint8

const (
	// AttemptCostUnknown means the attempt has no authoritative cost or usable price.
	AttemptCostUnknown AttemptCostStatus = iota + 1
	// AttemptCostProviderReported means the provider supplied an explicit cost.
	AttemptCostProviderReported
	// AttemptCostEstimated means the tracker calculated cost from known usage and a price tier.
	AttemptCostEstimated
)

// AttemptCostPriceSource identifies the source used to report an attempt cost.
type AttemptCostPriceSource uint8

const (
	// AttemptCostPriceUnknown means no cost source is available.
	AttemptCostPriceUnknown AttemptCostPriceSource = iota + 1
	// AttemptCostFromProvider means the provider supplied the cost directly.
	AttemptCostFromProvider
	// AttemptCostFromDefaultTier means the estimate used a built-in default price tier.
	AttemptCostFromDefaultTier
	// AttemptCostFromConfiguredTier means the estimate used a caller-configured price tier.
	AttemptCostFromConfiguredTier
)

// AttemptCostRecord retains one immutable provider result and its cost evidence.
type AttemptCostRecord struct {
	Result      llm.ProviderAttemptResult
	Status      AttemptCostStatus
	PriceSource AttemptCostPriceSource
	CostUSD     float64
}

// AttemptCostSummary contains attempt-only and combined known subtotals plus
// explicit unknown counts and legacy-record provenance.
type AttemptCostSummary struct {
	Attempts          int
	KnownInputTokens  int
	KnownOutputTokens int
	// KnownCostSubtotal covers attempt records only.
	KnownCostSubtotal    float64
	UnknownUsageAttempts int
	UnknownCostAttempts  int
	// LegacyCalls counts old Record writes that have no provider attempt identity.
	LegacyCalls int
	// LegacyKnownCostSubtotal is the known amount from old Record writes.
	LegacyKnownCostSubtotal float64
	// LegacyPriceSource is the configured pricing source for legacy records, or unknown if none exist.
	LegacyPriceSource AttemptCostPriceSource
	// CombinedKnownCostSubtotal includes legacy records and known attempt costs exactly once.
	CombinedKnownCostSubtotal float64
}

var (
	// ErrInvalidAttemptResult means a dispatched result lacks valid typed evidence.
	ErrInvalidAttemptResult = errors.New("cost_tracker: invalid provider attempt result")
	// ErrAttemptResultConflict means one AttemptID was reused for different terminal evidence.
	ErrAttemptResultConflict = errors.New("cost_tracker: conflicting provider attempt result")
)

// RecordAttempt records an immutable attempt once by AttemptID. Unknown usage
// and unpriced known usage remain explicit unknown-cost records; they do not
// increment the known cost subtotal. This path does not apply the legacy hard
// budget callback.
func (t *CostTracker) RecordAttempt(result llm.ProviderAttemptResult) (AttemptCostRecord, error) {
	result = cloneAttemptResult(result)
	if err := validateAttemptResult(result); err != nil {
		return AttemptCostRecord{}, err
	}
	id := string(result.AttemptID)
	t.mu.Lock()
	defer t.mu.Unlock()
	if previous, ok := t.attemptRecords[id]; ok {
		if !sameAttemptResult(previous.Result, result) {
			return AttemptCostRecord{}, fmt.Errorf("%w: %s", ErrAttemptResultConflict, id)
		}
		return cloneAttemptCostRecord(previous), nil
	}

	record := AttemptCostRecord{
		Result:      result,
		Status:      AttemptCostUnknown,
		PriceSource: AttemptCostPriceUnknown,
	}
	if result.ProviderReportedCostUSD != nil {
		record.Status = AttemptCostProviderReported
		record.PriceSource = AttemptCostFromProvider
		record.CostUSD = *result.ProviderReportedCostUSD
	} else if result.Usage.State == llm.UsageKnown {
		if tier, ok := t.tiers[result.Identity.ModelID]; ok && validTier(tier) {
			usage := result.Usage.Usage
			record.Status = AttemptCostEstimated
			record.PriceSource = t.priceSource
			record.CostUSD = float64(usage.InputTokens)*tier.InputPer1K/1000 + float64(usage.OutputTokens)*tier.OutputPer1K/1000
			if !finiteNonnegative(record.CostUSD) {
				record.Status = AttemptCostUnknown
				record.PriceSource = AttemptCostPriceUnknown
				record.CostUSD = 0
			}
		}
	}
	knownCostSubtotal := t.attemptSummary.KnownCostSubtotal
	knownTotal := t.total
	if record.Status != AttemptCostUnknown {
		knownCostSubtotal += record.CostUSD
		knownTotal += record.CostUSD
		if !finiteNonnegative(knownCostSubtotal) || !finiteNonnegative(knownTotal) {
			return AttemptCostRecord{}, fmt.Errorf("%w: accumulated known cost is not finite", ErrInvalidAttemptResult)
		}
	}

	t.attemptRecords[id] = cloneAttemptCostRecord(record)
	t.attemptSummary.Attempts++
	t.calls++
	if result.Usage.State == llm.UsageKnown {
		t.attemptSummary.KnownInputTokens += result.Usage.Usage.InputTokens
		t.attemptSummary.KnownOutputTokens += result.Usage.Usage.OutputTokens
	} else {
		t.attemptSummary.UnknownUsageAttempts++
	}
	if record.Status == AttemptCostUnknown {
		t.attemptSummary.UnknownCostAttempts++
	} else {
		t.attemptSummary.KnownCostSubtotal = knownCostSubtotal
		t.total = knownTotal
	}
	return cloneAttemptCostRecord(record), nil
}

// AttemptCostSnapshot returns sorted defensive copies of attempt cost records
// and the corresponding known/unknown summary.
func (t *CostTracker) AttemptCostSnapshot() ([]AttemptCostRecord, AttemptCostSummary) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ids := make([]string, 0, len(t.attemptRecords))
	for id := range t.attemptRecords {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]AttemptCostRecord, 0, len(ids))
	for _, id := range ids {
		rows = append(rows, cloneAttemptCostRecord(t.attemptRecords[id]))
	}
	summary := t.attemptSummary
	summary.LegacyCalls = t.legacyCalls
	summary.LegacyKnownCostSubtotal = t.legacyTotal
	summary.CombinedKnownCostSubtotal = t.total
	summary.LegacyPriceSource = AttemptCostPriceUnknown
	if t.legacyCalls > 0 {
		summary.LegacyPriceSource = t.priceSource
	}
	return rows, summary
}

func validateAttemptResult(result llm.ProviderAttemptResult) error {
	if result.OperationID == "" || result.AttemptID == "" ||
		result.Identity.ProviderID == "" || result.Identity.ModelID == "" ||
		result.Identity.ProtocolID == "" || result.Identity.Candidate == "" ||
		(result.Identity.OriginKind != llm.OriginResolverDefaultEndpoint && result.Identity.OriginKind != llm.OriginExplicitRequestEndpoint) {
		return fmt.Errorf("%w: missing operation, attempt, or resolved candidate identity", ErrInvalidAttemptResult)
	}
	if result.Outcome < llm.AttemptSuccess || result.Outcome > llm.AttemptCancelled || result.Failure > llm.FailureOther {
		return fmt.Errorf("%w: invalid outcome or failure class", ErrInvalidAttemptResult)
	}
	if result.Outcome == llm.AttemptSuccess && result.Failure != llm.FailureNone {
		return fmt.Errorf("%w: successful result has a failure class", ErrInvalidAttemptResult)
	}
	if result.Outcome != llm.AttemptSuccess && result.Failure == llm.FailureNone {
		return fmt.Errorf("%w: failed result has no failure class", ErrInvalidAttemptResult)
	}
	switch result.Usage.State {
	case llm.UsageKnown:
		if result.Usage.Usage == nil || result.Usage.Source != llm.UsageProviderResponse {
			return fmt.Errorf("%w: known usage requires provider response evidence", ErrInvalidAttemptResult)
		}
		if result.Usage.Usage.InputTokens < 0 || result.Usage.Usage.OutputTokens < 0 || result.Usage.Usage.TotalTokens < 0 {
			return fmt.Errorf("%w: token counts cannot be negative", ErrInvalidAttemptResult)
		}
	case llm.UsageUnknown:
		if result.Usage.Usage != nil || result.Usage.Source != llm.UsageNoAuthoritativeUsage {
			return fmt.Errorf("%w: unknown usage must be explicit and contain no token counts", ErrInvalidAttemptResult)
		}
	default:
		return fmt.Errorf("%w: usage state must be explicit", ErrInvalidAttemptResult)
	}
	if result.ProviderReportedCostUSD != nil && !finiteNonnegative(*result.ProviderReportedCostUSD) {
		return fmt.Errorf("%w: reported cost must be finite and non-negative", ErrInvalidAttemptResult)
	}
	return nil
}

func validTier(tier CostTier) bool {
	return finiteNonnegative(tier.InputPer1K) && finiteNonnegative(tier.OutputPer1K)
}

func finiteNonnegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func cloneAttemptResult(result llm.ProviderAttemptResult) llm.ProviderAttemptResult {
	clone := result
	if result.Usage.Usage != nil {
		usage := *result.Usage.Usage
		clone.Usage.Usage = &usage
	}
	if result.ProviderReportedCostUSD != nil {
		cost := *result.ProviderReportedCostUSD
		clone.ProviderReportedCostUSD = &cost
	}
	return clone
}

func cloneAttemptCostRecord(record AttemptCostRecord) AttemptCostRecord {
	clone := record
	clone.Result = cloneAttemptResult(record.Result)
	return clone
}

func sameAttemptResult(left, right llm.ProviderAttemptResult) bool {
	if left.OperationID != right.OperationID || left.AttemptID != right.AttemptID ||
		left.Identity != right.Identity || left.Outcome != right.Outcome || left.Failure != right.Failure ||
		left.Usage.State != right.Usage.State || left.Usage.Source != right.Usage.Source {
		return false
	}
	if (left.Usage.Usage == nil) != (right.Usage.Usage == nil) {
		return false
	}
	if left.Usage.Usage != nil && *left.Usage.Usage != *right.Usage.Usage {
		return false
	}
	if (left.ProviderReportedCostUSD == nil) != (right.ProviderReportedCostUSD == nil) {
		return false
	}
	return left.ProviderReportedCostUSD == nil || *left.ProviderReportedCostUSD == *right.ProviderReportedCostUSD
}
