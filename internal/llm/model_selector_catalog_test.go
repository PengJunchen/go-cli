package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultModelSelectorUsesProviderScopedCatalogForBareAndQualifiedSwitches(t *testing.T) {
	var built []ModelSpec
	failModel := false
	selector := NewDefaultModelSelector(&modelCatalogSelectorStub{}, nil).
		WithModelNames("openai", "shared-model", "", "").
		WithModelSpecLister(func() []ModelSpec {
			return []ModelSpec{
				{ProviderID: "custom", ModelID: "shared-model", Info: ModelInfo{Name: "shared-model"}, Known: true},
				{ProviderID: "openai", ModelID: "shared-model", Info: ModelInfo{Name: "shared-model"}, Known: true},
			}
		}).
		WithModelSpecBuilder(func(_ context.Context, candidate ModelSpec) (BaseChatModel, func(), error) {
			built = append(built, candidate)
			if failModel {
				return nil, nil, errors.New("fixture build failed")
			}
			return &modelCatalogSelectorStub{}, nil, nil
		})

	available := selector.AvailableModelSpecs()
	require.Len(t, available, 2)
	assert.Equal(t, "custom", available[0].ProviderID)
	assert.Equal(t, "openai", available[1].ProviderID)

	require.NoError(t, selector.SwitchModel(context.Background(), "vendor/model"))
	require.Len(t, built, 1)
	assert.Equal(t, ModelSpec{ProviderID: "openai", ModelID: "vendor/model"}, built[0])
	assert.Equal(t, ModelSpec{ProviderID: "openai", ModelID: "vendor/model"}, selector.CurrentModelSpec())

	require.NoError(t, selector.SwitchModelSpec(context.Background(), ModelSpec{
		ProviderID: " CUSTOM ",
		ModelID:    "shared-model",
		Info:       ModelInfo{Name: "ignored"},
		Known:      true,
	}))
	require.Len(t, built, 2)
	assert.Equal(t, ModelSpec{ProviderID: "custom", ModelID: "shared-model"}, built[1], "the builder must receive identity only")
	assert.Equal(t, ModelSpec{ProviderID: "custom", ModelID: "shared-model"}, selector.CurrentModelSpec())

	failModel = true
	err := selector.SwitchModelSpec(context.Background(), ModelSpec{ProviderID: "openai", ModelID: "rejected-model"})
	require.Error(t, err)
	assert.Equal(t, ModelSpec{ProviderID: "custom", ModelID: "shared-model"}, selector.CurrentModelSpec(), "failed switch must leave the current identity intact")
}

type modelCatalogSelectorStub struct{}

func (*modelCatalogSelectorStub) Generate(context.Context, []Message, ...Option) (*Message, error) {
	return &Message{Role: RoleAssistant, Content: "ok"}, nil
}

func (*modelCatalogSelectorStub) Stream(context.Context, []Message, ...Option) (<-chan MessageChunk, error) {
	chunks := make(chan MessageChunk, 1)
	chunks <- MessageChunk{Role: RoleAssistant, Content: "ok", Final: true}
	close(chunks)
	return chunks, nil
}
