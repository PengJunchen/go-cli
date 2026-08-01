package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultValidatorInterfaceSatisdition(t *testing.T) {
	var v Validator = NewDefaultValidator()
	assert.NotNil(t, v)
}

func TestDefaultValidator_ValidDefault(t *testing.T) {
	v := NewDefaultValidator()
	err := v.Validate(*defaultConfig())
	require.NoError(t, err)
}

func TestDefaultValidator_ValidEmptyModel(t *testing.T) {
	// A config with no model configured (empty name) is still valid.
	cfg := defaultConfig()
	cfg.Model = ModelConfig{}
	err := NewDefaultValidator().Validate(*cfg)
	require.NoError(t, err)
}

func TestDefaultValidator_InvalidTemperature(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider.Temperature = 3.0
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "temperature")

	cfg = defaultConfig()
	cfg.Model.Temperature = -1.0
	err = NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "temperature")
}

func TestDefaultValidator_InvalidTracing(t *testing.T) {
	cfg := defaultConfig()
	cfg.Tracing.Level = "verbose"
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracing level")

	cfg = defaultConfig()
	cfg.Tracing.Exporter = "otlp"
	err = NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tracing exporter")
}

func TestDefaultValidator_InvalidCompaction(t *testing.T) {
	cfg := defaultConfig()
	cfg.Compaction.MaxTokens = 0
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compaction")
}
