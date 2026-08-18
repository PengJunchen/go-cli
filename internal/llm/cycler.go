// Package llm cycler.go — ModelCycler rotates model selection across multiple
// providers using configurable strategies (round-robin, weighted, cost
// priority). It implements ModelMiddleware so it can be registered in a
// ModelMiddlewareChain.
package llm

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
)

// sessionIDKey is the context key used to carry a session identifier for
// ModelCycler session affinity.
type sessionIDKey struct{}

// WithSessionID returns a copy of ctx that carries the given sessionID. The
// sessionID is used by ModelCycler (when SessionAffinity is enabled) to pin a
// conversation to a consistent model.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionIDKey{}, sessionID)
}

// sessionIDFromContext extracts the session ID from ctx, or returns "" when no
// session ID is present.
func sessionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(sessionIDKey{}).(string); ok {
		return v
	}
	return ""
}

// Strategy constants for ModelCyclerConfig.Strategy.
const (
	// StrategyRoundRobin cycles through models in order.
	StrategyRoundRobin = "round_robin"
	// StrategyWeighted selects models proportional to their Weight.
	StrategyWeighted = "weighted"
	// StrategyCostPriority selects the model with the lowest cost (highest
	// weight) first.
	StrategyCostPriority = "cost_priority"
)

// ModelCyclerConfig configures model rotation.
type ModelCyclerConfig struct {
	Models          []ModelEntry
	Strategy        string // round_robin | weighted | cost_priority
	SessionAffinity bool   // same session keeps same model
}

// ModelEntry represents a model in the rotation pool.
type ModelEntry struct {
	Provider string // openai | claude | gemini
	Model    string // model name
	Weight   int    // weight for weighted strategy
	// TaskType optionally tags this model for a specific task type
	// (chat, summary, title, extraction). When non-empty, the cycler
	// prefers this model for calls with a matching task type from context.
	TaskType TaskType
}

// ModelCycler implements model rotation across multiple providers. It is
// safe for concurrent use.
type ModelCycler struct {
	config   ModelCyclerConfig
	registry *ProviderRegistry
	counter  atomic.Int64
	mu       sync.Mutex
	// sessions maps sessionID -> model index when SessionAffinity is enabled.
	sessions map[string]int
	// maxSessions bounds the sessions map to prevent unbounded memory growth.
	// When the limit is reached, the oldest entries are evicted.
	maxSessions int
}

// Compile-time assertion that ModelCycler satisfies ModelMiddleware.
var _ ModelMiddleware = (*ModelCycler)(nil)

// NewModelCycler creates a ModelCycler with the given configuration.
func NewModelCycler(config ModelCyclerConfig) *ModelCycler {
	return &ModelCycler{
		config:      config,
		sessions:    make(map[string]int),
		maxSessions: 1024,
	}
}

// WithRegistry sets the ProviderRegistry used to build selected models on
// demand. Without a registry the cycler falls back to the primary (wrapped)
// model for every call. Returns the cycler for chaining.
func (c *ModelCycler) WithRegistry(r *ProviderRegistry) *ModelCycler {
	c.registry = r
	return c
}

// Name returns "model_cycler".
func (c *ModelCycler) Name() string { return "model_cycler" }

// WrapModel returns a BaseChatModel that routes each call to the model selected
// by the configured strategy. If the registry is unavailable, the selected
// model cannot be built, or the selected model returns an error, the call
// falls back to the wrapped primary model.
func (c *ModelCycler) WrapModel(next BaseChatModel) BaseChatModel {
	return &cycledModel{
		cycler:  c,
		primary: next,
	}
}

// selectModel returns the index of the selected model based on the configured
// strategy. When session affinity is enabled and sessionID is non-empty, the
// same sessionID always maps to the same model index. When taskType is non-empty
// and a model with a matching TaskType tag exists, that model is preferred
// regardless of strategy or session affinity.
func (c *ModelCycler) selectModel(sessionID string, taskType TaskType) int {
	n := len(c.config.Models)
	if n == 0 {
		return 0
	}
	// Prefer a model explicitly tagged for the requested task type.
	if taskType != "" && taskType != TaskTypeChat {
		for i, m := range c.config.Models {
			if m.TaskType == taskType {
				return i
			}
		}
	}
	if c.config.SessionAffinity && sessionID != "" {
		c.mu.Lock()
		defer c.mu.Unlock()
		if idx, ok := c.sessions[sessionID]; ok && idx < n {
			return idx
		}
		idx := c.selectByStrategy()
		// Evict oldest entries when the sessions map exceeds the bound.
		if len(c.sessions) >= c.maxSessions {
			for k := range c.sessions {
				delete(c.sessions, k)
				break
			}
		}
		c.sessions[sessionID] = idx
		return idx
	}
	return c.selectByStrategy()
}

// selectByStrategy dispatches to the strategy-specific selection method.
func (c *ModelCycler) selectByStrategy() int {
	switch c.config.Strategy {
	case StrategyWeighted:
		return c.selectWeighted()
	case StrategyCostPriority:
		return c.selectCostPriority()
	default: // StrategyRoundRobin or empty
		return c.selectRoundRobin()
	}
}

// selectRoundRobin returns the next index in round-robin order.
func (c *ModelCycler) selectRoundRobin() int {
	n := len(c.config.Models)
	if n == 0 {
		return 0
	}
	return int(c.counter.Add(1)-1) % n
}

// selectWeighted returns an index chosen proportionally to each model's
// Weight. If all weights are zero or negative it falls back to round-robin.
func (c *ModelCycler) selectWeighted() int {
	n := len(c.config.Models)
	if n == 0 {
		return 0
	}
	totalWeight := 0
	for _, m := range c.config.Models {
		if m.Weight > 0 {
			totalWeight += m.Weight
		}
	}
	if totalWeight <= 0 {
		return c.selectRoundRobin()
	}
	r := rand.IntN(totalWeight)
	cumWeight := 0
	for i, m := range c.config.Models {
		if m.Weight > 0 {
			cumWeight += m.Weight
		}
		if r < cumWeight {
			return i
		}
	}
	return n - 1
}

// selectCostPriority returns the index of the model with the lowest cost.
// For now, weight is used as the inverse of cost: the model with the highest
// weight is considered the lowest cost and is selected.
func (c *ModelCycler) selectCostPriority() int {
	n := len(c.config.Models)
	if n == 0 {
		return 0
	}
	bestIdx := 0
	bestWeight := c.config.Models[0].Weight
	for i := 1; i < n; i++ {
		if c.config.Models[i].Weight > bestWeight {
			bestWeight = c.config.Models[i].Weight
			bestIdx = i
		}
	}
	return bestIdx
}

// cycledModel is the BaseChatModel produced by ModelCycler.WrapModel. It
// delegates each call to the model selected by the cycler's strategy, falling
// back to the primary model when the selected model is unavailable or errors.
type cycledModel struct {
	cycler  *ModelCycler
	primary BaseChatModel
}

var _ BaseChatModel = (*cycledModel)(nil)

// buildSelectedModel resolves the model at idx via the registry. It returns an
// error when no registry is configured, the index is out of range, or the
// registry fails to build the model.
func (m *cycledModel) buildSelectedModel(ctx context.Context, idx int) (BaseChatModel, func(), error) {
	if m.cycler.registry == nil {
		return nil, nil, errors.New("llm: model cycler has no registry")
	}
	models := m.cycler.config.Models
	if idx < 0 || idx >= len(models) {
		return nil, nil, errors.New("llm: model index out of range")
	}
	entry := models[idx]
	return m.cycler.registry.GetModel(ctx, entry.Provider, ModelConfig{
		Model: entry.Model,
	})
}

// Generate routes the call to the model selected by the cycler's strategy. On
// build failure or generation error it falls back to the primary model.
func (m *cycledModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	sessionID := sessionIDFromContext(ctx)
	taskType := TaskTypeFromContext(ctx)
	idx := m.cycler.selectModel(sessionID, taskType)

	model, cleanup, err := m.buildSelectedModel(ctx, idx)
	if err != nil {
		return m.primary.Generate(ctx, msgs, opts...)
	}
	if cleanup != nil {
		defer cleanup()
	}

	resp, err := model.Generate(ctx, msgs, opts...)
	if err != nil {
		return m.primary.Generate(ctx, msgs, opts...)
	}
	return resp, nil
}

// Stream routes the call to the model selected by the cycler's strategy. On
// build failure or stream initialization error it falls back to the primary
// model. When a cleanup function is returned by buildSelectedModel, the
// returned channel is wrapped in a forwarding goroutine that calls cleanup
// after the inner channel is drained. When cleanup is nil the original
// channel is returned without a wrapper.
func (m *cycledModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	sessionID := sessionIDFromContext(ctx)
	taskType := TaskTypeFromContext(ctx)
	idx := m.cycler.selectModel(sessionID, taskType)

	model, cleanup, err := m.buildSelectedModel(ctx, idx)
	if err != nil {
		return m.primary.Stream(ctx, msgs, opts...)
	}

	ch, err := model.Stream(ctx, msgs, opts...)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return m.primary.Stream(ctx, msgs, opts...)
	}

	// nil cleanup: return original channel without a wrapper goroutine.
	if cleanup == nil {
		return ch, nil
	}

	// Wrap the channel with a forwarding goroutine that calls cleanup when
	// the inner channel is drained (closed).
	out := make(chan MessageChunk, 16)
	go func() {
		defer cleanup()
		defer close(out)
		for chunk := range ch {
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
