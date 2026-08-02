package compaction

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// QualityMetrics quantifies how well a compaction preserved the original
// context. Values are derived from item counts and token estimates, so they are
// best-effort but deterministic for a given estimator.
type QualityMetrics struct {
	// Coverage is the share of original items retained after compaction
	// (retained-count / total-count), in [0, 1].
	Coverage float64
	// InfoLoss is the fraction of tokens represented by replaced/truncated
	// content, in [0, 1].
	InfoLoss float64
	// CompressionRatio is tokens_before / tokens_after. When there is nothing
	// to compress the ratio is 1; when everything is removed it is 0.
	CompressionRatio float64
	// Strategy is the strategy that produced the compressed result, or
	// StrategyNone when unknown.
	Strategy Strategy
}

// QualityEvaluator computes quality metrics comparing a conversation before and
// after compaction. Context is threaded through so the evaluation emits a
// trace span, matching the rest of the package.
type QualityEvaluator interface {
	// Evaluate compares the original items with their compressed form and
	// returns quality metrics.
	Evaluate(ctx context.Context, items []TurnItem, compressed []TurnItem) (*QualityMetrics, error)
}

// DefaultQualityEvaluator is the reference QualityEvaluator. It derives all
// metrics purely from item counts and token estimates and never performs LLM
// calls.
type DefaultQualityEvaluator struct {
	estimator TokenEstimator
	strategy  Strategy
}

// Compile-time assertion that DefaultQualityEvaluator satisfies
// QualityEvaluator.
var _ QualityEvaluator = (*DefaultQualityEvaluator)(nil)

// QualityEvaluatorOption configures a DefaultQualityEvaluator.
type QualityEvaluatorOption func(*DefaultQualityEvaluator)

// WithQualityStrategy sets the strategy reported in the returned metrics. When
// unset the metrics report StrategyNone.
func WithQualityStrategy(s Strategy) QualityEvaluatorOption {
	return func(e *DefaultQualityEvaluator) { e.strategy = s }
}

// NewDefaultQualityEvaluator returns a DefaultQualityEvaluator that uses
// estimator to estimate token counts. A nil estimator falls back to the
// heuristic.
func NewDefaultQualityEvaluator(estimator TokenEstimator, opts ...QualityEvaluatorOption) QualityEvaluator {
	e := &DefaultQualityEvaluator{estimator: estimator, strategy: StrategyNone}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Evaluate computes Coverage, InfoLoss and CompressionRatio as described on
// QualityMetrics. Empty input never panics: it yields a zero-coverage metrics
// object with a ratio of 1.
func (e *DefaultQualityEvaluator) Evaluate(ctx context.Context, items []TurnItem, compressed []TurnItem) (*QualityMetrics, error) {
	span, _ := tracing.SpanFromContext(ctx, "compaction.quality", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())

	before := estimateTokens(items, e.estimator)
	after := estimateTokens(compressed, e.estimator)

	total := len(items)
	coverage := 0.0
	if total > 0 {
		coverage = clampedRatio(float64(len(compressed)), float64(total))
	}

	infoLoss := 0.0
	if before > 0 {
		loss := before - after
		if loss < 0 {
			loss = 0
		}
		infoLoss = clampedRatio(float64(loss), float64(before))
	}

	var compression float64
	switch {
	case before == 0:
		// Nothing to compress; treat as identity.
		compression = 1.0
	case after == 0:
		// Everything removed; avoid a divide-by-zero (and +Inf).
		compression = 0.0
	default:
		compression = float64(before) / float64(after)
	}

	metrics := &QualityMetrics{
		Coverage:         coverage,
		InfoLoss:         infoLoss,
		CompressionRatio: compression,
		Strategy:         e.strategy,
	}

	span.SetAttributes(
		tracing.Attribute{Key: "coverage", Value: metrics.Coverage},
		tracing.Attribute{Key: "info_loss", Value: metrics.InfoLoss},
		tracing.Attribute{Key: "compression_ratio", Value: metrics.CompressionRatio},
	)
	logger.Info("compaction.quality.done",
		"coverage", metrics.Coverage,
		"info_loss", metrics.InfoLoss,
		"compression_ratio", metrics.CompressionRatio,
		"items_in", total,
		"items_out", len(compressed))

	return metrics, nil
}

// clampedRatio divides num by den, guarding against a zero denominator and
// clamping the result to [0, 1].
func clampedRatio(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	r := num / den
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}
