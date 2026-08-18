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
//
// To avoid the O(n²) full-scan that re-estimating every item on every loop
// iteration would cause, MidTurnCompact tracks how many items have already
// been estimated (lastIdx) and the running total (runningTotal). Each
// CompactIfNeeded call only estimates items that are new since the last call,
// yielding O(1) work per iteration instead of O(n).
type MidTurnCompact struct {
	thresholdRatio float64
	// lastIdx is the number of items already estimated in previous
	// CompactIfNeeded calls within the same turn. Items[0:lastIdx] are
	// assumed unchanged; only items[lastIdx:] are estimated.
	lastIdx int
	// runningTotal is the accumulated token estimate for items[0:lastIdx].
	runningTotal int
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

// Reset clears the incremental estimation state. Call this when the
// conversation items are replaced (e.g. at the start of a new turn or after
// compaction) so the next CompactIfNeeded re-estimates from scratch.
func (m *MidTurnCompact) Reset() {
	m.lastIdx = 0
	m.runningTotal = 0
}

// estimateIncremental returns the total token estimate for items without
// re-scanning items that were already estimated in a previous call. It appends
// the estimate of each new item (items[lastIdx:]) to runningTotal, caching the
// per-item result in TurnItem.EstimatedTokens. When the item count shrinks
// (e.g. a new turn starts with fewer items) the state is reset and all items
// are estimated from scratch. This satisfies AC-2: the midturn guard does not
// do a full scan every iteration.
func (m *MidTurnCompact) estimateIncremental(items []TurnItem, estimator TokenEstimator) int {
	if len(items) < m.lastIdx {
		// Items shrank: new turn or external replacement. Reset and
		// re-estimate everything.
		m.lastIdx = 0
		m.runningTotal = 0
	}
	for i := m.lastIdx; i < len(items); i++ {
		m.runningTotal += estimateItemTokens(&items[i], estimator)
	}
	m.lastIdx = len(items)
	return m.runningTotal
}

// CompactIfNeeded triggers compaction only when the current context estimate
// exceeds the configured threshold fraction of maxTokens. When it does not, the
// items are returned unchanged with Triggered=false. When it does, compaction
// runs and the result is returned together with a Triggered=true result whose
// Reason is TriggerThreshold.
//
// Estimation is incremental: only items new since the last call are estimated,
// avoiding the O(n²) full-scan that would otherwise occur when the guard is
// called on every loop iteration with a growing conversation.
func (m *MidTurnCompact) CompactIfNeeded(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator, compactor Compactor) ([]TurnItem, CompactResult, error) {
	current := m.estimateIncremental(items, estimator)
	if current <= m.thresholdTokens(maxTokens) {
		return items, CompactResult{Triggered: false, Reason: TriggerNone}, nil
	}
	// Compaction will replace items; reset incremental state so the next
	// call re-estimates from scratch.
	m.Reset()
	return compactWithReason(ctx, items, maxTokens, estimator, compactor, TriggerThreshold)
}

// CompactTriggered always runs compaction, mirroring the same observable result
// shape as CompactIfNeeded. It is the entry point for explicit compactions
// (manual, or an already-observed overflow) and reports Reason TriggerManual; a
// caller that knows the overflow source may replace the reason as needed.
func (m *MidTurnCompact) CompactTriggered(ctx context.Context, items []TurnItem, maxTokens int, estimator TokenEstimator, compactor Compactor) ([]TurnItem, CompactResult, error) {
	// Compaction will replace items; reset incremental state.
	m.Reset()
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
