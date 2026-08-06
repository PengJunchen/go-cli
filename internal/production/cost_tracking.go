package production

import (
	"fmt"
	"log/slog"
	"sync"
)

// CostCalculator computes the monetary cost of model calls.
type CostCalculator interface {
	// CalculateCost returns the USD cost of a single model call given the
	// model name and the input/output token counts.
	CalculateCost(model string, inputTokens, outputTokens int) (float64, error)
}

// CostTier defines pricing for a model. Prices are expressed as USD per 1K
// tokens.
type CostTier struct {
	// Model is the model identifier the tier applies to.
	Model string
	// InputPer1K is the USD price per 1K input tokens.
	InputPer1K float64
	// OutputPer1K is the USD price per 1K output tokens.
	OutputPer1K float64
}

// DefaultCostTiers holds pricing for common models. Prices are approximate
// list prices in USD per 1K tokens and are intended for rough cost estimation,
// not billing.
var DefaultCostTiers = []CostTier{
	{Model: "gpt-4o", InputPer1K: 0.0025, OutputPer1K: 0.01},
	{Model: "gpt-4o-mini", InputPer1K: 0.00015, OutputPer1K: 0.0006},
	{Model: "gpt-4-turbo", InputPer1K: 0.01, OutputPer1K: 0.03},
	{Model: "gpt-3.5-turbo", InputPer1K: 0.0005, OutputPer1K: 0.0015},
	{Model: "claude-sonnet-4", InputPer1K: 0.003, OutputPer1K: 0.015},
	{Model: "claude-opus-4", InputPer1K: 0.015, OutputPer1K: 0.075},
	{Model: "claude-haiku-3.5", InputPer1K: 0.0008, OutputPer1K: 0.004},
}

// CostTracker accumulates costs across a session. It is safe for concurrent
// use.
type CostTracker struct {
	mu    sync.Mutex
	tiers map[string]CostTier
	total float64
	calls int
}

// Compile-time assertion that CostTracker satisfies CostCalculator.
var _ CostCalculator = (*CostTracker)(nil)

// NewCostTracker builds a CostTracker from the given tiers. If tiers is empty
// or nil, DefaultCostTiers is used.
func NewCostTracker(tiers []CostTier) *CostTracker {
	if len(tiers) == 0 {
		tiers = DefaultCostTiers
	}
	tm := make(map[string]CostTier, len(tiers))
	for _, t := range tiers {
		tm[t.Model] = t
	}
	slog.Debug("production.cost_tracker.new", "tiers", len(tm))
	return &CostTracker{tiers: tm}
}

// CalculateCost returns the cost of a single model call. It returns an error
// if the model has no registered tier.
func (t *CostTracker) CalculateCost(model string, inputTokens, outputTokens int) (float64, error) {
	tier, ok := t.tiers[model]
	if !ok {
		return 0, fmt.Errorf("cost_tracker: no pricing tier for model %q", model)
	}
	cost := float64(inputTokens)*tier.InputPer1K/1000 + float64(outputTokens)*tier.OutputPer1K/1000
	slog.Debug("production.cost_tracker.calculate",
		"model", model,
		"input_tokens", inputTokens,
		"output_tokens", outputTokens,
		"cost", cost,
	)
	return cost, nil
}

// Record calculates the cost of a call and adds it to the running total. It
// returns the cost of the call or an error if the model is unknown.
func (t *CostTracker) Record(model string, inputTokens, outputTokens int) (float64, error) {
	cost, err := t.CalculateCost(model, inputTokens, outputTokens)
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	t.total += cost
	t.calls++
	calls := t.calls
	t.mu.Unlock()
	slog.Debug("production.cost_tracker.record",
		"model", model,
		"cost", cost,
		"calls", calls,
	)
	return cost, nil
}

// Total returns the accumulated cost across all recorded calls.
func (t *CostTracker) Total() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total
}

// Calls returns the number of recorded calls.
func (t *CostTracker) Calls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}
