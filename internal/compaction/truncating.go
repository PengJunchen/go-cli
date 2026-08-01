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

	// Add newest non-system entries first, dropping the oldest. An entry that
	// does not fit the remaining budget on its own is skipped so the total stays
	// within the limit.
	for i := len(nonSystem) - 1; i >= 0; i-- {
		n := estimateTokens([]TurnItem{nonSystem[i]}, estimator)
		if n <= budget {
			result = append(result, nonSystem[i])
			budget -= n
		}
	}

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
