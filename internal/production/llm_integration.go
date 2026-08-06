// Package production llm_integration.go - wires production resilience and
// observability components (retry, cost tracking, stats) into a single model
// wrapper that can be applied to any llm.BaseChatModel.
package production

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// ProductionModelWrapper wraps a BaseChatModel with production middleware
// (retry, circuit breaker, loop detection, cost tracking). It is constructed
// with functional options and applied via WrapModel.
type ProductionModelWrapper struct {
	retryPolicy   RetryPolicy
	costTracker   *CostTracker
	statsRegistry *StatsRegistry
	sessionID     string
	modelName     string
}

// WrapperOption configures a ProductionModelWrapper at construction time.
type WrapperOption func(*ProductionModelWrapper)

// WithWrapperRetryPolicy sets the retry policy used to wrap the model.
func WithWrapperRetryPolicy(p RetryPolicy) WrapperOption {
	return func(w *ProductionModelWrapper) { w.retryPolicy = p }
}

// WithWrapperCostTracker sets the cost tracker for recording call costs.
func WithWrapperCostTracker(t *CostTracker) WrapperOption {
	return func(w *ProductionModelWrapper) { w.costTracker = t }
}

// WithWrapperStatsRegistry sets the stats registry for recording session
// token usage.
func WithWrapperStatsRegistry(r *StatsRegistry) WrapperOption {
	return func(w *ProductionModelWrapper) { w.statsRegistry = r }
}

// WithWrapperSessionID sets the session ID for stats recording.
func WithWrapperSessionID(id string) WrapperOption {
	return func(w *ProductionModelWrapper) { w.sessionID = id }
}

// WithWrapperModelName sets the model name used for cost tier lookup.
func WithWrapperModelName(name string) WrapperOption {
	return func(w *ProductionModelWrapper) { w.modelName = name }
}

// NewProductionModelWrapper builds a ProductionModelWrapper from functional
// options. A default model name of "gpt-4o-mini" is used when none is set.
func NewProductionModelWrapper(opts ...WrapperOption) *ProductionModelWrapper {
	w := &ProductionModelWrapper{
		modelName: "gpt-4o-mini",
	}
	for _, opt := range opts {
		opt(w)
	}
	slog.Info("production.llm_integration.new",
		"session", w.sessionID,
		"model", w.modelName,
		"has_retry", w.retryPolicy != nil,
		"has_cost_tracker", w.costTracker != nil,
		"has_stats", w.statsRegistry != nil,
	)
	return w
}

// WrapModel takes an llm.BaseChatModel and returns a wrapped version with
// production middleware applied. The wrapping order is:
//  1. Cost tracking + stats recording (innermost, closest to the real model)
//  2. Retry middleware (outermost, retries transient failures)
//
// Retry is applied last so it wraps the cost/stats layer; this means retries
// are counted as separate calls for cost and stats purposes.
func (w *ProductionModelWrapper) WrapModel(model llm.BaseChatModel) llm.BaseChatModel {
	slog.Info("production.llm_integration.wrap",
		"session", w.sessionID,
		"model", w.modelName,
		"has_retry", w.retryPolicy != nil,
		"has_cost_tracker", w.costTracker != nil,
		"has_stats", w.statsRegistry != nil,
	)

	// Inner: wrap with cost tracking + stats recording.
	var wrapped llm.BaseChatModel = &costTrackingModel{
		next:      model,
		tracker:   w.costTracker,
		stats:     w.statsRegistry,
		sessionID: w.sessionID,
		modelName: w.modelName,
	}

	// Outer: wrap with retry middleware.
	if w.retryPolicy != nil {
		retryMW := llm.NewRetryModelMiddleware(llm.WithRetryPolicy(w.retryPolicy))
		wrapped = retryMW.WrapModel(wrapped)
	}

	return wrapped
}

// costTrackingModel wraps a BaseChatModel to record token usage costs and
// session stats after each successful Generate call.
type costTrackingModel struct {
	next      llm.BaseChatModel
	tracker   *CostTracker
	stats     *StatsRegistry
	sessionID string
	modelName string
}

// Compile-time assertion that costTrackingModel satisfies BaseChatModel.
var _ llm.BaseChatModel = (*costTrackingModel)(nil)

// Generate delegates to the wrapped model and records usage on success.
func (m *costTrackingModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	resp, err := m.next.Generate(ctx, msgs, opts...)
	if err == nil && resp != nil {
		m.recordUsage(resp)
	}
	return resp, err
}

// Stream delegates to the wrapped model. Usage tracking for streaming is
// best-effort: since MessageChunk does not carry Usage, tokens are not
// recorded for streamed responses.
func (m *costTrackingModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	return m.next.Stream(ctx, msgs, opts...)
}

// recordUsage records token counts to the cost tracker and stats registry.
func (m *costTrackingModel) recordUsage(resp *llm.Message) {
	if resp.Usage == nil {
		return
	}
	in := resp.Usage.InputTokens
	out := resp.Usage.OutputTokens

	if m.stats != nil {
		m.stats.RecordTokens(m.sessionID, in, out)
	}
	if m.tracker != nil {
		if _, err := m.tracker.Record(m.modelName, in, out); err != nil {
			slog.Debug("production.llm_integration.cost_record_failed",
				"err", err,
				"model", m.modelName,
			)
		}
	}
	slog.Debug("production.llm_integration.usage_recorded",
		"model", m.modelName,
		"input_tokens", in,
		"output_tokens", out,
		"session", m.sessionID,
	)
}

// String returns a debug-friendly description of the wrapper.
func (w *ProductionModelWrapper) String() string {
	return fmt.Sprintf("ProductionModelWrapper(model=%s, session=%s)", w.modelName, w.sessionID)
}
