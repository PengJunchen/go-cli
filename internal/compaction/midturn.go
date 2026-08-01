package compaction

import (
	"context"
	"errors"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// TriggerReason identifies why a compaction was triggered. Values are stable and
// serializable.
type TriggerReason int

// Compaction trigger sources.
const (
	// TriggerNone indicates no trigger occurred.
	TriggerNone TriggerReason = iota
	// TriggerThreshold indicates the context estimate crossed a configured
	// fraction of the token budget before a turn.
	TriggerThreshold
	// TriggerOverflow indicates the model call itself exceeded the window.
	TriggerOverflow
	// TriggerManual indicates an explicit, caller-initiated compaction.
	TriggerManual
)

// String returns a stable, lowercase identifier for the trigger reason.
func (r TriggerReason) String() string {
	switch r {
	case TriggerThreshold:
		return "threshold"
	case TriggerOverflow:
		return "overflow"
	case TriggerManual:
		return "manual"
	default:
		return "none"
	}
}

// ErrCompactorRequired is returned when a compaction is requested but no
// compactor was supplied.
var ErrCompactorRequired = errors.New("compaction: compactor required")

// defaultThresholdRatio is the fraction of maxTokens at which CompactIfNeeded
// triggers a compaction. 0.8 means compaction fires when the current estimate
// exceeds 80% of the budget.
const defaultThresholdRatio = 0.8

// CompactResult reports whether a compaction ran and, when it did, why.
type CompactResult struct {
	// Triggered reports whether compaction actually ran.
	Triggered bool
	// Reason records the trigger source that caused the compaction.
	Reason TriggerReason
}

// MidTurnCompact is the overflow auto-compaction guard. It checks whether the
// current conversation estimate has crossed a threshold fraction of the token
// budget and, when it has, triggers a compaction so the next turn retries with a
// smaller context. It is deliberately trigger-only: the actual rewriting is
// delegated to a caller-supplied Compactor.
type MidTurnCompact struct {
	thresholdRatio float64
}

// MidTurnCompactOption configures a MidTurnCompact.
type MidTurnCompactOption func(*MidTurnCompact)

// WithThresholdRatio sets the fraction of maxTokens at which CompactIfNeeded
// triggers compaction. The value must be in (0, 1]; values outside that range
// are ignored, defaulting to 0.8.
func WithThresholdRatio(r float64) MidTurnCompactOption {
	return func(m *MidTurnCompact) {
		if r > 0 && r <= 1 {
			m.thresholdRatio = r
		}
	}
}

// NewMidTurnCompact returns a MidTurnCompact with a default threshold ratio of
// 0.8.
func NewMidTurnCompact(opts ...MidTurnCompactOption) *MidTurnCompact {
	m := &MidTurnCompact{thresholdRatio: defaultThresholdRatio}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// thresholdTokens returns the token estimate at or above which compaction must
// run, derived from the configured ratio. The result is always clamped to
// maxTokens so a pathological ratio can never trigger past the full budget.
func (m *MidTurnCompact) thresholdTokens(maxTokens int) int {
	limit := int(m.thresholdRatio * float64(maxTokens))
	if limit < 0 || limit >= maxTokens {
		return maxTokens
	}
	return limit
}

// CompactIfNeeded triggers compaction only when the current context estimate
// exceeds the configured threshold fraction of maxTokens. When it does not, the
// items are returned unchanged with Triggered=false. When it does, compaction
// runs and the result is returned together with a Triggered=true result whose
// Reason is TriggerThreshold.
func (m *MidTurnCompact) CompactIfNeeded(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator, compactor Compactor) ([]TurnItem, CompactResult, error) {
	current := estimateTokens(items, estimator)
	if current <= m.thresholdTokens(maxTokens) {
		return items, CompactResult{Triggered: false, Reason: TriggerNone}, nil
	}
	return compactWithReason(ctx, items, maxTokens, estimator, compactor, TriggerThreshold)
}

// CompactTriggered always runs compaction, mirroring the same observable result
// shape as CompactIfNeeded. It is the entry point for explicit compactions
// (manual, or an already-observed overflow) and reports Reason TriggerManual; a
// caller that knows the overflow source may replace the reason as needed.
func (m *MidTurnCompact) CompactTriggered(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator, compactor Compactor) ([]TurnItem, CompactResult, error) {
	return compactWithReason(ctx, items, maxTokens, estimator, compactor, TriggerManual)
}

// compactWithReason emits a `compaction.trigger` span annotating why compaction
// ran, then delegates the rewrite to compactor. The CompactResult always reports
// Triggered=true with the given reason when a compactor is present.
func compactWithReason(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator, compactor Compactor, reason TriggerReason) ([]TurnItem, CompactResult, error) {
	if compactor == nil {
		return items, CompactResult{Triggered: false, Reason: TriggerNone}, ErrCompactorRequired
	}

	span, sctx := tracing.SpanFromContext(ctx, "compaction.trigger", tracing.SpanKindInternal)
	defer span.End()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(tracing.Attribute{Key: "reason", Value: reason.String()})

	out, err := compactor.Compact(sctx, items, maxTokens, estimator)
	if err != nil {
		logger.Warn("compaction.trigger.failed", "reason", reason.String(), "err", err)
		return out, CompactResult{Triggered: true, Reason: reason}, err
	}

	logger.Info("compaction.trigger.done", "reason", reason.String())
	return out, CompactResult{Triggered: true, Reason: reason}, nil
}
