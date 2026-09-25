package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/llm"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssembledLoadedProviderSamplingFillsUnsetModelSettings(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	apiKey := newTestCredentialCanary(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := fmt.Sprintf("provider:\n  name: loaded-sampling-provider\n  base_url: %s\n  api_key: %s\n  temperature: 0.8\n  max_tokens: 512\nmodel:\n  name: loaded-sampling-model\n", srv.URL, apiKey)
	require.NoError(t, os.WriteFile(configPath, []byte(configBody), 0o600))
	rc, err := config.NewLoader().WithFile(configPath).WithEnvPrefix("R3_SAMPLING_PROVIDER_FALLBACK_UNUSED").Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0.8, rc.Provider.Temperature)
	assert.Equal(t, 512, rc.Provider.MaxTokens)
	assert.Equal(t, 4096, rc.Model.MaxTokens, "the effective loader value is a builtin fallback, not an explicit model setting")

	generateCapturedModelRequest(t, capture, rc, rc.Provider.Name, rc.Model.Name)
	request := decodeSamplingRequestBody(t, capture)
	assertSamplingNumber(t, request, "temperature", "0.8")
	assertSamplingNumber(t, request, "max_tokens", "512")
}

func TestAssembledLoadedExplicitModelZeroOverridesProviderSampling(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	apiKey := newTestCredentialCanary(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := fmt.Sprintf("provider:\n  name: zero-sampling-provider\n  base_url: %s\n  api_key: %s\n  temperature: 0.8\n  max_tokens: 512\nmodel:\n  name: zero-sampling-model\n  temperature: 0\n  max_tokens: 0\n", srv.URL, apiKey)
	require.NoError(t, os.WriteFile(configPath, []byte(configBody), 0o600))
	rc, err := config.NewLoader().WithFile(configPath).WithEnvPrefix("R3_SAMPLING_ZERO_MODEL_UNUSED").Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0.0, rc.Model.Temperature)
	assert.Equal(t, 0, rc.Model.MaxTokens, "explicit YAML zero must override the builtin 4096 fallback")

	generateCapturedModelRequest(t, capture, rc, rc.Provider.Name, rc.Model.Name)
	request := decodeSamplingRequestBody(t, capture)
	assertSamplingNumber(t, request, "temperature", "0")
	assertSamplingNumber(t, request, "max_tokens", "0")
}

func TestAssembledLoadedJSONExplicitZeroSamplingIsEncoded(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const providerName = "json-zero-sampling-provider"
	rawConfig, err := json.Marshal(config.Config{
		Provider: config.ProviderConfig{
			Name:        providerName,
			BaseURL:     srv.URL,
			APIKey:      newTestCredentialCanary(t),
			Temperature: 0.8,
			MaxTokens:   512,
		},
		Model: config.ModelConfig{
			Name:        "json-zero-sampling-model",
			Temperature: 0,
			MaxTokens:   0,
		},
	})
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(configPath, rawConfig, 0o600))
	rc, err := config.NewLoader().WithFile(configPath).WithEnvPrefix("R3_SAMPLING_JSON_ZERO_UNUSED").Load(context.Background())
	require.NoError(t, err)
	assert.True(t, rc.ModelSampling().TemperatureSet)
	assert.True(t, rc.ModelSampling().MaxTokensSet)

	generateCapturedModelRequest(t, capture, rc, rc.Provider.Name, rc.Model.Name)
	request := decodeSamplingRequestBody(t, capture)
	assertSamplingNumber(t, request, "temperature", "0")
	assertSamplingNumber(t, request, "max_tokens", "0")
}

func TestAssembledPerCallZeroSamplingIsEncoded(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const providerName = "per-call-zero-sampling-provider"
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:        providerName,
			BaseURL:     srv.URL,
			APIKey:      newTestCredentialCanary(t),
			Temperature: 0.8,
			MaxTokens:   512,
		},
		Model: config.ModelConfig{Temperature: 0.4, MaxTokens: 128},
	}

	generateCapturedModelRequest(t, capture, rc, providerName, "per-call-model", llm.WithTemperature(0), llm.WithMaxTokens(0))
	request := decodeSamplingRequestBody(t, capture)
	assertSamplingNumber(t, request, "temperature", "0")
	assertSamplingNumber(t, request, "max_tokens", "0")
}

func TestAssembledDefaultSamplingKeeps4096Fallback(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	apiKey := newTestCredentialCanary(t)
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configBody := fmt.Sprintf("provider:\n  name: builtin-sampling-provider\n  base_url: %s\n  api_key: %s\nmodel:\n  name: default-sampling-model\n", srv.URL, apiKey)
	require.NoError(t, os.WriteFile(configPath, []byte(configBody), 0o600))
	rc, err := config.NewLoader().WithFile(configPath).WithEnvPrefix("R3_SAMPLING_BUILTIN_DEFAULT_UNUSED").Load(context.Background())
	require.NoError(t, err)

	generateCapturedModelRequest(t, capture, rc, rc.Provider.Name, rc.Model.Name)
	request := decodeSamplingRequestBody(t, capture)
	assertSamplingNumber(t, request, "max_tokens", "4096")
	assert.NotContains(t, request, "temperature", "an unset temperature should be left to the adapter default")
}

func TestAssembledSmallModelSamplingUsesSmallThenMatchingProviderSettings(t *testing.T) {
	tests := []struct {
		name             string
		setSmallSampling bool
		wantTemperature  string
		wantMaxTokens    string
	}{
		{name: "matching provider fallback", wantTemperature: "0.8", wantMaxTokens: "512"},
		{name: "small explicit zero", setSmallSampling: true, wantTemperature: "0", wantMaxTokens: "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := &providerRequestCapture{}
			srv := httptest.NewServer(http.HandlerFunc(capture.handler))
			t.Cleanup(srv.Close)

			const providerName = "small-sampling-provider"
			rc := &config.Config{
				Provider: config.ProviderConfig{
					Name:        providerName,
					BaseURL:     srv.URL,
					APIKey:      newTestCredentialCanary(t),
					Temperature: 0.8,
					MaxTokens:   512,
				},
				Model: config.ModelConfig{Temperature: 0.3, MaxTokens: 64},
				SmallModel: config.SmallModelConfig{
					Model: "small-model",
				},
			}
			if tt.setSmallSampling {
				rc.SmallModel.SetTemperature(0)
				rc.SmallModel.SetMaxTokens(0)
			}

			assembly, err := AssembleAgent(context.Background(), rc, providerName, "primary-model", io.Discard)
			require.NoError(t, err)
			t.Cleanup(assembly.Cleanup)
			smallModel := assembly.ModelConfig.ModelSelector.SmallModel()
			require.NotNil(t, smallModel)
			response, err := smallModel.Generate(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "small sampling"}})
			require.NoError(t, err)
			require.NotNil(t, response)

			request := decodeSamplingRequestBody(t, capture)
			assertSamplingNumber(t, request, "temperature", tt.wantTemperature)
			assertSamplingNumber(t, request, "max_tokens", tt.wantMaxTokens)
		})
	}
}

func TestAssembledLoadedSmallModelExplicitZeroSamplingIsEncoded(t *testing.T) {
	capture := &providerRequestCapture{}
	srv := httptest.NewServer(http.HandlerFunc(capture.handler))
	t.Cleanup(srv.Close)

	const providerName = "loaded-small-zero-provider"
	rawConfig, err := json.Marshal(config.Config{
		Provider: config.ProviderConfig{
			Name:        providerName,
			BaseURL:     srv.URL,
			APIKey:      newTestCredentialCanary(t),
			Temperature: 0.8,
			MaxTokens:   512,
		},
		Model: config.ModelConfig{Name: "primary-model"},
		SmallModel: config.SmallModelConfig{
			Model:       "small-model",
			Temperature: 0,
			MaxTokens:   0,
		},
	})
	require.NoError(t, err)
	configPath := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(configPath, rawConfig, 0o600))
	rc, err := config.NewLoader().WithFile(configPath).WithEnvPrefix("R3_SMALL_ZERO_UNUSED").Load(context.Background())
	require.NoError(t, err)
	assert.True(t, rc.SmallModelSampling().TemperatureSet)
	assert.True(t, rc.SmallModelSampling().MaxTokensSet)

	assembly, err := AssembleAgent(context.Background(), rc, rc.Provider.Name, rc.Model.Name, io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	smallModel := assembly.ModelConfig.ModelSelector.SmallModel()
	require.NotNil(t, smallModel)
	response, err := smallModel.Generate(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "loaded small zero"}})
	require.NoError(t, err)
	require.NotNil(t, response)

	request := decodeSamplingRequestBody(t, capture)
	assertSamplingNumber(t, request, "temperature", "0")
	assertSamplingNumber(t, request, "max_tokens", "0")
}

func TestAssembledSmallModelDoesNotInheritOtherProviderSampling(t *testing.T) {
	primaryCapture := &providerRequestCapture{}
	primaryServer := httptest.NewServer(http.HandlerFunc(primaryCapture.handler))
	t.Cleanup(primaryServer.Close)
	smallCapture := &providerRequestCapture{}
	smallServer := httptest.NewServer(http.HandlerFunc(smallCapture.handler))
	t.Cleanup(smallServer.Close)

	const primaryProvider = "sampling-primary-provider"
	rc := &config.Config{
		Provider: config.ProviderConfig{
			Name:        primaryProvider,
			BaseURL:     primaryServer.URL,
			APIKey:      newTestCredentialCanary(t),
			Temperature: 0.8,
			MaxTokens:   512,
		},
		Model: config.ModelConfig{Temperature: 0.3, MaxTokens: 64},
		SmallModel: config.SmallModelConfig{
			Provider: "sampling-small-provider",
			BaseURL:  smallServer.URL,
			Model:    "small-model",
		},
	}

	assembly, err := AssembleAgent(context.Background(), rc, primaryProvider, "primary-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	smallModel := assembly.ModelConfig.ModelSelector.SmallModel()
	require.NotNil(t, smallModel)
	response, err := smallModel.Generate(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "small sampling"}})
	require.NoError(t, err)
	require.NotNil(t, response)

	request := decodeSamplingRequestBody(t, smallCapture)
	assert.NotContains(t, request, "temperature", "provider sampling cannot cross provider identities")
	assertSamplingNumber(t, request, "max_tokens", "4096")
	count, _, authorization, _ := smallCapture.snapshot()
	assert.Equal(t, 1, count)
	assert.Empty(t, authorization)
	primaryCount, _, _, _ := primaryCapture.snapshot()
	assert.Zero(t, primaryCount)
}

func TestLoaderRejectsInvalidSmallModelSamplingBeforeProviderDispatch(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*config.Config)
		wantError string
	}{
		{
			name: "temperature out of range",
			configure: func(cfg *config.Config) {
				cfg.SmallModel.Temperature = 2.1
			},
			wantError: "small model temperature out of range",
		},
		{
			name: "negative max tokens",
			configure: func(cfg *config.Config) {
				cfg.SmallModel.MaxTokens = -1
			},
			wantError: "small model max_tokens must be non-negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capture := &providerRequestCapture{}
			srv := httptest.NewServer(http.HandlerFunc(capture.handler))
			t.Cleanup(srv.Close)

			const providerName = "small-validation-provider"
			source := config.Config{
				Provider: config.ProviderConfig{
					Name:    providerName,
					BaseURL: srv.URL,
					APIKey:  newTestCredentialCanary(t),
				},
				Model:      config.ModelConfig{Name: "primary-model"},
				SmallModel: config.SmallModelConfig{Model: "small-model"},
			}
			tt.configure(&source)
			body, err := json.Marshal(source)
			require.NoError(t, err)
			configPath := filepath.Join(t.TempDir(), "config.json")
			require.NoError(t, os.WriteFile(configPath, body, 0o600))

			rc, loadErr := config.NewLoader().WithFile(configPath).WithEnvPrefix("R3_INVALID_SMALL_SAMPLING_UNUSED").Load(context.Background())
			if loadErr == nil {
				assembly, assembleErr := AssembleAgent(context.Background(), rc, providerName, rc.Model.Name, io.Discard)
				if assembleErr == nil {
					t.Cleanup(assembly.Cleanup)
					if smallModel := assembly.ModelConfig.ModelSelector.SmallModel(); smallModel != nil {
						_, _ = smallModel.Generate(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "invalid small sampling"}})
					}
				}
			}
			require.Error(t, loadErr)
			assert.Contains(t, loadErr.Error(), tt.wantError)
			count, _, _, _ := capture.snapshot()
			assert.Zero(t, count, "invalid small sampling must be rejected before any provider request")
		})
	}
}

func generateCapturedModelRequest(t *testing.T, capture *providerRequestCapture, rc *config.Config, providerName, modelName string, opts ...llm.Option) {
	t.Helper()
	assembly, err := AssembleAgent(context.Background(), rc, providerName, modelName, io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	response, err := assembly.ModelConfig.Model.Generate(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "sampling precedence"}}, opts...)
	require.NoError(t, err)
	require.NotNil(t, response)
	count, _, _, _ := capture.snapshot()
	assert.Equal(t, 1, count, "the assembly should send exactly one real provider request")
}

func decodeSamplingRequestBody(t *testing.T, capture *providerRequestCapture) map[string]json.RawMessage {
	t.Helper()
	_, _, _, body := capture.snapshot()
	var request map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	return request
}

func assertSamplingNumber(t *testing.T, request map[string]json.RawMessage, name, want string) {
	t.Helper()
	raw, ok := request[name]
	require.True(t, ok, "%s must be present in the wire request", name)
	assert.Equal(t, want, string(raw), "%s has the wrong sampling value", name)
}
