package config

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoaderRejectsNonFiniteSamplingTemperatures(t *testing.T) {
	for _, section := range []struct {
		name string
		want string
	}{
		{name: "provider", want: "provider temperature out of range"},
		{name: "model", want: "model temperature out of range"},
		{name: "small_model", want: "small model temperature out of range"},
	} {
		for _, value := range []string{"NaN", "+Inf", "-Inf"} {
			t.Run(section.name+"/"+value, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "config.yaml")
				body := section.name + ":\n  temperature: " + value + "\n"
				require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
				cfg, err := NewLoader().WithFile(path).Load(context.Background())
				require.Error(t, err)
				assert.Nil(t, cfg)
				assert.Contains(t, err.Error(), section.want)
			})
		}
	}
}

func TestLoaderAllowsFiniteSamplingTemperatureBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "provider:\n  temperature: 0\nmodel:\n  temperature: 0\nsmall_model:\n  temperature: 0\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := NewLoader().WithFile(path).Load(context.Background())
	require.NoError(t, err)
	assert.Zero(t, cfg.Provider.Temperature)
	assert.Zero(t, cfg.Model.Temperature)
	assert.Zero(t, cfg.SmallModel.Temperature)
}

func TestValidatorRejectsCodeBuiltNonFiniteSamplingTemperatures(t *testing.T) {
	sections := []struct {
		name string
		want string
		set  func(*Config, float64)
	}{
		{name: "provider", want: "provider temperature out of range", set: func(cfg *Config, value float64) { cfg.Provider.Temperature = value }},
		{name: "model", want: "model temperature out of range", set: func(cfg *Config, value float64) { cfg.Model.Temperature = value }},
		{name: "small_model", want: "small model temperature out of range", set: func(cfg *Config, value float64) { cfg.SmallModel.Temperature = value }},
	}
	for _, section := range sections {
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			t.Run(section.name+"/"+strconvFloatName(value), func(t *testing.T) {
				cfg := Config{Compaction: CompactionConfig{MaxTokens: 1}}
				section.set(&cfg, value)
				err := NewDefaultValidator().Validate(cfg)
				require.Error(t, err)
				assert.Contains(t, err.Error(), section.want)
			})
		}
	}
}

func strconvFloatName(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "+Inf"
	default:
		return "-Inf"
	}
}
