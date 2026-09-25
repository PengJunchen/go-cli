package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIModelResolverRegistersConfiguredModelCatalog(t *testing.T) {
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "openai",
			APIKey:  "catalog-primary-canary",
			Model:   "provider-model",
			BaseURL: "",
		},
		Model: config.ModelConfig{Name: "legacy-model"},
		SmallModel: config.SmallModelConfig{
			Model: "small-model",
		},
	}

	modelResolver, err := newCLIModelResolver(rc, "openai", true)
	require.NoError(t, err)

	for _, modelID := range []string{"provider-model", "small-model"} {
		resolved, resolveErr := modelResolver.resolve(context.Background(), llm.ModelRequest{
			ProviderID: "openai",
			ModelID:    modelID,
		})
		require.NoError(t, resolveErr)
		assert.True(t, resolved.Model.Known, "configured model %q should be in the provider snapshot", modelID)
		assert.Equal(t, modelID, resolved.Model.ModelID)
	}

	assert.Equal(t, []llm.ModelSpec{
		{ProviderID: "openai", ModelID: "provider-model", Info: llm.ModelInfo{Name: "provider-model"}, Known: true},
		{ProviderID: "openai", ModelID: "small-model", Info: llm.ModelInfo{Name: "small-model"}, Known: true},
	}, modelResolver.listModelSpecs())
}

func TestCLIModelResolverCatalogKeepsSameModelIDProviderScoped(t *testing.T) {
	const sharedModel = "shared-model"
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:   "openai",
			APIKey: "primary-canary-key",
			Model:  sharedModel,
		},
		SmallModel: config.SmallModelConfig{
			Provider: "custom-compatible",
			BaseURL:  "http://127.0.0.1:8127/v1",
			APIKey:   "small-canary-key",
			Model:    sharedModel,
		},
	}

	modelResolver, err := newCLIModelResolver(rc, "openai", true)
	require.NoError(t, err)

	for _, providerID := range []string{"openai", "custom-compatible"} {
		resolved, resolveErr := modelResolver.resolve(context.Background(), llm.ModelRequest{
			ProviderID: providerID,
			ModelID:    sharedModel,
		})
		require.NoError(t, resolveErr)
		assert.True(t, resolved.Model.Known, "configured model should be registered under %q", providerID)
		assert.Equal(t, providerID, resolved.Model.ProviderID)
		assert.Equal(t, sharedModel, resolved.Model.ModelID)
	}

	assert.Equal(t, []llm.ModelSpec{
		{ProviderID: "custom-compatible", ModelID: sharedModel, Info: llm.ModelInfo{Name: sharedModel}, Known: true},
		{ProviderID: "openai", ModelID: sharedModel, Info: llm.ModelInfo{Name: sharedModel}, Known: true},
	}, modelResolver.listModelSpecs())
}

func TestCLIModelResolverCatalogUsesFinalModelOverrideWithoutSplittingModelID(t *testing.T) {
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:   "openai",
			APIKey: "catalog-override-canary",
			Model:  "configured-model",
		},
		Model: config.ModelConfig{Name: "legacy-model"},
	}

	modelResolver, err := newCLIModelResolver(rc, "openai", false, "vendor/model-with-slash")
	require.NoError(t, err)
	assert.Equal(t, []llm.ModelSpec{{
		ProviderID: "openai",
		ModelID:    "vendor/model-with-slash",
		Info:       llm.ModelInfo{Name: "vendor/model-with-slash"},
		Known:      true,
	}}, modelResolver.listModelSpecs())
}

func TestCLIModelResolverScopedSwitchKeepsCredentialAndSamplingWithTargetProvider(t *testing.T) {
	primaryCapture := &providerRequestCapture{}
	primaryServer := httptest.NewServer(http.HandlerFunc(primaryCapture.handler))
	t.Cleanup(primaryServer.Close)

	smallCapture := &providerRequestCapture{}
	smallServer := httptest.NewServer(http.HandlerFunc(smallCapture.handler))
	t.Cleanup(smallServer.Close)

	const primaryProvider = "primary-compatible"
	const smallProvider = "small-compatible"
	const sharedModel = "shared-model"
	const primaryKey = "primary-scope-canary-key"
	const smallKey = "small-scope-canary-key"
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:        primaryProvider,
			BaseURL:     primaryServer.URL + "/primary",
			APIKey:      primaryKey,
			Model:       sharedModel,
			Temperature: 0.3,
			MaxTokens:   120,
		},
		Model: config.ModelConfig{
			Temperature: 0.4,
			MaxTokens:   128,
		},
		SmallModel: config.SmallModelConfig{
			Provider:    smallProvider,
			BaseURL:     smallServer.URL + "/small",
			APIKey:      smallKey,
			Model:       sharedModel,
			Temperature: 0.9,
			MaxTokens:   64,
		},
	}

	modelResolver, err := newCLIModelResolver(rc, primaryProvider, true, sharedModel)
	require.NoError(t, err)
	catalog := modelResolver.listModelSpecs()
	require.Len(t, catalog, 2)
	assert.Equal(t, []llm.ModelSpec{
		{ProviderID: primaryProvider, ModelID: sharedModel, Info: llm.ModelInfo{Name: sharedModel}, Known: true},
		{ProviderID: smallProvider, ModelID: sharedModel, Info: llm.ModelInfo{Name: sharedModel}, Known: true},
	}, catalog)

	var candidate llm.ModelSpec
	for _, model := range catalog {
		if model.ProviderID == smallProvider {
			candidate = model
		}
	}
	model, _, cleanup, err := modelResolver.buildInteractive(context.Background(), rc, candidate)
	require.NoError(t, err)
	if cleanup != nil {
		defer cleanup()
	}
	_, err = model.Generate(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "scope fixture"}})
	require.NoError(t, err)

	primaryCount, _, primaryAuthorization, primaryBody := primaryCapture.snapshot()
	smallCount, _, smallAuthorization, smallBody := smallCapture.snapshot()
	assert.Zero(t, primaryCount, "cross-provider switching must not dispatch to the primary origin")
	assert.NotContains(t, primaryAuthorization+primaryBody, smallKey)
	assert.Equal(t, 1, smallCount)
	assert.Equal(t, "Bearer "+smallKey, smallAuthorization)
	assert.NotContains(t, smallBody, primaryKey)
	var request map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(smallBody), &request))
	var modelID string
	require.NoError(t, json.Unmarshal(request["model"], &modelID))
	assert.Equal(t, sharedModel, modelID)
	assert.NotContains(t, request, "temperature", "neither primary nor small-only sampling should cross provider identity")
	var maxTokens int
	require.NoError(t, json.Unmarshal(request["max_tokens"], &maxTokens))
	assert.Equal(t, config.DefaultLLMMaxTokens, maxTokens, "cross-provider switches use generic sampling defaults")
}

func TestCLIModelResolverSamePrimaryAndSmallIdentityUsesPrimarySampling(t *testing.T) {
	capture := &providerRequestCapture{}
	server := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(server.Close)

	const providerName = "role-compatible"
	const sharedModel = "same-role-model"
	const apiKey = "same-origin-scope-canary"
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    providerName,
			BaseURL: server.URL + "/scope",
			APIKey:  apiKey,
			Model:   sharedModel,
		},
		Model: config.ModelConfig{
			Temperature: 0.35,
			MaxTokens:   120,
		},
		SmallModel: config.SmallModelConfig{
			Provider:    providerName,
			BaseURL:     server.URL + "/scope",
			APIKey:      apiKey,
			Model:       sharedModel,
			Temperature: 0.9,
			MaxTokens:   64,
		},
	}

	modelResolver, err := newCLIModelResolver(rc, providerName, true, sharedModel)
	require.NoError(t, err)
	require.Equal(t, []llm.ModelSpec{{
		ProviderID: providerName,
		ModelID:    sharedModel,
		Info:       llm.ModelInfo{Name: sharedModel},
		Known:      true,
	}}, modelResolver.listModelSpecs(), "primary/small role overlap must have one catalog entry")

	model, _, cleanup, err := modelResolver.buildInteractive(context.Background(), rc, llm.ModelSpec{
		ProviderID: providerName,
		ModelID:    sharedModel,
	})
	require.NoError(t, err)
	if cleanup != nil {
		defer cleanup()
	}
	_, err = model.Generate(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "role fixture"}})
	require.NoError(t, err)

	count, _, authorization, body := capture.snapshot()
	assert.Equal(t, 1, count)
	assert.Equal(t, "Bearer "+apiKey, authorization)
	request := decodeSamplingRequestBody(t, capture)
	assertSamplingNumber(t, request, "temperature", "0.35")
	assertSamplingNumber(t, request, "max_tokens", "120")
	assert.NotContains(t, body, "0.9")
	assert.NotContains(t, body, "64")
}
