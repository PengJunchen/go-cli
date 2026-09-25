package llm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelResolverListModelsKeepsProviderScopedIdentityAndStableOrder(t *testing.T) {
	registry := NewProviderRegistryWithDefaultProviderOptions(WithModels([]ModelInfo{
		{Name: "z-model"},
		{Name: "shared-model", Description: "default entry"},
	}))
	require.NoError(t, registry.Register(&modelCatalogFixtureProvider{
		name: "alpha",
		models: []ModelInfo{
			{Name: "shared-model", Description: "first alpha entry"},
			{Name: "z-model"},
			{Name: "shared-model", Description: "duplicate alpha entry"},
		},
	}))
	require.NoError(t, registry.Register(&modelCatalogFixtureProvider{
		name:   "zeta",
		models: []ModelInfo{{Name: "shared-model"}},
	}))
	require.NoError(t, registry.Register(NewClaudeProvider(WithNativeModels([]ModelInfo{{Name: "native-model"}}))))

	models := registry.SnapshotResolver().ListModels()
	got := make([]string, 0, len(models))
	for _, model := range models {
		got = append(got, model.ProviderID+"/"+model.ModelID)
		assert.True(t, model.Known)
	}
	assert.Equal(t, []string{
		"alpha/shared-model",
		"alpha/z-model",
		"eino/shared-model",
		"eino/z-model",
		"zeta/shared-model",
	}, got)

	var alphaShared ModelSpec
	for _, model := range models {
		if model.ProviderID == "alpha" && model.ModelID == "shared-model" {
			alphaShared = model
			break
		}
	}
	assert.Equal(t, "first alpha entry", alphaShared.Info.Description, "same-provider duplicates keep the first static metadata")

	models[0].Info.Name = "mutated"
	assert.Equal(t, "shared-model", registry.SnapshotResolver().ListModels()[0].Info.Name, "catalog results must not mutate the resolver snapshot")
}

type modelCatalogFixtureProvider struct {
	name   string
	models []ModelInfo
}

func (p *modelCatalogFixtureProvider) Name() string { return p.name }

func (p *modelCatalogFixtureProvider) Models() []ModelInfo {
	return append([]ModelInfo(nil), p.models...)
}

func (p *modelCatalogFixtureProvider) Build(context.Context, ModelConfig) (BaseChatModel, func(), error) {
	return &stubModel{}, nil, nil
}

func (p *modelCatalogFixtureProvider) resolverSpec() ProviderSpec {
	return ProviderSpec{
		ProviderID:              p.name,
		ProtocolID:              ProtocolOpenAICompatible,
		DefaultBaseURL:          "http://127.0.0.1:8128/v1",
		DefaultEndpointExplicit: true,
		Enabled:                 true,
	}
}
