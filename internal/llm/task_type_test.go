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
