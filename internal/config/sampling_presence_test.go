package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnmarshalConfigSamplingPresenceJSONAndYAML(t *testing.T) {
	tests := []struct {
		name   string
		format ConfigFormat
		body   string
	}{
		{
			name:   "json",
			format: ConfigFormatJSON,
			body:   `{"provider":{"temperature":0,"max_tokens":0},"model":{"temperature":0.5,"max_tokens":32},"small_model":{"temperature":0,"max_tokens":12}}`,
		},
		{
			name:   "yaml",
			format: ConfigFormatYAML,
			body:   "provider:\n  temperature: 0\n  max_tokens: 0\nmodel:\n  temperature: 0.5\n  max_tokens: 32\nsmall_model:\n  temperature: 0\n  max_tokens: 12\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			require.NoError(t, UnmarshalConfig([]byte(tt.body), tt.format, &cfg))

			provider := cfg.ProviderSampling()
			assert.True(t, provider.TemperatureSet)
			assert.Equal(t, 0.0, provider.Temperature)
			assert.True(t, provider.MaxTokensSet)
			assert.Zero(t, provider.MaxTokens)

			model := cfg.ModelSampling()
			assert.True(t, model.TemperatureSet)
			assert.Equal(t, 0.5, model.Temperature)
			assert.True(t, model.MaxTokensSet)
			assert.Equal(t, 32, model.MaxTokens)

			small := cfg.SmallModelSampling()
			assert.True(t, small.TemperatureSet)
			assert.Zero(t, small.Temperature)
			assert.True(t, small.MaxTokensSet)
			assert.Equal(t, 12, small.MaxTokens)
		})
	}
}

func TestUnmarshalConfigSamplingAbsentFieldsStayUnset(t *testing.T) {
	for _, tt := range []struct {
		name   string
		format ConfigFormat
		body   string
	}{
		{name: "json", format: ConfigFormatJSON, body: `{"provider":{},"model":{},"small_model":{}}`},
		{name: "yaml", format: ConfigFormatYAML, body: "provider: {}\nmodel: {}\nsmall_model: {}\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			require.NoError(t, UnmarshalConfig([]byte(tt.body), tt.format, &cfg))
			assert.Equal(t, SamplingSettings{}, cfg.ProviderSampling())
			assert.Equal(t, SamplingSettings{}, cfg.ModelSampling())
			assert.Equal(t, SamplingSettings{}, cfg.SmallModelSampling())
		})
	}
}

func TestUnmarshalConfigRejectsJSONNullSamplingValues(t *testing.T) {
	for _, section := range []string{"provider", "model", "small_model"} {
		for _, field := range []string{"temperature", "max_tokens"} {
			t.Run(section+"/"+field, func(t *testing.T) {
				body := []byte(`{"` + section + `":{"` + field + `":null}}`)
				var cfg Config
				err := UnmarshalConfig(body, ConfigFormatJSON, &cfg)
				require.Error(t, err, "JSON null must not become an explicit zero sampling value")
				assert.Contains(t, err.Error(), "null")
			})
		}
	}
}

func TestLoaderRejectsJSONNullInsteadOfOverridingSamplingFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"provider":{"max_tokens":null}}`), 0o600))

	cfg, err := NewLoader().WithFile(path).Load(context.Background())
	require.Error(t, err, "invalid null must fail the whole load instead of replacing the 4096 fallback")
	assert.Nil(t, cfg)
	assert.Contains(t, err.Error(), "null")
}

func TestLoaderSamplingPresenceAndZeroAwarePriority(t *testing.T) {
	for _, key := range []string{
		"GO_CLI_TEST_SAMPLING_PROVIDER_TEMPERATURE",
		"GO_CLI_TEST_SAMPLING_PROVIDER_MAX_TOKENS",
		"GO_CLI_TEST_SAMPLING_MODEL_TEMPERATURE",
		"GO_CLI_TEST_SAMPLING_MODEL_MAX_TOKENS",
	} {
		t.Setenv(key, "")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "provider:\n  temperature: 0.8\n  max_tokens: 512\nmodel:\n  temperature: 0.6\n  max_tokens: 256\nsmall_model:\n  temperature: 0.4\n  max_tokens: 128\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	t.Setenv("GO_CLI_TEST_SAMPLING_PROVIDER_TEMPERATURE", "0")
	t.Setenv("GO_CLI_TEST_SAMPLING_MODEL_MAX_TOKENS", "0")

	cfg, err := NewLoader().WithFile(path).WithEnvPrefix("GO_CLI_TEST_SAMPLING").Load(context.Background())
	require.NoError(t, err)
	provider := cfg.ProviderSampling()
	assert.True(t, provider.TemperatureSet)
	assert.Zero(t, provider.Temperature, "a valid environment zero overrides the file value")
	assert.Equal(t, 512, provider.MaxTokens)
	model := cfg.ModelSampling()
	assert.Equal(t, 0.6, model.Temperature)
	assert.True(t, model.MaxTokensSet)
	assert.Zero(t, model.MaxTokens, "a valid environment zero overrides the file value")
	small := cfg.SmallModelSampling()
	assert.Equal(t, 0.4, small.Temperature)
	assert.True(t, small.TemperatureSet)
	assert.Equal(t, 128, small.MaxTokens)
	assert.True(t, small.MaxTokensSet)
}

func TestSamplingProgrammaticCompatibilityAndExplicitZeroAPI(t *testing.T) {
	cfg := &Config{
		Provider: ProviderConfig{Temperature: 0.8, MaxTokens: 512},
		Model:    ModelConfig{Temperature: 0.4, MaxTokens: 128},
	}
	assert.Equal(t, SamplingSettings{Temperature: 0.8, TemperatureSet: true, MaxTokens: 512, MaxTokensSet: true}, cfg.ProviderSampling())
	assert.Equal(t, SamplingSettings{Temperature: 0.4, TemperatureSet: true, MaxTokens: 128, MaxTokensSet: true}, cfg.ModelSampling())

	zero := &Config{}
	zero.Provider.SetTemperature(0)
	zero.Provider.SetMaxTokens(0)
	zero.Model.SetTemperature(0)
	zero.Model.SetMaxTokens(0)
	zero.SmallModel.SetTemperature(0)
	zero.SmallModel.SetMaxTokens(0)
	expected := SamplingSettings{TemperatureSet: true, MaxTokensSet: true}
	assert.Equal(t, expected, zero.ProviderSampling())
	assert.Equal(t, expected, zero.ModelSampling())
	assert.Equal(t, expected, zero.SmallModelSampling())
}

func TestMergeConfigsPreservesProgrammaticExplicitZero(t *testing.T) {
	base := &Config{samplingPresenceKnown: true}
	base.Provider = ProviderConfig{Temperature: 0.8, MaxTokens: 512}
	base.Provider.SetTemperature(0.8)
	base.Provider.SetMaxTokens(512)
	base.Model = ModelConfig{Temperature: 0.4, MaxTokens: 128}
	base.Model.SetTemperature(0.4)
	base.Model.SetMaxTokens(128)

	override := &Config{}
	override.Model.SetTemperature(0)
	override.Model.SetMaxTokens(0)
	merged := mergeConfigs(base, override)

	model := merged.ModelSampling()
	assert.True(t, model.TemperatureSet)
	assert.Zero(t, model.Temperature)
	assert.True(t, model.MaxTokensSet)
	assert.Zero(t, model.MaxTokens)
}

func TestLoaderBuiltinTokenFallbackIsNotAnExplicitSamplingSetting(t *testing.T) {
	for _, key := range []string{"GO_CLI_PROVIDER_MAX_TOKENS", "GO_CLI_MODEL_MAX_TOKENS"} {
		t.Setenv(key, "")
	}
	cfg, err := NewLoader().Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, defaultMaxTokens, cfg.ProviderSampling().MaxTokens)
	assert.False(t, cfg.ProviderSampling().MaxTokensSet)
	assert.Equal(t, defaultMaxTokens, cfg.ModelSampling().MaxTokens)
	assert.False(t, cfg.ModelSampling().MaxTokensSet)
}
