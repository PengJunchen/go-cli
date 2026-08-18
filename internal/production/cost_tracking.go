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

// CostSummary is a per-key cost breakdown (e.g. per sub-agent task ID). It
// accumulates the monetary cost, call count, and token usage for a discrete
// unit of work.
type CostSummary struct {
	// Cost is the accumulated monetary cost in USD.
	Cost float64
	// Calls is the number of model calls recorded.
	Calls int
	// TokensIn is the cumulative input tokens consumed.
	TokensIn int
	// TokensOut is the cumulative output tokens produced.
	TokensOut int
}

// SubagentCostRecord pairs a sub-agent task ID with its accumulated cost
// summary. It is a value type returned by SubagentCostSnapshot so callers
// receive an independent defensive copy that is safe to use without holding
// the tracker's lock.
type SubagentCostRecord struct {
	// TaskID is the sub-agent task identifier.
	TaskID string
	// CostSummary is the accumulated cost, calls, and token usage for the task.
	CostSummary
}

// BudgetExceededError is returned by CheckBudget (and surfaced from Record)
// when the accumulated cost exceeds the configured budget limit. A zero budget
// means no limit, in which case this error is never produced.
type BudgetExceededError struct {
	// Spent is the total cost accumulated so far.
	Spent float64
	// Budget is the configured spending cap.
	Budget float64
}

// Error implements the error interface.
func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("cost_tracker: budget exceeded: spent $%.4f, budget $%.4f", e.Spent, e.Budget)
}

// BudgetCallback is invoked after a Record call pushes the running total past
// the budget limit. It receives the current spent total and the configured
// budget. The callback is invoked without holding the tracker's mutex, so it
// may safely query the tracker (e.g. Total) or trigger external actions such
// as pausing processing.
type BudgetCallback func(spent, budget float64)

// CostTracker accumulates costs across a session. It is safe for concurrent
// use.
type CostTracker struct {
	mu             sync.Mutex
	tiers          map[string]CostTier
	total          float64
	calls          int
	subagentCosts  map[string]CostSummary
	budgetLimit    float64
	budgetCallback BudgetCallback
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
	return &CostTracker{tiers: tm, subagentCosts: make(map[string]CostSummary)}
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
// returns the cost of the call or an error if the model is unknown. After
// updating the total, Record checks the budget: when a positive budget limit is
// exceeded it fires the configured BudgetCallback (if any) and returns the
// recorded cost together with a *BudgetExceededError. A budget of 0 (the
// default) means no limit and Record never returns a budget error.
func (t *CostTracker) Record(model string, inputTokens, outputTokens int) (float64, error) {
	cost, err := t.CalculateCost(model, inputTokens, outputTokens)
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	t.total += cost
	t.calls++
	calls := t.calls
	total := t.total
	budget := t.budgetLimit
	cb := t.budgetCallback
	t.mu.Unlock()
	slog.Debug("production.cost_tracker.record",
		"model", model,
		"cost", cost,
		"calls", calls,
	)
	// Budget check after each record. A budget of 0 means no limit.
	if budget > 0 && total > budget {
		if cb != nil {
			cb(total, budget)
		}
		return cost, &BudgetExceededError{Spent: total, Budget: budget}
	}
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

// SetBudgetLimit configures the spending cap in USD. A limit of 0 (the default)
// means no limit. When the running total exceeds a positive limit, Record
// returns a *BudgetExceededError and fires the configured BudgetCallback, if
// any.
func (t *CostTracker) SetBudgetLimit(limit float64) {
	t.mu.Lock()
	t.budgetLimit = limit
	t.mu.Unlock()
}

// SetBudgetCallback registers a callback invoked when a Record call pushes the
// running total past the configured budget. Set to nil to disable. The callback
// is invoked without holding the tracker's mutex.
func (t *CostTracker) SetBudgetCallback(cb BudgetCallback) {
	t.mu.Lock()
	t.budgetCallback = cb
	t.mu.Unlock()
}

// CheckBudget returns a *BudgetExceededError if the accumulated total exceeds
// the configured budget limit. It returns nil when the budget is 0 (no limit)
// or when the total is within budget. CheckBudget is side-effect free: it does
// not invoke the BudgetCallback.
func (t *CostTracker) CheckBudget() error {
	t.mu.Lock()
	total := t.total
	budget := t.budgetLimit
	t.mu.Unlock()
	if budget <= 0 {
		return nil
	}
	if total > budget {
		return &BudgetExceededError{Spent: total, Budget: budget}
	}
	return nil
}

// RecordSubagent records a sub-agent's token usage under the given taskID,
// accumulating cost, calls, and tokens separately from the main session.
func (t *CostTracker) RecordSubagent(taskID, model string, inputTokens, outputTokens int) (float64, error) {
	cost, err := t.CalculateCost(model, inputTokens, outputTokens)
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	entry := t.subagentCosts[taskID]
	entry.Cost += cost
	entry.Calls++
	entry.TokensIn += inputTokens
	entry.TokensOut += outputTokens
	t.subagentCosts[taskID] = entry
	t.mu.Unlock()
	slog.Debug("production.cost_tracker.record_subagent",
		"task_id", taskID,
		"model", model,
		"cost", cost,
		"calls", entry.Calls,
	)
	return cost, nil
}

// SubagentCostSnapshot returns a defensive copy of the current sub-agent cost
// records. The returned slice and its elements are safe to use without holding
// any lock; mutations to the returned slice do not affect the tracker. The
// snapshot is taken under the same mutex used by RecordSubagent, so it is safe
// to call concurrently with writers.
func (t *CostTracker) SubagentCostSnapshot() []SubagentCostRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SubagentCostRecord, 0, len(t.subagentCosts))
	for taskID, summary := range t.subagentCosts {
		out = append(out, SubagentCostRecord{TaskID: taskID, CostSummary: summary})
	}
	return out
}

// SubagentTotal returns the aggregate cost across all recorded sub-agent
// calls.
func (t *CostTracker) SubagentTotal() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	var sum float64
	for _, s := range t.subagentCosts {
		sum += s.Cost
	}
	return sum
}

// SubagentCalls returns the total number of sub-agent model calls recorded.
func (t *CostTracker) SubagentCalls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	var sum int
	for _, s := range t.subagentCosts {
		sum += s.Calls
	}
	return sum
}
