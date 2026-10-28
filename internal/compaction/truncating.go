package compaction

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// TruncatingCompactor is the last-resort compaction strategy. It preserves every
// system entry, then keeps the newest non-system entries until the remaining
// budget is exhausted, dropping the oldest entries. It rejects input when the
// mandatory system prompt itself exceeds the budget and never panics on empty
// input.
type TruncatingCompactor struct{}

// Compile-time assertion that TruncatingCompactor satisfies Compactor.
var _ Compactor = (*TruncatingCompactor)(nil)

// NewTruncatingCompactor returns a zero-configuration truncating compactor.
func NewTruncatingCompactor() *TruncatingCompactor {
	return &TruncatingCompactor{}
}

// Compact keeps all system entries and the newest non-system entries that fit
// the budget.
func (c *TruncatingCompactor) Compact(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator) (CompactionResult, error) {
	if err := ctx.Err(); err != nil {
		return CompactionResult{Items: items}, err
	}
	span, _ := tracing.SpanFromContext(ctx, "compaction.truncating", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	if err := validateDurableCompactionInput(items); err != nil {
		return CompactionResult{Items: items}, err
	}
	normalized, groups, err := groupTurnItems(items)
	if err != nil {
		return CompactionResult{}, err
	}
	if err := checkMandatorySystemPromptBudget(normalized, maxTokens, estimator); err != nil {
		return CompactionResult{Items: items}, err
	}

	tokensBefore := estimateTokens(normalized, estimator)
	span.SetAttributes(
		tracing.Attribute{Key: "items_in", Value: len(normalized)},
		tracing.Attribute{Key: "tokens_before", Value: tokensBefore},
	)

	if hasDurableSource(normalized) {
		return truncateDurablePrefix(normalized, groups, maxTokens, estimator)
	}

	result := make([]TurnItem, 0, len(normalized))
	system := make([]TurnItem, 0, 4)
	nonSystem := make([]itemGroup, 0, len(groups))

	for _, group := range groups {
		if group.system {
			system = append(system, items[group.start:group.end]...)
		} else {
			nonSystem = append(nonSystem, group)
		}
	}

	result = append(result, system...)
	budget := maxTokens - mandatorySystemPromptTokens(normalized, estimator)

	// Keep the newest non-system entries that fit the budget, dropping the
	// oldest. We iterate newest-first so the most recent entries win the
	// remaining budget, but collect them into a temporary slice and reverse
	// before appending so the result stays in chronological order.
	kept := make([]itemGroup, 0, len(nonSystem))
	for i := len(nonSystem) - 1; i >= 0; i-- {
		group := nonSystem[i]
		n := estimateTokens(normalized[group.start:group.end], estimator)
		if n <= budget {
			kept = append(kept, group)
			budget -= n
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	for _, group := range kept {
		result = append(result, normalized[group.start:group.end]...)
	}

	tokensAfter := estimateTokensCopy(result, estimator)
	span.SetAttributes(
		tracing.Attribute{Key: "items_out", Value: len(result)},
		tracing.Attribute{Key: "tokens_after", Value: tokensAfter},
	)

	logger.Info("compaction.truncating.done",
		"items_in", len(items),
		"items_out", len(result),
		"tokens_before", tokensBefore,
		"tokens_after", tokensAfter)
	return CompactionResult{Items: result, Strategy: StrategyTruncating}, nil
}
