package compaction

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// defaultTriggerReason is applied to the compaction span when no explicit
// trigger reason is configured on the UnifiedCompactor. It is informational and
// only annotates tracing; the router does not branch on it.
const defaultTriggerReason = "unknown"

// ErrDurableSummaryProjectionRequired reports that source-backed history
// exceeded its budget but no typed summary or omission projection was produced.
// The historical name is retained; projection-free results remain unsupported.
var ErrDurableSummaryProjectionRequired = errors.New("compaction: durable history requires a typed context projection")

// UnifiedCompactor is the routing layer of the compaction package. For legacy
// history it tries strategies in order of increasing cost — micro, then summary,
// then truncating — and falls through when a strategy cannot satisfy the
// budget. Source-backed durable history requires a typed summary or explicit
// omission projection; projection-free micro/truncating winners are ineligible.
// Truncating keeps mandatory system instructions; if those alone exceed the
// budget, the router returns an explicit error instead of an over-budget result.
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

// Compact routes compaction across the configured strategies. Source-backed
// history first tries a typed summary, then a typed omission only when no
// summarizer was configured or the built-in summary hit a local output limit.
// Provider failures are not fallback permission. Other history is cheapest first. It emits one `compaction` span and exposes the selected strategy.
func (u *UnifiedCompactor) Compact(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator) (CompactionResult, error) {
	span, sctx := tracing.SpanFromContext(ctx, "compaction", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	originalItems := items
	if err := validateDurableCompactionInput(items); err != nil {
		return CompactionResult{Items: items}, err
	}

	// Validate tool-call closure and normalize legacy derived summaries on a
	// defensive copy before trying any strategy.
	normalized, _, err := groupTurnItems(items)
	if err != nil {
		return CompactionResult{}, err
	}
	if err := checkMandatorySystemPromptBudget(normalized, maxTokens, estimator); err != nil {
		return CompactionResult{Items: items}, err
	}
	items = normalized
	durable := hasDurableSource(items)
	if durable {
		// Durable routing must not trust an old strategy cache after adding the
		// provider-visible fields to the estimate. `normalized` is a defensive
		// copy, so clearing these hints cannot mutate caller-owned history.
		for i := range items {
			items[i].EstimatedTokens = 0
		}
	}

	// Pre-compute token counts per item, caching results in EstimatedTokens so
	// sub-compactors (micro, summary) reuse the cached values instead of
	// re-estimating from scratch.
	current := 0
	for i := range items {
		if items[i].Role == RoleSystem && !items[i].IsCompaction {
			current = saturatingTokenAdd(current, mandatorySystemPromptItemTokens(items[i], estimator))
			continue
		}
		current = saturatingTokenAdd(current, estimateItemTokens(&items[i], estimator))
	}
	span.SetAttributes(
		tracing.Attribute{Key: "trigger_reason", Value: u.triggerReason},
		tracing.Attribute{Key: "current_tokens", Value: current},
		tracing.Attribute{Key: "max_tokens", Value: maxTokens},
		tracing.Attribute{Key: "items_count", Value: len(items)},
	)

	if durable && current > maxTokens {
		builtin, isBuiltin := u.summary.(*SummaryCompactor)
		missing := u.summary == nil
		if isBuiltin {
			_, missing = builtin.summarizer.(noopSummarizer)
		}
		if !missing {
			span.AddEvent("trying_summary")
			result, err := u.summary.Compact(sctx, items, maxTokens, estimator)
			if err == nil {
				if result.NewProjection == nil {
					return CompactionResult{Items: items}, ErrDurableSummaryProjectionRequired
				}
				return u.finish(sctx, logger, span, originalItems, current, maxTokens, estimator, result, StrategySummary), nil
			}
			// Do not traverse wrapped source errors or trust a foreign strategy's
			// self-reported sentinel. Provider/authority/persistence failures keep
			// their original error; only this built-in's local result may fall back.
			if _, local := err.(localSummaryBudgetError); !isBuiltin || !local {
				return CompactionResult{}, err
			}
		}
		if err := ctx.Err(); err != nil {
			return CompactionResult{Items: items}, err
		}
		if u.truncating == nil {
			return CompactionResult{Items: items}, ErrDurableSummaryProjectionRequired
		}
		span.AddEvent("trying_truncating")
		result, err := u.truncating.Compact(sctx, items, maxTokens, estimator)
		if err != nil {
			return CompactionResult{}, err
		}
		if result.Strategy != StrategyTruncating || result.NewProjection == nil || result.NewProjection.Summary != DurableTruncationMarker {
			return CompactionResult{Items: items}, ErrDurableSummaryProjectionRequired
		}
		if err := ValidateRunRequestIntegrity(result.Items); err != nil {
			return CompactionResult{Items: items}, err
		}
		if estimateTokens(result.Items, estimator) > maxTokens {
			return CompactionResult{Items: items}, ErrRequiresTruncating
		}
		return u.finish(sctx, logger, span, originalItems, current, maxTokens, estimator, result, StrategyTruncating), nil
	}

	// Micro: cheapest, zero-LLM. Any failure (including ErrRequiresTruncating)
	// escalates to the next strategy.
	if u.micro != nil {
		span.AddEvent("trying_micro")
		result, err := u.micro.Compact(sctx, items, maxTokens, estimator)
		if err == nil {
			return u.finish(sctx, logger, span, originalItems, current, maxTokens, estimator, result, StrategyMicro), nil
		}
		if errors.Is(err, errMalformedToolGroup) {
			return CompactionResult{}, err
		}
		logger.Debug("compaction.unified.escalate", "from", "micro", "err", err)
	}

	// Summary: LLM-driven, when a summarizer is configured. Failure (including a
	// missing summarizer surfaced via errNoSummarizer) escalates to truncating.
	if u.summary != nil {
		span.AddEvent("trying_summary")
		result, err := u.summary.Compact(sctx, items, maxTokens, estimator)
		if err == nil {
			return u.finish(sctx, logger, span, originalItems, current, maxTokens, estimator, result, StrategySummary), nil
		}
		if errors.Is(err, errMalformedToolGroup) {
			return CompactionResult{}, err
		}
		logger.Debug("compaction.unified.escalate", "from", "summary", "err", err)
	}

	// Truncating: keep mandatory system instructions and the newest context that
	// fits, or return a typed error if the system instructions alone do not fit.
	if u.truncating != nil {
		span.AddEvent("trying_truncating")
		result, err := u.truncating.Compact(sctx, items, maxTokens, estimator)
		if err == nil {
			return u.finish(sctx, logger, span, originalItems, current, maxTokens, estimator, result, StrategyTruncating), nil
		}
		return CompactionResult{}, err
	}

	// No strategy could run; surface a descriptive error rather than returning
	// an unconstrained result.
	return CompactionResult{Items: items}, ErrRequiresTruncating
}

// finish records the winning strategy, emits the result attributes, runs the
// quality evaluator (when configured), and returns the compacted items.
// tokensIn is the pre-computed total for the input items, passed from Compact
// to avoid a redundant re-estimation.
func (u *UnifiedCompactor) finish(ctx context.Context, logger *slog.Logger, span tracing.TraceSpan, items []TurnItem, tokensIn, maxTokens int, estimator TokenEstimator, result CompactionResult, strategy Strategy) CompactionResult {
	u.setLastStrategy(strategy)

	result.Strategy = strategy
	after := estimateTokensCopy(result.Items, estimator)
	span.SetAttributes(
		tracing.Attribute{Key: "strategy_used", Value: strategy.String()},
		tracing.Attribute{Key: "compacted_items", Value: len(result.Items)},
		tracing.Attribute{Key: "compacted_tokens", Value: after},
	)
	logger.Info("compaction.unified.done",
		"strategy", strategy.String(),
		"items_in", len(items),
		"items_out", len(result.Items),
		"tokens_in", tokensIn,
		"tokens_out", after,
		"max_tokens", maxTokens)

	// Quality evaluation is opt-in. The evaluator creates its own
	// "compaction.quality" child span recording coverage/info_loss/
	// compression_ratio, so here we only invoke it and log the result.
	if u.evaluator != nil {
		metrics, err := u.evaluator.Evaluate(
			ctx,
			append([]TurnItem(nil), items...),
			append([]TurnItem(nil), result.Items...),
		)
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
