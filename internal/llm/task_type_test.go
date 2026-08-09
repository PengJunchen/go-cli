package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- TaskType context tests ---

func TestWithTaskType_AndFromContext(t *testing.T) {
	ctx := WithTaskType(context.Background(), TaskTypeSummary)
	tt := TaskTypeFromContext(ctx)
	assert.Equal(t, TaskTypeSummary, tt)
}

func TestTaskTypeFromContext_Default(t *testing.T) {
	tt := TaskTypeFromContext(context.Background())
	assert.Equal(t, TaskTypeChat, tt)
}

func TestTaskTypeFromContext_EmptyString(t *testing.T) {
	ctx := context.WithValue(context.Background(), taskTypeKey{}, TaskType(""))
	tt := TaskTypeFromContext(ctx)
	assert.Equal(t, TaskTypeChat, tt)
}

// --- ModelSelector tests ---

func TestDefaultModelSelector_ChatUsesPrimary(t *testing.T) {
	primary := &fbMockModel{genContent: "primary"}
	small := &fbMockModel{genContent: "small"}
	sel := NewDefaultModelSelector(primary, small)
	assert.Same(t, primary, sel.SelectModel(TaskTypeChat))
}

func TestDefaultModelSelector_SummaryUsesSmall(t *testing.T) {
	primary := &fbMockModel{genContent: "primary"}
	small := &fbMockModel{genContent: "small"}
	sel := NewDefaultModelSelector(primary, small)
	assert.Same(t, small, sel.SelectModel(TaskTypeSummary))
}

func TestDefaultModelSelector_TitleUsesSmall(t *testing.T) {
	primary := &fbMockModel{genContent: "primary"}
	small := &fbMockModel{genContent: "small"}
	sel := NewDefaultModelSelector(primary, small)
	assert.Same(t, small, sel.SelectModel(TaskTypeTitle))
}

func TestDefaultModelSelector_ExtractionUsesSmall(t *testing.T) {
	primary := &fbMockModel{genContent: "primary"}
	small := &fbMockModel{genContent: "small"}
	sel := NewDefaultModelSelector(primary, small)
	assert.Same(t, small, sel.SelectModel(TaskTypeExtraction))
}

func TestDefaultModelSelector_NoSmallModel_FallsBackToPrimary(t *testing.T) {
	primary := &fbMockModel{genContent: "primary"}
	sel := NewDefaultModelSelector(primary, nil)
	assert.Same(t, primary, sel.SelectModel(TaskTypeSummary))
	assert.Same(t, primary, sel.SelectModel(TaskTypeChat))
}

func TestDefaultModelSelector_HasSmallModel(t *testing.T) {
	primary := &fbMockModel{genContent: "primary"}
	small := &fbMockModel{genContent: "small"}

	sel := NewDefaultModelSelector(primary, small)
	assert.True(t, sel.HasSmallModel())

	sel2 := NewDefaultModelSelector(primary, nil)
	assert.False(t, sel2.HasSmallModel())
}

// --- Cycler taskType tests ---

func TestModelCycler_SelectModel_TaskTypePriority(t *testing.T) {
	cycler := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{
			{Provider: "p1", Model: "big"},
			{Provider: "p2", Model: "small", TaskType: TaskTypeSummary},
		},
		Strategy: StrategyRoundRobin,
	})
	// TaskTypeSummary should prefer index 1 (the small model).
	idx := cycler.selectModel("", TaskTypeSummary)
	assert.Equal(t, 1, idx)

	// TaskTypeChat should use the strategy (round-robin), not the tagged model.
	idx = cycler.selectModel("", TaskTypeChat)
	assert.Equal(t, 0, idx) // first call, counter starts at 0
}

func TestModelCycler_SelectModel_NoTaskTypeMatch(t *testing.T) {
	cycler := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{
			{Provider: "p1", Model: "big"},
			{Provider: "p2", Model: "small", TaskType: TaskTypeSummary},
		},
		Strategy: StrategyRoundRobin,
	})
	// TaskTypeExtraction has no matching tagged model — should fall through to strategy.
	idx := cycler.selectModel("", TaskTypeExtraction)
	assert.Equal(t, 0, idx)
}

func TestModelCycler_SelectModel_EmptyModels(t *testing.T) {
	cycler := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{},
	})
	idx := cycler.selectModel("", TaskTypeSummary)
	assert.Equal(t, 0, idx)
}

func TestCycledModel_Generate_TaskTypeContextPropagation(t *testing.T) {
	// Verify that Generate extracts TaskType from context and passes it to
	// selectModel without panicking. Without a registry the cycler falls back
	// to the primary model — the routing logic itself (tagged model selection)
	// is covered by TestModelCycler_SelectModel_TaskTypePriority.
	primary := &fbMockModel{genContent: "primary-ok"}
	cycler := NewModelCycler(ModelCyclerConfig{
		Models: []ModelEntry{
			{Provider: "p1", Model: "big"},
		},
		Strategy: StrategyRoundRobin,
	})
	wrapped := cycler.WrapModel(primary)
	ctx := WithTaskType(context.Background(), TaskTypeSummary)
	resp, err := wrapped.Generate(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, "primary-ok", resp.Content) // fallback to primary since no registry
}

// --- SelectModelWithTokens tests ---

// mockRegistry is a configurable ModelRegistry for token-aware selector tests.
// It returns the configured ModelInfo for a single (provider, model) pair and
// false for all other lookups.
type mockRegistry struct {
	provider    string
	model       string
	info        ModelInfo
	lookupCount int
}

func (m *mockRegistry) Lookup(_ context.Context, provider, model string) (ModelInfo, bool) {
	m.lookupCount++
	if provider == m.provider && model == m.model {
		return m.info, true
	}
	return ModelInfo{}, false
}

func (m *mockRegistry) Providers() []ProviderMetadata                  { return nil }
func (m *mockRegistry) ModelsForProvider(_ string) []ModelInfo          { return nil }
func (m *mockRegistry) Refresh(_ context.Context) error                 { return nil }

var _ ModelRegistry = (*mockRegistry)(nil)

// TestSelectorWithRegistry verifies SelectModelWithTokens token-aware routing
// against a mock ModelRegistry, covering: nil registry fallback, zero-token
// fallback, within-limit passthrough, overflow switch to small, overflow with
// no small model, small-task bypass, ContextWindow fallback, and unknown-model
// fallback.
func TestSelectorWithRegistry(t *testing.T) {
	primary := &fbMockModel{genContent: "primary"}
	small := &fbMockModel{genContent: "small"}

	t.Run("nil_registry_falls_back_to_static_routing", func(t *testing.T) {
		sel := NewDefaultModelSelector(primary, small)
		// No registry set → SelectModelWithTokens should behave like SelectModel.
		got := sel.SelectModelWithTokens(context.Background(), TaskTypeChat, 999999)
		assert.Same(t, primary, got, "nil registry should return static routing result")
		assert.Nil(t, sel.Registry(), "Registry() should be nil when not configured")
	})

	t.Run("zero_or_negative_tokens_falls_back", func(t *testing.T) {
		reg := &mockRegistry{provider: "openai", model: "gpt-4o", info: ModelInfo{InputTokenLimit: 1000}}
		sel := NewDefaultModelSelector(primary, small).
			WithModelRegistry(reg).
			WithModelNames("openai", "gpt-4o", "openai", "gpt-4o-mini")

		// estimatedTokens=0 → no limit check, returns static result (primary for chat).
		got := sel.SelectModelWithTokens(context.Background(), TaskTypeChat, 0)
		assert.Same(t, primary, got)
		// negative tokens → same fallback.
		got = sel.SelectModelWithTokens(context.Background(), TaskTypeChat, -1)
		assert.Same(t, primary, got)
		// registry should not have been queried for tokens.
		assert.Equal(t, 0, reg.lookupCount, "zero/negative tokens should not query registry")
	})

	t.Run("within_limit_returns_primary", func(t *testing.T) {
		reg := &mockRegistry{provider: "openai", model: "gpt-4o", info: ModelInfo{InputTokenLimit: 128000}}
		sel := NewDefaultModelSelector(primary, small).
			WithModelRegistry(reg).
			WithModelNames("openai", "gpt-4o", "openai", "gpt-4o-mini")

		got := sel.SelectModelWithTokens(context.Background(), TaskTypeChat, 50000)
		assert.Same(t, primary, got, "tokens within limit should return primary")
	})

	t.Run("overflow_switches_to_small", func(t *testing.T) {
		reg := &mockRegistry{provider: "openai", model: "gpt-4o", info: ModelInfo{InputTokenLimit: 128000}}
		sel := NewDefaultModelSelector(primary, small).
			WithModelRegistry(reg).
			WithModelNames("openai", "gpt-4o", "openai", "gpt-4o-mini")

		got := sel.SelectModelWithTokens(context.Background(), TaskTypeChat, 200000)
		assert.Same(t, small, got, "overflow should switch to small model")
	})

	t.Run("overflow_no_small_returns_primary", func(t *testing.T) {
		reg := &mockRegistry{provider: "openai", model: "gpt-4o", info: ModelInfo{InputTokenLimit: 128000}}
		sel := NewDefaultModelSelector(primary, nil). // no small model
								WithModelRegistry(reg).
								WithModelNames("openai", "gpt-4o", "", "")

		got := sel.SelectModelWithTokens(context.Background(), TaskTypeChat, 200000)
		assert.Same(t, primary, got, "overflow with no small should still return primary")
	})

	t.Run("small_task_bypasses_limit_check", func(t *testing.T) {
		reg := &mockRegistry{provider: "openai", model: "gpt-4o", info: ModelInfo{InputTokenLimit: 1000}}
		sel := NewDefaultModelSelector(primary, small).
			WithModelRegistry(reg).
			WithModelNames("openai", "gpt-4o", "openai", "gpt-4o-mini")

		// TaskTypeSummary → SelectModel returns small (not primary).
		// SelectModelWithTokens should return small without checking primary limit.
		got := sel.SelectModelWithTokens(context.Background(), TaskTypeSummary, 999999)
		assert.Same(t, small, got, "small task should bypass primary limit check")
	})

	t.Run("context_window_fallback_when_input_limit_zero", func(t *testing.T) {
		// InputTokenLimit=0 but ContextWindow=100000 → should use ContextWindow.
		reg := &mockRegistry{provider: "openai", model: "gpt-4o", info: ModelInfo{InputTokenLimit: 0, ContextWindow: 100000}}
		sel := NewDefaultModelSelector(primary, small).
			WithModelRegistry(reg).
			WithModelNames("openai", "gpt-4o", "openai", "gpt-4o-mini")

		// 50000 < 100000 (ContextWindow) → primary.
		got := sel.SelectModelWithTokens(context.Background(), TaskTypeChat, 50000)
		assert.Same(t, primary, got, "should use ContextWindow as fallback limit")

		// 150000 > 100000 → switch to small.
		got = sel.SelectModelWithTokens(context.Background(), TaskTypeChat, 150000)
		assert.Same(t, small, got, "overflow against ContextWindow should switch to small")
	})

	t.Run("unknown_model_returns_primary", func(t *testing.T) {
		// Registry does not know the configured primary model → Lookup returns
		// false → limit=0 → no overflow check → returns primary.
		reg := &mockRegistry{provider: "openai", model: "gpt-4o", info: ModelInfo{InputTokenLimit: 1000}}
		sel := NewDefaultModelSelector(primary, small).
			WithModelRegistry(reg).
			WithModelNames("unknown-provider", "unknown-model", "openai", "gpt-4o-mini")

		got := sel.SelectModelWithTokens(context.Background(), TaskTypeChat, 999999)
		assert.Same(t, primary, got, "unknown model should return primary (limit=0, no overflow)")
	})
}
