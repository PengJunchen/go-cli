package compaction

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// TruncatingCompactor is the last-resort compaction strategy. It preserves every
// system entry, then keeps the newest non-system entries until the remaining
// budget is exhausted, dropping the oldest entries. It always returns a list
// that or fits the budget (or the best achievable subset when the system prompt
// itself exceeds it), and it never panics on empty input.
type TruncatingCompactor struct{}

// Compile-time assertion that TruncatingCompactor satisfies Compactor.
var _ Compactor = (*TruncatingCompactor)(nil)

// NewTruncatingCompactor returns a zero-configuration truncating compactor.
func NewTruncatingCompactor() *TruncatingCompactor {
	return &TruncatingCompactor{}
}

// Compact keeps all system entries and the newest non-system entries that fit
// the budget.
func (c *TruncatingCompactor) Compact(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator) ([]TurnItem, error) {
	span, _ := tracing.SpanFromContext(ctx, "compaction.truncating", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	tokensBefore := estimateTokens(items, estimator)
	span.SetAttributes(
		tracing.Attribute{Key: "items_in", Value: len(items)},
		tracing.Attribute{Key: "tokens_before", Value: tokensBefore},
	)

	result := make([]TurnItem, 0, len(items))
	system := make([]TurnItem, 0, 4)
	nonSystem := make([]TurnItem, 0, len(items))

	for _, it := range items {
		if it.Role == RoleSystem {
			system = append(system, it)
		} else {
			nonSystem = append(nonSystem, it)
		}
	}

	result = append(result, system...)
	budget := maxTokens - estimateTokens(result, estimator)

	// Keep the newest non-system entries that fit the budget, dropping the
	// oldest. We iterate newest-first so the most recent entries win the
	// remaining budget, but collect them into a temporary slice and reverse
	// before appending so the result stays in chronological order.
	kept := make([]TurnItem, 0, len(nonSystem))
	for i := len(nonSystem) - 1; i >= 0; i-- {
		n := estimateTokens([]TurnItem{nonSystem[i]}, estimator)
		if n <= budget {
			kept = append(kept, nonSystem[i])
			budget -= n
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	kept = removeDanglingToolResults(kept)
	result = append(result, kept...)

	tokensAfter := estimateTokens(result, estimator)
	span.SetAttributes(
		tracing.Attribute{Key: "items_out", Value: len(result)},
		tracing.Attribute{Key: "tokens_after", Value: tokensAfter},
	)

	logger.Info("compaction.truncating.done",
		"items_in", len(items),
		"items_out", len(result),
		"tokens_before", tokensBefore,
		"tokens_after", tokensAfter)
	return result, nil
}

func removeDanglingToolResults(items []TurnItem) []TurnItem {
	if len(items) == 0 {
		return items
	}
	out := make([]TurnItem, 0, len(items))
	for _, it := range items {
		if it.Role == RoleTool && (len(out) == 0 || (out[len(out)-1].Role != RoleAssistant && out[len(out)-1].Role != RoleTool)) {
			continue
		}
		out = append(out, it)
	}
	return out
}
