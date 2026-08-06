// Package llm cycler.go — ModelCycler rotates model selection across multiple
// providers using configurable strategies (round-robin, weighted, cost
// priority). It implements ModelMiddleware so it can be registered in a
// ModelMiddlewareChain. The full WrapModel routing is task 25-5; this file
// defines the types and selection logic.
package llm

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"
)

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
}

// ModelCycler implements model rotation across multiple providers. It is
// safe for concurrent use.
type ModelCycler struct {
	config  ModelCyclerConfig
	counter atomic.Int64
	mu      sync.Mutex
	// sessions maps sessionID -> model index when SessionAffinity is enabled.
	sessions map[string]int
}

// Compile-time assertion that ModelCycler satisfies ModelMiddleware.
var _ ModelMiddleware = (*ModelCycler)(nil)

// NewModelCycler creates a ModelCycler with the given configuration.
func NewModelCycler(config ModelCyclerConfig) *ModelCycler {
	return &ModelCycler{
		config:   config,
		sessions: make(map[string]int),
	}
}

// Name returns "model_cycler".
func (c *ModelCycler) Name() string { return "model_cycler" }

// WrapModel is a placeholder that returns the wrapped model unchanged. The
// full implementation that routes to the selected provider's model is task
// 25-5.
func (c *ModelCycler) WrapModel(next BaseChatModel) BaseChatModel {
	return next
}

// selectModel returns the index of the selected model based on the configured
// strategy. When session affinity is enabled and sessionID is non-empty, the
// same sessionID always maps to the same model index.
func (c *ModelCycler) selectModel(sessionID string) int {
	n := len(c.config.Models)
	if n == 0 {
		return 0
	}
	if c.config.SessionAffinity && sessionID != "" {
		c.mu.Lock()
		defer c.mu.Unlock()
		if idx, ok := c.sessions[sessionID]; ok && idx < n {
			return idx
		}
		idx := c.selectByStrategy()
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
