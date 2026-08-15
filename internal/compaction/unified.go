package compaction

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// defaultTriggerReason is applied to the compaction span when no explicit
// trigger reason is configured on the UnifiedCompactor. It is informational and
// only annotates tracing; the router does not branch on it.
const defaultTriggerReason = "unknown"

// UnifiedCompactor is the routing layer of the compaction package. It tries the
// strategies in order of increasing cost — micro, then summary, then truncating
// — and falls through to the next strategy whenever the current one cannot
// satisfy the token budget. Because TruncatingCompactor always returns a result
// within budget, the router guarantees a valid outcome unless truncating itself
// is unavailable.
//
// Each strategy is held by the Compactor interface (never a concrete type) so
// the router stays decoupled and injectable, and so callers can substitute
// fakes in tests.
type UnifiedCompactor struct {
	mu sync.Mutex

	micro         Compactor
	summary       Compactor
	truncating    Compactor
	triggerReason string
	evaluator     QualityEvaluator

	lastStrategy Strategy
}

// Compile-time assertion that UnifiedCompactor satisfies Compactor.
var _ Compactor = (*UnifiedCompactor)(nil)

// UnifiedCompactorOption configures a UnifiedCompactor.
type UnifiedCompactorOption func(*UnifiedCompactor)

// WithMicro sets the micro (zero-LLM) strategy. When nil it defaults to a
// freshly constructed MicroCompactor.
func WithMicro(c Compactor) UnifiedCompactorOption {
	return func(u *UnifiedCompactor) { u.micro = c }
}

// WithSummary sets the summary (LLM) strategy. When nil (the default) the
// router skips summary entirely and falls straight through to truncating.
func WithSummary(c Compactor) UnifiedCompactorOption {
	return func(u *UnifiedCompactor) { u.summary = c }
}

// WithTruncating sets the truncating fallback strategy. When nil it defaults to
// a freshly constructed TruncatingCompactor.
func WithTruncating(c Compactor) UnifiedCompactorOption {
	return func(u *UnifiedCompactor) { u.truncating = c }
}

// WithTriggerReason annotates the compaction span with the reason compaction
// was requested (for example threshold|overflow|manual).
func WithTriggerReason(reason string) UnifiedCompactorOption {
	return func(u *UnifiedCompactor) { u.triggerReason = reason }
}

// WithQualityEvaluator wires a QualityEvaluator that runs after a strategy
// succeeds, emitting coverage/info_loss/compression_ratio to a child
// "compaction.quality" span and to slog. When nil (the default) quality
// evaluation is skipped, so the change is safe to roll out gradually.
func WithQualityEvaluator(evaluator QualityEvaluator) UnifiedCompactorOption {
	return func(u *UnifiedCompactor) { u.evaluator = evaluator }
}

// NewUnifiedCompactor returns a UnifiedCompactor with sensible defaults: a real
// MicroCompactor first, no summary (unless provided), and a real
// TruncatingCompactor as the guaranteed fallback.
func NewUnifiedCompactor(opts ...UnifiedCompactorOption) *UnifiedCompactor {
	u := &UnifiedCompactor{
		micro:         NewMicroCompactor(),
		truncating:    NewTruncatingCompactor(),
		triggerReason: defaultTriggerReason,
		lastStrategy:  StrategyNone,
	}
	for _, opt := range opts {
		opt(u)
	}
	return u
}

// LastStrategy reports the strategy selected by the most recent Compact call.
// Callers query it after Compact to learn which strategy produced the result.
// It is safe for concurrent use; when Compact has not run yet it reports
// StrategyNone.
func (u *UnifiedCompactor) LastStrategy() Strategy {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastStrategy
}

func (u *UnifiedCompactor) setLastStrategy(s Strategy) {
	u.mu.Lock()
	u.lastStrategy = s
	u.mu.Unlock()
}

// Compact routes compaction across the configured strategies, cheapest first.
// It emits a single `compaction` span annotating the trigger reason, the token
// counts, and which strategy ultimately succeeded, and it exposes the chosen
// strategy through LastStrategy.
func (u *UnifiedCompactor) Compact(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator) ([]TurnItem, error) {
	span, sctx := tracing.SpanFromContext(ctx, "compaction", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	// Pre-compute token counts per item, caching results in EstimatedTokens so
	// sub-compactors (micro, summary) reuse the cached values instead of
	// re-estimating from scratch.
	current := 0
	for i := range items {
		current += estimateItemTokens(&items[i], estimator)
	}
	span.SetAttributes(
		tracing.Attribute{Key: "trigger_reason", Value: u.triggerReason},
		tracing.Attribute{Key: "current_tokens", Value: current},
		tracing.Attribute{Key: "max_tokens", Value: maxTokens},
		tracing.Attribute{Key: "items_count", Value: len(items)},
	)

	// Micro: cheapest, zero-LLM. Any failure (including ErrRequiresTruncating)
	// escalates to the next strategy.
	if u.micro != nil {
		span.AddEvent("trying_micro")
		result, err := u.micro.Compact(sctx, items, maxTokens, estimator)
		if err == nil {
			return u.finish(sctx, logger, span, items, current, maxTokens, estimator, result, StrategyMicro), nil
		}
		logger.Debug("compaction.unified.escalate", "from", "micro", "err", err)
	}

	// Summary: LLM-driven, when a summarizer is configured. Failure (including a
	// missing summarizer surfaced via errNoSummarizer) escalates to truncating.
	if u.summary != nil {
		span.AddEvent("trying_summary")
		result, err := u.summary.Compact(sctx, items, maxTokens, estimator)
		if err == nil {
			return u.finish(sctx, logger, span, items, current, maxTokens, estimator, result, StrategySummary), nil
		}
		logger.Debug("compaction.unified.escalate", "from", "summary", "err", err)
	}

	// Truncating: guaranteed within budget (or the best achievable subset).
	if u.truncating != nil {
		span.AddEvent("trying_truncating")
		result, err := u.truncating.Compact(sctx, items, maxTokens, estimator)
		if err == nil {
			return u.finish(sctx, logger, span, items, current, maxTokens, estimator, result, StrategyTruncating), nil
		}
		return nil, err
	}

	// No strategy could run; surface a descriptive error rather than returning
	// an unconstrained result.
	return nil, ErrRequiresTruncating
}

// finish records the winning strategy, emits the result attributes, runs the
// quality evaluator (when configured), and returns the compacted items.
// tokensIn is the pre-computed total for the input items, passed from Compact
// to avoid a redundant re-estimation.
func (u *UnifiedCompactor) finish(ctx context.Context, logger *slog.Logger, span tracing.TraceSpan, items []TurnItem, tokensIn, maxTokens int, estimator TokenEstimator, result []TurnItem, strategy Strategy) []TurnItem {
	u.setLastStrategy(strategy)

	after := estimateTokens(result, estimator)
	span.SetAttributes(
		tracing.Attribute{Key: "strategy_used", Value: strategy.String()},
		tracing.Attribute{Key: "compacted_items", Value: len(result)},
		tracing.Attribute{Key: "compacted_tokens", Value: after},
	)
	logger.Info("compaction.unified.done",
		"strategy", strategy.String(),
		"items_in", len(items),
		"items_out", len(result),
		"tokens_in", tokensIn,
		"tokens_out", after,
		"max_tokens", maxTokens)

	// Quality evaluation is opt-in. The evaluator creates its own
	// "compaction.quality" child span recording coverage/info_loss/
	// compression_ratio, so here we only invoke it and log the result.
	if u.evaluator != nil {
		metrics, err := u.evaluator.Evaluate(ctx, items, result)
		if err != nil {
			logger.Warn("compaction.quality.failed", "strategy", strategy.String(), "err", err)
		} else if metrics != nil {
			logger.Info("compaction.quality.metrics",
				"strategy", strategy.String(),
				"coverage", metrics.Coverage,
				"info_loss", metrics.InfoLoss,
				"compression_ratio", metrics.CompressionRatio)
		}
	}
	return result
}
