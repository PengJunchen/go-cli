package config

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestDefaultValidatorInterfaceSatisdition(t *testing.T) {
	v := NewDefaultValidator()
	assert.NotNil(t, v)
}

func TestDefaultValidator_ValidDefault(t *testing.T) {
	v := NewDefaultValidator()
	err := v.Validate(*defaultConfig())
	require.NoError(t, err)
}

func TestDefaultValidator_WebSearchProviderValues(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		provider string
	}{
		{name: "unset"},
		{name: "mock", provider: "mock"},
		{name: "fetch", provider: "fetch"},
		{name: "brave", provider: "brave"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.WebSearch.Provider = testCase.provider
			require.NoError(t, NewDefaultValidator().Validate(*cfg))
		})
	}

	cfg := defaultConfig()
	cfg.WebSearch.Provider = "not-a-provider"
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "web_search provider")
}

func TestDefaultValidatorLoopDetectorDisposition(t *testing.T) {
	for _, disposition := range []string{"", "warn", "steer", "terminate"} {
		t.Run("valid/"+disposition, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Production.LoopDetector.Disposition = disposition
			require.NoError(t, NewDefaultValidator().Validate(*cfg))
		})
	}

	cfg := defaultConfig()
	cfg.Production.LoopDetector.Disposition = "stop"
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "production.loop_detector disposition")
}

func TestDefaultValidatorProductionModelTimeout(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		total      string
		streamIdle string
	}{
		{name: "defaults"},
		{name: "explicit bounds", total: "24h", streamIdle: "24h"},
		{name: "total only within default idle", total: "30s"},
		{name: "idle only within default total", streamIdle: "30s"},
	} {
		t.Run("valid/"+testCase.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Production.ModelTimeout.Total = testCase.total
			cfg.Production.ModelTimeout.StreamIdle = testCase.streamIdle
			require.NoError(t, NewDefaultValidator().Validate(*cfg))
		})
	}

	for _, testCase := range []struct {
		name       string
		total      string
		streamIdle string
		want       string
	}{
		{name: "invalid total", total: "later", want: "production.model_timeout.total"},
		{name: "zero total", total: "0s", want: "production.model_timeout.total"},
		{name: "negative idle", streamIdle: "-1s", want: "production.model_timeout.stream_idle"},
		{name: "too-large idle", streamIdle: "24h1s", want: "production.model_timeout.stream_idle"},
		{name: "idle exceeds total", total: "5s", streamIdle: "6s", want: "stream_idle must be no greater than total"},
		{name: "total below default idle", total: "5s", want: "stream_idle must be no greater than total"},
	} {
		t.Run("invalid/"+testCase.name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Production.ModelTimeout.Total = testCase.total
			cfg.Production.ModelTimeout.StreamIdle = testCase.streamIdle
			err := NewDefaultValidator().Validate(*cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), testCase.want)
		})
	}
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

func TestDefaultValidatorTracingExporterMatchesRuntimeSupport(t *testing.T) {
	for _, name := range []string{"", "jsonl", "stdout", "none", "otlp", "kafka", "bogus"} {
		t.Run(name, func(t *testing.T) {
			cfg := defaultConfig()
			cfg.Tracing.Exporter = name
			err := NewDefaultValidator().Validate(*cfg)
			if tracing.IsSupportedExporter(name) {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), "tracing exporter")
		})
	}
}

func TestDefaultValidator_InvalidCompaction(t *testing.T) {
	cfg := defaultConfig()
	cfg.Compaction.MaxTokens = 0
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compaction")
}

func TestDefaultValidator_InsecureBaseURL(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	v := &DefaultValidator{logger: logger}

	cfg := defaultConfig()
	cfg.Provider.BaseURL = "http://api.openai.com/v1"
	err := v.Validate(*cfg)
	require.NoError(t, err) // HTTP base_url is a warning, not an error
	assert.Contains(t, buf.String(), "insecure_base_url")
}

func TestDefaultValidator_LocalhostHTTPBaseURL(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider.BaseURL = "http://localhost:8080/v1"
	err := NewDefaultValidator().Validate(*cfg)
	require.NoError(t, err)

	cfg = defaultConfig()
	cfg.Provider.BaseURL = "http://127.0.0.1:8080/v1"
	err = NewDefaultValidator().Validate(*cfg)
	require.NoError(t, err)
}

func TestDefaultValidator_HTTPSBaseURL(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider.BaseURL = "https://api.openai.com/v1"
	err := NewDefaultValidator().Validate(*cfg)
	require.NoError(t, err)
}

func TestDefaultValidator_PathTraversal(t *testing.T) {
	cfg := defaultConfig()
	cfg.Tracing.FilePath = "../../etc/passwd"
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path traversal")

	// SSH key path with traversal should also be rejected.
	cfg = defaultConfig()
	cfg.Remote.Hosts = map[string]SSHHostConfig{
		"myhost": {Host: "example.com", User: "root", KeyPath: "../../../.ssh/id_rsa"},
	}
	err = NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "path traversal")
}

func TestDefaultValidator_CleanPath(t *testing.T) {
	cfg := defaultConfig()
	cfg.Tracing.FilePath = "/var/log/go-cli/trace.jsonl"
	cfg.Skill.Dir = "/etc/go-cli/skills"
	err := NewDefaultValidator().Validate(*cfg)
	require.NoError(t, err)
}

func TestDefaultValidator_SSHPasswordWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	v := &DefaultValidator{logger: logger}

	cfg := defaultConfig()
	cfg.Remote.Hosts = map[string]SSHHostConfig{
		"myhost": {Host: "example.com", User: "root", Password: "s3cret"},
	}

	err := v.Validate(*cfg)
	require.NoError(t, err) // password is a warning, not an error
	output := buf.String()
	assert.Contains(t, output, "insecure_ssh_password")
	assert.Contains(t, output, "key-based authentication")
}
