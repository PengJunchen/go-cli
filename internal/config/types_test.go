package config //nolint:scan003

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultConfigDefaults(t *testing.T) {
	cfg := defaultConfig()

	assert.False(t, cfg.Verbose())
	assert.Equal(t, defaultMaxTokens, cfg.Model.MaxTokens)
	assert.Equal(t, defaultMaxTokens, cfg.Provider.MaxTokens)
	assert.NotNil(t, cfg.Tracing.Enabled)
	assert.True(t, *cfg.Tracing.Enabled)
	assert.Equal(t, defaultTracingExporter, cfg.Tracing.Exporter)
	assert.Equal(t, defaultTracingLevel, cfg.Tracing.Level)
	assert.Equal(t, defaultCompactionMaxTokens, cfg.Compaction.MaxTokens)
}

func TestConfigVerbose(t *testing.T) {
	cfg := &Config{verbose: true}
	assert.True(t, cfg.Verbose())
	cfg = &Config{verbose: false}
	assert.False(t, cfg.Verbose())
}

func TestSourceOrder(t *testing.T) {
	// A later source must have a strictly higher priority value.
	var order = []Source{
		SourceDefault, SourceFile, SourceEnv, SourceFlag, SourceOverride,
	}
	for i := 1; i < len(order); i++ {
		assert.Greater(t, int(order[i]), int(order[i-1]))
	}
}

func TestSourceString(t *testing.T) {
	assert.Equal(t, "default", SourceDefault.String())
	assert.Equal(t, "file", SourceFile.String())
	assert.Equal(t, "env", SourceEnv.String())
	assert.Equal(t, "flag", SourceFlag.String())
	assert.Equal(t, "override", SourceOverride.String())
	assert.Equal(t, "unknown", Source(99).String())
}

func TestProviderAndModelNestedFields(t *testing.T) {
	cfg := &Config{
		Provider: ProviderConfig{
			Name: "openai", APIKey: "sk-test", BaseURL: "https://example.com",
			Model: "gpt-4", Temperature: 0.7, MaxTokens: 4096,
		},
		Model: ModelConfig{Name: "gpt-4", Temperature: 0.7, MaxTokens: 4096},
	}
	assert.Equal(t, "openai", cfg.Provider.Name)
	assert.Equal(t, "sk-test", cfg.Provider.APIKey)
	assert.Equal(t, "gpt-4", cfg.Model.Name)
	assert.Equal(t, 4096, cfg.Model.MaxTokens)
}

func TestConfigIsValidByDefault(t *testing.T) {
	err := NewDefaultValidator().Validate(*defaultConfig())
	require.NoError(t, err)
}

func TestWebSearchConfigFields(t *testing.T) {
	cfg := &Config{
		WebSearch: WebSearchConfig{
			Provider: "brave",
			APIKey:   "test-key",
			Timeout:  "15s",
		},
	}
	assert.Equal(t, "brave", cfg.WebSearch.Provider)
	assert.Equal(t, "test-key", cfg.WebSearch.APIKey)
	assert.Equal(t, "15s", cfg.WebSearch.Timeout)
}

func TestWebSearchConfigDefaultsToEmpty(t *testing.T) {
	cfg := defaultConfig()
	assert.Equal(t, "", cfg.WebSearch.Provider, "default provider should be empty (treated as mock)")
	assert.Equal(t, "", cfg.WebSearch.APIKey)
	assert.Equal(t, "", cfg.WebSearch.Timeout)
}
