package config

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestDefaultValidator_InsecureBaseURL(t *testing.T) {
	cfg := defaultConfig()
	cfg.Provider.BaseURL = "http://api.openai.com/v1"
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "base_url must use HTTPS")
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
