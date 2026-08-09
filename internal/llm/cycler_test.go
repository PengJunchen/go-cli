package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	assert.Equal(t, 0, c.selectModel("", TaskTypeChat))
	assert.Equal(t, 1, c.selectModel("", TaskTypeChat))
	assert.Equal(t, 2, c.selectModel("", TaskTypeChat))
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
		c.selectModel("", TaskTypeChat)
	}
	// The next call should wrap to index 0.
	assert.Equal(t, 0, c.selectModel("", TaskTypeChat))
	assert.Equal(t, 1, c.selectModel("", TaskTypeChat))
}

// TestModelCycler_SessionAffinity_SameSession verifies that the same sessionID
// always maps to the same model index.
func TestModelCycler_SessionAffinity_SameSession(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{
		Models:          threeModels,
		Strategy:        StrategyRoundRobin,
		SessionAffinity: true,
	})
	first := c.selectModel("session-A", TaskTypeChat)
	for i := 0; i < 10; i++ {
		assert.Equal(t, first, c.selectModel("session-A", TaskTypeChat),
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
	idxA := c.selectModel("session-A", TaskTypeChat)
	idxB := c.selectModel("session-B", TaskTypeChat)
	idxC := c.selectModel("session-C", TaskTypeChat)

	// With round-robin and three models, three new sessions should cover
	// all three indices.
	seen := map[int]bool{idxA: true, idxB: true, idxC: true}
	assert.Len(t, seen, 3, "three different sessions should map to three different indices")

	// Repeated calls keep their original assignment.
	assert.Equal(t, idxA, c.selectModel("session-A", TaskTypeChat))
	assert.Equal(t, idxB, c.selectModel("session-B", TaskTypeChat))
	assert.Equal(t, idxC, c.selectModel("session-C", TaskTypeChat))
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
		idx := c.selectModel("", TaskTypeChat)
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
	idx := c.selectModel("", TaskTypeChat)
	assert.GreaterOrEqual(t, idx, 0)
	assert.Less(t, idx, len(models))
}

// TestModelCycler_EmptyConfig verifies that an empty model pool does not
// crash and always returns 0.
func TestModelCycler_EmptyConfig(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{})
	assert.Equal(t, 0, c.selectModel("", TaskTypeChat))
	assert.Equal(t, 0, c.selectModel("session-A", TaskTypeChat))
}

// TestModelCycler_EmptyConfig_WithStrategy verifies that empty config is safe
// across all strategies.
func TestModelCycler_EmptyConfig_WithStrategy(t *testing.T) {
	for _, s := range []string{StrategyRoundRobin, StrategyWeighted, StrategyCostPriority} {
		c := NewModelCycler(ModelCyclerConfig{Strategy: s})
		assert.Equal(t, 0, c.selectModel("", TaskTypeChat), "strategy %s should return 0 for empty config", s)
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
			assert.Equal(t, 0, c.selectModel("", TaskTypeChat),
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
		assert.Equal(t, 1, c.selectModel("", TaskTypeChat))
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
	assert.Equal(t, 0, c.selectModel("", TaskTypeChat))
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
	assert.Equal(t, 0, c.selectModel("", TaskTypeChat))
	assert.Equal(t, 1, c.selectModel("", TaskTypeChat))
	assert.Equal(t, 2, c.selectModel("", TaskTypeChat))
	assert.Empty(t, c.sessions, "no sessions should be tracked for empty sessionIDs")
}

// TestModelCycler_WrapModel verifies that WrapModel returns a wrapped model
// (not the input unchanged) that falls back to the primary when no registry is
// configured.
func TestModelCycler_WrapModel(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{Models: threeModels})
	primary := &fbMockModel{genContent: "primary-ok"}
	wrapped := c.WrapModel(primary)
	assert.NotSame(t, primary, wrapped, "WrapModel should return a wrapped model, not the input")

	// Without a registry, the wrapped model falls back to primary.
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "primary-ok", resp.Content)
	assert.Equal(t, 1, primary.genCalls, "primary should be called when no registry is set")
}

// TestModelCycler_ImplementsModelMiddleware verifies the compile-time
// assertion at runtime.
func TestModelCycler_ImplementsModelMiddleware(t *testing.T) {
	var _ ModelMiddleware = NewModelCycler(ModelCyclerConfig{})
}

// cyclerTestProvider is a test ModelProvider whose Build returns a mock model
// whose Generate response content and Stream text are the model name from the
// config. This lets tests verify which model entry was selected by checking the
// response content. When genErr is non-nil, all built models fail Generate and
// Stream.
type cyclerTestProvider struct {
	name   string
	genErr error
}

func (p *cyclerTestProvider) Name() string        { return p.name }
func (p *cyclerTestProvider) Models() []ModelInfo { return nil }
func (p *cyclerTestProvider) Build(_ context.Context, cfg ModelConfig) (BaseChatModel, func(), error) {
	return &fbMockModel{
		genContent: cfg.Model,
		genErr:     p.genErr,
		streamText: cfg.Model,
		streamErr:  p.genErr,
	}, func() {}, nil
}

// newCyclerTestRegistry creates a ProviderRegistry with cyclerTestProvider
// instances registered under the names used by threeModels. When genErr is
// non-nil, every built model will fail.
func newCyclerTestRegistry(t *testing.T, genErr error) *ProviderRegistry {
	t.Helper()
	reg := NewProviderRegistry()
	for _, name := range []string{"openai", "claude", "gemini"} {
		require.NoError(t, reg.Register(&cyclerTestProvider{name: name, genErr: genErr}))
	}
	return reg
}

// TestModelCycler_WrapModel_DelegatesToSelected verifies that Generate
// delegates to the model selected by the cycler instead of the primary.
func TestModelCycler_WrapModel_DelegatesToSelected(t *testing.T) {
	reg := newCyclerTestRegistry(t, nil)
	c := NewModelCycler(ModelCyclerConfig{
		Models:   threeModels,
		Strategy: StrategyRoundRobin,
	}).WithRegistry(reg)

	primary := &fbMockModel{genContent: "primary-ok"}
	wrapped := c.WrapModel(primary)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Contains(t, []string{"gpt-4o", "claude-3", "gemini-pro"}, resp.Content,
		"response should come from a selected model, not the primary")
	assert.Equal(t, 0, primary.genCalls, "primary should not be called when selected model succeeds")
}

// TestModelCycler_WrapModel_FallbackOnError verifies that when the selected
// model returns an error, Generate falls back to the primary model.
func TestModelCycler_WrapModel_FallbackOnError(t *testing.T) {
	reg := newCyclerTestRegistry(t, errors.New("selected model down"))
	c := NewModelCycler(ModelCyclerConfig{
		Models:   threeModels,
		Strategy: StrategyRoundRobin,
	}).WithRegistry(reg)

	primary := &fbMockModel{genContent: "primary-ok"}
	wrapped := c.WrapModel(primary)

	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "primary-ok", resp.Content, "should fall back to primary on error")
	assert.Equal(t, 1, primary.genCalls, "primary should be called exactly once as fallback")
}

// TestModelCycler_WrapModel_NoRegistry verifies that without a registry, every
// call falls back to the primary model.
func TestModelCycler_WrapModel_NoRegistry(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{
		Models:   threeModels,
		Strategy: StrategyRoundRobin,
	})

	primary := &fbMockModel{genContent: "primary-ok"}
	wrapped := c.WrapModel(primary)

	for i := 0; i < 3; i++ {
		resp, err := wrapped.Generate(context.Background(), nil)
		require.NoError(t, err)
		assert.Equal(t, "primary-ok", resp.Content, "call %d should fall back to primary", i)
	}
	assert.Equal(t, 3, primary.genCalls, "primary should handle all calls without a registry")
}

// TestModelCycler_WrapModel_RoundRobin verifies that consecutive Generate calls
// cycle through all models in round-robin order.
func TestModelCycler_WrapModel_RoundRobin(t *testing.T) {
	reg := newCyclerTestRegistry(t, nil)
	c := NewModelCycler(ModelCyclerConfig{
		Models:   threeModels,
		Strategy: StrategyRoundRobin,
	}).WithRegistry(reg)

	primary := &fbMockModel{genContent: "primary-ok"}
	wrapped := c.WrapModel(primary)

	expected := []string{"gpt-4o", "claude-3", "gemini-pro"}
	for i, want := range expected {
		resp, err := wrapped.Generate(context.Background(), nil)
		require.NoError(t, err)
		assert.Equal(t, want, resp.Content, "call %d should select model %q", i, want)
	}
	assert.Equal(t, 0, primary.genCalls, "primary should never be called when all selected models succeed")
}

// TestModelCycler_WrapModel_SessionAffinity verifies that the same session ID
// always routes to the same model across multiple calls.
func TestModelCycler_WrapModel_SessionAffinity(t *testing.T) {
	reg := newCyclerTestRegistry(t, nil)
	c := NewModelCycler(ModelCyclerConfig{
		Models:          threeModels,
		Strategy:        StrategyRoundRobin,
		SessionAffinity: true,
	}).WithRegistry(reg)

	primary := &fbMockModel{genContent: "primary-ok"}
	wrapped := c.WrapModel(primary)

	ctx := WithSessionID(context.Background(), "session-A")

	var firstContent string
	for i := 0; i < 5; i++ {
		resp, err := wrapped.Generate(ctx, nil)
		require.NoError(t, err)
		if i == 0 {
			firstContent = resp.Content
			assert.Contains(t, []string{"gpt-4o", "claude-3", "gemini-pro"}, firstContent,
				"first call should select a valid model")
		} else {
			assert.Equal(t, firstContent, resp.Content,
				"session-A should always route to the same model (call %d)", i)
		}
	}
	assert.Equal(t, 0, primary.genCalls, "primary should never be called when selected models succeed")
}

// TestModelCycler_WrapModel_SessionAffinity_DifferentSessions verifies that
// different session IDs can be routed to different models.
func TestModelCycler_WrapModel_SessionAffinity_DifferentSessions(t *testing.T) {
	reg := newCyclerTestRegistry(t, nil)
	c := NewModelCycler(ModelCyclerConfig{
		Models:          threeModels,
		Strategy:        StrategyRoundRobin,
		SessionAffinity: true,
	}).WithRegistry(reg)

	primary := &fbMockModel{genContent: "primary-ok"}
	wrapped := c.WrapModel(primary)

	contents := make(map[string]bool)
	for _, sid := range []string{"s1", "s2", "s3"} {
		ctx := WithSessionID(context.Background(), sid)
		resp, err := wrapped.Generate(ctx, nil)
		require.NoError(t, err)
		contents[resp.Content] = true
	}
	// With round-robin and three models, three new sessions should hit three
	// different models.
	assert.Len(t, contents, 3, "three different sessions should route to three different models")
}

// TestModelCycler_WrapModel_Stream_Delegates verifies that Stream delegates to
// the selected model.
func TestModelCycler_WrapModel_Stream_Delegates(t *testing.T) {
	reg := newCyclerTestRegistry(t, nil)
	c := NewModelCycler(ModelCyclerConfig{
		Models:   threeModels,
		Strategy: StrategyRoundRobin,
	}).WithRegistry(reg)

	primary := &fbMockModel{streamText: "primary-stream"}
	wrapped := c.WrapModel(primary)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var got string
	for chunk := range ch {
		got += chunk.Content
	}
	assert.Contains(t, []string{"gpt-4o", "claude-3", "gemini-pro"}, got,
		"stream should come from a selected model, not the primary")
	assert.Equal(t, 0, primary.streamCalls, "primary should not be called when selected model succeeds")
}

// TestModelCycler_WrapModel_Stream_FallbackOnError verifies that when the
// selected model's Stream returns an error, the call falls back to the primary.
func TestModelCycler_WrapModel_Stream_FallbackOnError(t *testing.T) {
	reg := newCyclerTestRegistry(t, errors.New("stream down"))
	c := NewModelCycler(ModelCyclerConfig{
		Models:   threeModels,
		Strategy: StrategyRoundRobin,
	}).WithRegistry(reg)

	primary := &fbMockModel{streamText: "primary-stream"}
	wrapped := c.WrapModel(primary)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var got string
	for chunk := range ch {
		got += chunk.Content
	}
	assert.Equal(t, "primary-stream", got, "should fall back to primary stream on error")
	assert.Equal(t, 1, primary.streamCalls, "primary should be called exactly once as fallback")
}

// TestModelCycler_WrapModel_Stream_NoRegistry verifies that without a registry,
// Stream falls back to the primary.
func TestModelCycler_WrapModel_Stream_NoRegistry(t *testing.T) {
	c := NewModelCycler(ModelCyclerConfig{
		Models:   threeModels,
		Strategy: StrategyRoundRobin,
	})

	primary := &fbMockModel{streamText: "primary-stream"}
	wrapped := c.WrapModel(primary)

	ch, err := wrapped.Stream(context.Background(), nil)
	require.NoError(t, err)

	var got string
	for chunk := range ch {
		got += chunk.Content
	}
	assert.Equal(t, "primary-stream", got, "should fall back to primary without a registry")
}
