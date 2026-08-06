package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// threeModels is a reusable pool of three ModelEntry values for tests.
var threeModels = []ModelEntry{
	{Provider: "openai", Model: "gpt-4o", Weight: 1},
	{Provider: "claude", Model: "claude-3", Weight: 1},
	{Provider: "gemini", Model: "gemini-pro", Weight: 1},
}

// TestModelCycler_Name verifies the middleware identifier.
func TestModelCycler_Name(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{})
	assert.Equal(t, "model_cycler", c.Name())
}

// TestModelCycler_RoundRobin hits each model once over three calls.
func TestModelCycler_RoundRobin(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{
		Models:   threeModels,
		Strategy: StrategyRoundRobin,
	})
	assert.Equal(t, 0, c.selectModel(""))
	assert.Equal(t, 1, c.selectModel(""))
	assert.Equal(t, 2, c.selectModel(""))
}

// TestModelCycler_RoundRobin_WrapsAround verifies that the counter wraps back
// to index 0 after cycling through all models.
func TestModelCycler_RoundRobin_WrapsAround(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{
		Models:   threeModels,
		Strategy: StrategyRoundRobin,
	})
	// Exhaust one full cycle.
	for i := 0; i < 3; i++ {
		c.selectModel("")
	}
	// The next call should wrap to index 0.
	assert.Equal(t, 0, c.selectModel(""))
	assert.Equal(t, 1, c.selectModel(""))
}

// TestModelCycler_SessionAffinity_SameSession verifies that the same sessionID
// always maps to the same model index.
func TestModelCycler_SessionAffinity_SameSession(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{
		Models:          threeModels,
		Strategy:        StrategyRoundRobin,
		SessionAffinity: true,
	})
	first := c.selectModel("session-A")
	for i := 0; i < 10; i++ {
		assert.Equal(t, first, c.selectModel("session-A"),
			"session-A should always return the same model index")
	}
}

// TestModelCycler_SessionAffinity_DifferentSessions verifies that different
// sessionIDs can be assigned different model indices.
func TestModelCycler_SessionAffinity_DifferentSessions(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{
		Models:          threeModels,
		Strategy:        StrategyRoundRobin,
		SessionAffinity: true,
	})
	idxA := c.selectModel("session-A")
	idxB := c.selectModel("session-B")
	idxC := c.selectModel("session-C")

	// With round-robin and three models, three new sessions should cover
	// all three indices.
	seen := map[int]bool{idxA: true, idxB: true, idxC: true}
	assert.Len(t, seen, 3, "three different sessions should map to three different indices")

	// Repeated calls keep their original assignment.
	assert.Equal(t, idxA, c.selectModel("session-A"))
	assert.Equal(t, idxB, c.selectModel("session-B"))
	assert.Equal(t, idxC, c.selectModel("session-C"))
}

// TestModelCycler_Weighted verifies that higher-weight models are selected
// more often in a statistical sense.
func TestModelCycler_Weighted(t *testing.T) {
	models := []ModelEntry{
		{Provider: "openai", Model: "gpt-4o", Weight: 1},
		{Provider: "claude", Model: "claude-3", Weight: 3},
	}
	c := NewModelCycler(ModelCyclerConfig{
		Models:   models,
		Strategy: StrategyWeighted,
	})

	const iterations = 10000
	counts := make([]int, len(models))
	for i := 0; i < iterations; i++ {
		idx := c.selectModel("")
		counts[idx]++
	}

	// Expected: model 0 ~25%, model 1 ~75%.
	// Use a generous ±5% tolerance to avoid flakiness.
	assert.InDelta(t, 0.25, float64(counts[0])/iterations, 0.05,
		"model 0 (weight 1) should be selected ~25%% of the time")
	assert.InDelta(t, 0.75, float64(counts[1])/iterations, 0.05,
		"model 1 (weight 3) should be selected ~75%% of the time")
}

// TestModelCycler_Weighted_AllZeroWeights verifies that when all weights are
// zero the cycler falls back to round-robin without crashing.
func TestModelCycler_Weighted_AllZeroWeights(t *testing.T) {
	models := []ModelEntry{
		{Provider: "openai", Model: "gpt-4o", Weight: 0},
		{Provider: "claude", Model: "claude-3", Weight: 0},
	}
	c := NewModelCycler(ModelCyclerConfig{
		Models:   models,
		Strategy: StrategyWeighted,
	})
	// Should not crash and should return valid indices.
	idx := c.selectModel("")
	assert.GreaterOrEqual(t, idx, 0)
	assert.Less(t, idx, len(models))
}

// TestModelCycler_EmptyConfig verifies that an empty model pool does not
// crash and always returns 0.
func TestModelCycler_EmptyConfig(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{})
	assert.Equal(t, 0, c.selectModel(""))
	assert.Equal(t, 0, c.selectModel("session-A"))
}

// TestModelCycler_EmptyConfig_WithStrategy verifies that empty config is safe
// across all strategies.
func TestModelCycler_EmptyConfig_WithStrategy(t *testing.T) {
	for _, s := range []string{StrategyRoundRobin, StrategyWeighted, StrategyCostPriority} {
		c := NewModelCycler(ModelCyclerConfig{Strategy: s})
		assert.Equal(t, 0, c.selectModel(""), "strategy %s should return 0 for empty config", s)
	}
}

// TestModelCycler_SingleModel verifies that a single-model pool always
// returns index 0 regardless of strategy.
func TestModelCycler_SingleModel(t *testing.T) {
	models := []ModelEntry{
		{Provider: "openai", Model: "gpt-4o", Weight: 5},
	}
	for _, s := range []string{StrategyRoundRobin, StrategyWeighted, StrategyCostPriority} {
		c := NewModelCycler(ModelCyclerConfig{Models: models, Strategy: s})
		for i := 0; i < 5; i++ {
			assert.Equal(t, 0, c.selectModel(""),
				"strategy %s should always return 0 for a single model", s)
		}
	}
}

// TestModelCycler_CostPriority verifies that the model with the highest weight
// (lowest cost) is always selected.
func TestModelCycler_CostPriority(t *testing.T) {
	models := []ModelEntry{
		{Provider: "openai", Model: "gpt-4o", Weight: 10},
		{Provider: "claude", Model: "claude-3", Weight: 50},
		{Provider: "gemini", Model: "gemini-pro", Weight: 20},
	}
	c := NewModelCycler(ModelCyclerConfig{
		Models:   models,
		Strategy: StrategyCostPriority,
	})
	// Model at index 1 has the highest weight (50), so it should always win.
	for i := 0; i < 10; i++ {
		assert.Equal(t, 1, c.selectModel(""))
	}
}

// TestModelCycler_CostPriority_Ties verifies that ties are resolved to the
// first occurrence.
func TestModelCycler_CostPriority_Ties(t *testing.T) {
	models := []ModelEntry{
		{Provider: "openai", Model: "gpt-4o", Weight: 50},
		{Provider: "claude", Model: "claude-3", Weight: 50},
	}
	c := NewModelCycler(ModelCyclerConfig{
		Models:   models,
		Strategy: StrategyCostPriority,
	})
	assert.Equal(t, 0, c.selectModel(""))
}

// TestModelCycler_SessionAffinity_EmptySessionID verifies that an empty
// sessionID does not trigger session tracking even when affinity is enabled.
func TestModelCycler_SessionAffinity_EmptySessionID(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{
		Models:          threeModels,
		Strategy:        StrategyRoundRobin,
		SessionAffinity: true,
	})
	// With empty sessionID, round-robin should advance normally.
	assert.Equal(t, 0, c.selectModel(""))
	assert.Equal(t, 1, c.selectModel(""))
	assert.Equal(t, 2, c.selectModel(""))
	assert.Empty(t, c.sessions, "no sessions should be tracked for empty sessionIDs")
}

// TestModelCycler_WrapModel verifies that WrapModel returns the input model
// unchanged (placeholder behavior; full routing is task 25-5).
func TestModelCycler_WrapModel(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{Models: threeModels})
	inner := &fbMockModel{genContent: "test"}
	wrapped := c.WrapModel(inner)
	assert.Same(t, inner, wrapped, "WrapModel should return the input model unchanged")
}

// TestModelCycler_ImplementsModelMiddleware verifies the compile-time
// assertion at runtime.
func TestModelCycler_ImplementsModelMiddleware(t *testing.T) {
	var _ ModelMiddleware = NewModelCycler(ModelCyclerConfig{})
}
