package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCredentialCanary(t *testing.T) string {
	t.Helper()

	var random [16]byte
	_, err := rand.Read(random[:])
	require.NoError(t, err)
	return "runtime-canary-" + hex.EncodeToString(random[:])
}

func TestSmallModelSameProviderReusesPrimaryCredential(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const providerName = "custom-gateway"
	primaryAPIKey := newTestCredentialCanary(t)
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    providerName,
			BaseURL: srv.URL,
			APIKey:  primaryAPIKey,
		},
		SmallModel: config.SmallModelConfig{Model: "small-model"},
	}
	model, _, cleanup, err := buildSmallModel(context.Background(), rc, providerName, providerName, "small-model")
	require.NoError(t, err)
	require.NotNil(t, model)
	if cleanup != nil {
		defer cleanup()
	}

	_, err = model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "summarize"}})
	require.NoError(t, err)

	count, path, authorization, _ := capture.snapshot()
	assert.Equal(t, 1, count)
	assert.Equal(t, "/chat/completions", path)
	assert.Equal(t, "Bearer "+primaryAPIKey, authorization)
}

func TestSmallModelProviderChangeDoesNotInheritPrimaryConfiguration(t *testing.T) {
	primaryCapture := &providerRequestCapture{}
	primarySrv := httptest.NewServer(http.HandlerFunc(primaryCapture.handler))
	t.Cleanup(primarySrv.Close)

	const primaryProviderName = "anthropic"
	const smallProviderName = "openai"
	primaryAPIKey := newTestCredentialCanary(t)
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    primaryProviderName,
			BaseURL: primarySrv.URL,
			APIKey:  primaryAPIKey,
		},
		SmallModel: config.SmallModelConfig{
			Provider: smallProviderName,
			Model:    "small-model",
		},
	}
	model, _, cleanup, err := buildSmallModel(context.Background(), rc, primaryProviderName, smallProviderName, "small-model")
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		_, requestErr := model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "summarize"}})
		assert.NoError(t, requestErr)
	}

	assert.Error(t, err, "a different small provider without its own endpoint or key must fail closed")
	assert.Nil(t, model)
	count, _, authorization, body := primaryCapture.snapshot()
	assert.Zero(t, count, "the primary endpoint must not receive a cross-provider request")
	assert.NotContains(t, authorization, primaryAPIKey)
	assert.NotContains(t, body, primaryAPIKey)
}

func TestSmallModelDifferentEndpointDoesNotInheritPrimaryCredential(t *testing.T) {
	primaryCapture := &providerRequestCapture{}
	primarySrv := httptest.NewServer(http.HandlerFunc(primaryCapture.handler))
	t.Cleanup(primarySrv.Close)
	smallCapture := &providerRequestCapture{}
	smallSrv := httptest.NewServer(http.HandlerFunc(smallCapture.handler))
	t.Cleanup(smallSrv.Close)

	const providerName = "custom-gateway"
	primaryAPIKey := newTestCredentialCanary(t)
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    providerName,
			BaseURL: primarySrv.URL,
			APIKey:  primaryAPIKey,
		},
		SmallModel: config.SmallModelConfig{
			Provider: providerName,
			BaseURL:  smallSrv.URL,
			Model:    "small-model",
		},
	}
	model, _, cleanup, err := buildSmallModel(context.Background(), rc, providerName, providerName, "small-model")
	require.NoError(t, err)
	require.NotNil(t, model)
	if cleanup != nil {
		defer cleanup()
	}

	_, err = model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "summarize"}})
	require.NoError(t, err)

	primaryCount, _, primaryAuthorization, _ := primaryCapture.snapshot()
	smallCount, _, smallAuthorization, smallBody := smallCapture.snapshot()
	assert.Zero(t, primaryCount)
	assert.NotContains(t, primaryAuthorization, primaryAPIKey)
	assert.Equal(t, 1, smallCount)
	assert.Empty(t, smallAuthorization, "an explicit different origin must not inherit the primary key")
	assert.NotContains(t, smallBody, primaryAPIKey)
}

func TestSmallModelSameOriginMayReusePrimaryCredential(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const providerName = "custom-gateway"
	primaryAPIKey := newTestCredentialCanary(t)
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    providerName,
			BaseURL: srv.URL + "/primary",
			APIKey:  primaryAPIKey,
		},
		SmallModel: config.SmallModelConfig{
			Provider: providerName,
			BaseURL:  srv.URL + "/small",
			Model:    "small-model",
		},
	}
	model, _, cleanup, err := buildSmallModel(context.Background(), rc, providerName, providerName, "small-model")
	require.NoError(t, err)
	require.NotNil(t, model)
	if cleanup != nil {
		defer cleanup()
	}

	_, err = model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "summarize"}})
	require.NoError(t, err)

	count, path, authorization, _ := capture.snapshot()
	assert.Equal(t, 1, count)
	assert.Equal(t, "/small/chat/completions", path)
	assert.Equal(t, "Bearer "+primaryAPIKey, authorization)
}

func TestSmallModelOwnCredentialStaysAtOwnOrigin(t *testing.T) {
	primaryCapture := &providerRequestCapture{}
	primarySrv := httptest.NewServer(http.HandlerFunc(primaryCapture.handler))
	t.Cleanup(primarySrv.Close)
	smallCapture := &providerRequestCapture{}
	smallSrv := httptest.NewServer(http.HandlerFunc(smallCapture.handler))
	t.Cleanup(smallSrv.Close)

	primaryAPIKey := newTestCredentialCanary(t)
	smallAPIKey := newTestCredentialCanary(t)
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:    "primary-gateway",
			BaseURL: primarySrv.URL,
			APIKey:  primaryAPIKey,
		},
		SmallModel: config.SmallModelConfig{
			Provider: "small-gateway",
			BaseURL:  smallSrv.URL,
			APIKey:   smallAPIKey,
			Model:    "small-model",
		},
	}
	model, _, cleanup, err := buildSmallModel(context.Background(), rc, "primary-gateway", "small-gateway", "small-model")
	require.NoError(t, err)
	require.NotNil(t, model)
	if cleanup != nil {
		defer cleanup()
	}

	_, err = model.Generate(withCLITestOperation(context.Background()), []llm.Message{{Role: llm.RoleUser, Content: "summarize"}})
	require.NoError(t, err)

	primaryCount, _, primaryAuthorization, _ := primaryCapture.snapshot()
	smallCount, _, smallAuthorization, _ := smallCapture.snapshot()
	assert.Zero(t, primaryCount)
	assert.NotContains(t, primaryAuthorization, smallAPIKey)
	assert.Equal(t, 1, smallCount)
	assert.Equal(t, "Bearer "+smallAPIKey, smallAuthorization)
}

func TestSmallModelSameIdentityUsesNormalizedDefaultOrigin(t *testing.T) {
	primaryAPIKey := newTestCredentialCanary(t)
	rc := &config.Config{
		Provider: config.ProviderConfig{APIKey: primaryAPIKey},
		SmallModel: config.SmallModelConfig{
			BaseURL: "https://api.openai.com:443/v1/small",
		},
	}

	baseURL, apiKey, err := resolveSmallModelProviderSettings("eino", "eino", rc)
	require.NoError(t, err)
	assert.Equal(t, rc.SmallModel.BaseURL, baseURL)
	assert.Equal(t, primaryAPIKey, apiKey, "an explicit path on the same default origin may reuse the provider credential")
}
