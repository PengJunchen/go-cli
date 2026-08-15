package compaction

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// compactedToolResult is the placeholder substituted in place of an old tool
// result. It is short and constant so repeated compaction converges to a small,
// predictable size regardless of how verbose the original output was.
const compactedToolResult = "[compacted tool result]"

// MicroCompactor is the cheapest compaction strategy: it performs no LLM calls.
// It shrinks the context by replacing the results of older tool calls with a
// fixed placeholder while preserving user and assistant messages verbatim. When
// even fully compacted tool results cannot bring the list under budget, it
// returns ErrRequiresTruncating so a caller can fall back to a heavier strategy.
type MicroCompactor struct{}

// Compile-time assertion that MicroCompactor satisfies Compactor.
var _ Compactor = (*MicroCompactor)(nil)

// NewMicroCompactor returns a zero-dependency MicroCompactor.
func NewMicroCompactor() *MicroCompactor {
	return &MicroCompactor{}
}

// Compact replaces old tool results with the placeholder until the estimated
// size fits maxTokens. User and assistant messages are never modified.
func (c *MicroCompactor) Compact(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator) ([]TurnItem, error) {
	span, _ := tracing.SpanFromContext(ctx, "compaction.micro", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	result := make([]TurnItem, len(items))
	copy(result, items)

	// Pre-compute the total token count once. The loop below maintains this
	// total incrementally instead of re-estimating the entire list on every
	// iteration, avoiding the O(n²) behaviour of the previous approach.
	tokensBefore := estimateTokens(result, estimator)
	total := tokensBefore

	span.SetAttributes(
		tracing.Attribute{Key: "items_in", Value: len(items)},
		tracing.Attribute{Key: "tokens_before", Value: tokensBefore},
	)

	// Pre-compute the placeholder token count once.
	placeholderTokens := estimateLength(compactedToolResult, estimator)

	// Repeatedly replace the oldest un-compacted tool result until the budget
	// is satisfied or there is nothing left to replace. A cursor avoids
	// re-scanning already-compacted items, and the running total is updated
	// incrementally by subtracting the replaced item's tool-result tokens
	// and adding the placeholder tokens.
	cursor := 0
	for total > maxTokens {
		replaced := false
		for i := cursor; i < len(result); i++ {
			if result[i].ToolResult != "" && result[i].ToolResult != compactedToolResult {
				oldTRTokens := estimateLength(result[i].ToolResult, estimator)
				total -= oldTRTokens - placeholderTokens
				result[i].ToolResult = compactedToolResult
				cursor = i + 1
				replaced = true
				break
			}
		}
		if !replaced {
			// Every tool result is already the placeholder and we are still
			// over budget; a heavier strategy is required.
			break
		}
	}

	tokensAfter := estimateTokens(result, estimator)
	span.SetAttributes(
		tracing.Attribute{Key: "items_out", Value: len(result)},
		tracing.Attribute{Key: "tokens_after", Value: tokensAfter},
	)

	if tokensAfter > maxTokens {
		logger.Warn("compaction.micro.insufficient", "max_tokens", maxTokens, "tokens_after", tokensAfter)
		return result, ErrRequiresTruncating
	}

	logger.Info("compaction.micro.done",
		"items_in", len(items),
		"items_out", len(result),
		"tokens_before", tokensBefore,
		"tokens_after", tokensAfter)
	return result, nil
}
