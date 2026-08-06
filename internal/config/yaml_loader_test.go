package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestConfigFormat_String(t *testing.T) {
	assert.Equal(t, "json", ConfigFormatJSON.String())
	assert.Equal(t, "yaml", ConfigFormatYAML.String())
	assert.Equal(t, "auto", ConfigFormatAuto.String())
	assert.Equal(t, "unknown", ConfigFormat(99).String())
}

func TestDetectConfigFormat(t *testing.T) {
	tests := []struct {
		path   string
		want   ConfigFormat
		errMsg string
	}{
		{"config.json", ConfigFormatJSON, ""},
		{"c.json", ConfigFormatJSON, ""},
		{"config.yaml", ConfigFormatYAML, ""},
		{"config.yml", ConfigFormatYAML, ""},
		{"CONFIG.YAML", ConfigFormatYAML, ""}, // extension is case-insensitive
		{"config.toml", ConfigFormatAuto, "unsupported"},
		{"config", ConfigFormatAuto, "unsupported"},
		{"config.txt", ConfigFormatAuto, "unsupported"},
	}
	for _, tc := range tests {
		got, err := DetectConfigFormat(tc.path)
		require.Equal(t, tc.want, got, "path %q", tc.path)
		if tc.errMsg == "" {
			assert.NoError(t, err)
		} else {
			require.Error(t, err, "path %q", tc.path)
			assert.Contains(t, err.Error(), tc.errMsg)
		}
	}
}

func TestUnmarshalConfig_JSON(t *testing.T) {
	doc := `{"model":{"name":"gpt-4","temperature":0.7},"tracing":{"enabled":false,"exporter":"stdout"},"compaction":{"max_tokens":32000}}`
	var cfg Config
	require.NoError(t, UnmarshalConfig([]byte(doc), ConfigFormatJSON, &cfg))
	assert.Equal(t, "gpt-4", cfg.Model.Name)
	assert.Equal(t, 0.7, cfg.Model.Temperature)
	require.NotNil(t, cfg.Tracing.Enabled)
	assert.False(t, *cfg.Tracing.Enabled)
	assert.Equal(t, "stdout", cfg.Tracing.Exporter)
	assert.Equal(t, 32000, cfg.Compaction.MaxTokens)
}

func TestUnmarshalConfig_YAML(t *testing.T) {
	doc := `
provider:
  name: openai
  api_key: sk-test
  base_url: "https://api.openai.com"
  temperature: 0.5
  max_tokens: 2048
model:
  name: gpt-4
  temperature: 0.7
tools:
  builtin:
    - read
    - write
  registry:
    - ext1
tracing:
  enabled: false
  exporter: jsonl
  level: debug
approval:
  mode: auto
  classifier: llm
compaction:
  strategy: micro_first
  max_tokens: 64000
`
	var cfg Config
	require.NoError(t, UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg))
	assert.Equal(t, "openai", cfg.Provider.Name)
	assert.Equal(t, "sk-test", cfg.Provider.APIKey)
	assert.Equal(t, "https://api.openai.com", cfg.Provider.BaseURL)
	assert.Equal(t, 0.5, cfg.Provider.Temperature)
	assert.Equal(t, 2048, cfg.Provider.MaxTokens)
	assert.Equal(t, "gpt-4", cfg.Model.Name)
	assert.Equal(t, 0.7, cfg.Model.Temperature)
	assert.Equal(t, []string{"read", "write"}, cfg.Tools.Builtin)
	assert.Equal(t, []string{"ext1"}, cfg.Tools.Registry)
	require.NotNil(t, cfg.Tracing.Enabled)
	assert.False(t, *cfg.Tracing.Enabled)
	assert.Equal(t, "jsonl", cfg.Tracing.Exporter)
	assert.Equal(t, "debug", cfg.Tracing.Level)
	assert.Equal(t, "auto", cfg.Approval.Mode)
	assert.Equal(t, "llm", cfg.Approval.Classifier)
	assert.Equal(t, "micro_first", cfg.Compaction.Strategy)
	assert.Equal(t, 64000, cfg.Compaction.MaxTokens)
}

func TestUnmarshalConfig_YAML_CommentsAndQuotes(t *testing.T) {
	doc := `
# top-level comment
model:
  name: "gpt-4"   # inline comment
  max_tokens: 4096
`
	var cfg Config
	require.NoError(t, UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg))
	assert.Equal(t, "gpt-4", cfg.Model.Name)
	assert.Equal(t, 4096, cfg.Model.MaxTokens)
}

func TestUnmarshalConfig_AutoResolvesToJSON(t *testing.T) {
	doc := `{"model":{"name":"auto-json"}}`
	var cfg Config
	require.NoError(t, UnmarshalConfig([]byte(doc), ConfigFormatAuto, &cfg))
	assert.Equal(t, "auto-json", cfg.Model.Name)
}

func TestUnmarshalConfig_UnknownFormat(t *testing.T) {
	var cfg Config
	err := UnmarshalConfig([]byte("x"), ConfigFormat(99), &cfg)
	require.Error(t, err)
}

func TestUnmarshalConfig_YAMLBadIndent(t *testing.T) {
	doc := "model:\n    name: gpt-4\n  bad: x\n"
	var cfg Config
	// Tolerantly the parser only incorrectly parses; it must not panic.
	_ = UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg) //nolint:errcheck // only verifying no panic
}

func TestLoad_AutoDetectYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	doc := "provider:\n  name: anthropic\nmodel:\n  name: claude\ncompaction:\n  max_tokens: 48000\n"
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	cfg, err := NewLoader().WithFile(path).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "anthropic", cfg.Provider.Name)
	assert.Equal(t, "claude", cfg.Model.Name)
	assert.Equal(t, 48000, cfg.Compaction.MaxTokens)
}

func TestLoad_AutoDetectYML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yml")
	doc := "model:\n  name: gpt-3.5\n"
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	cfg, err := NewLoader().WithFile(path).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "gpt-3.5", cfg.Model.Name)
}

func TestLoad_BackwardCompatJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	doc := `{"provider":{"name":"openai"},"model":{"name":"gpt-4"}}`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	cfg, err := NewLoader().WithFile(path).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "openai", cfg.Provider.Name)
	assert.Equal(t, "gpt-4", cfg.Model.Name)
}

func TestLoad_CoexistenceJSONWins(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "config.json")
	doc := `{"model":{"name":"from-json"}}`
	require.NoError(t, os.WriteFile(jsonPath, []byte(doc), 0o600))

	// A sibling .yaml also exists, but the Loader uses only the configured path
	// which is the .json; so JSON wins.
	yamlPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(yamlPath, []byte("model:\n  name: from-yaml\n"), 0o600))

	cfg, err := NewLoader().WithFile(jsonPath).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "from-json", cfg.Model.Name)
}

func TestYAMLConfigLoader(t *testing.T) {
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(yamlPath, []byte("model:\n  name: claude\n"), 0o600))

	cfg, err := NewYAMLConfigLoader().Load(context.Background(), yamlPath)
	require.NoError(t, err)
	assert.Equal(t, "claude", cfg.Model.Name)

	// The loader also supports JSON via the same path-based detection.
	jsonPath := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(jsonPath, []byte(`{"model":{"name":"gpt-4"}}`), 0o600))
	cfg2, err := NewYAMLConfigLoader().Load(context.Background(), jsonPath)
	require.NoError(t, err)
	assert.Equal(t, "gpt-4", cfg2.Model.Name)
}

func TestYAMLConfigLoader_Unsupported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("a=b\n"), 0o600))
	_, err := NewYAMLConfigLoader().Load(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported")
}

func TestLoad_SpanCarriesFormatAttr(t *testing.T) {
	exp := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("trace-format-attr", exp)
	root, ctx := tr.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(yamlPath, []byte("model:\n  name: claude\n"), 0o600))
	_, err := NewLoader().WithFile(yamlPath).Load(ctx)
	require.NoError(t, err)
	root.End()

	require.Eventually(t, func() bool { return exp.SpanCount() >= 3 }, 2_000_000_000, 5_000_000, "expected spans")

	spans := exp.Spans()
	format := ""
	for _, sp := range spans {
		if sp.Name != "config.load" {
			continue
		}
		for _, a := range sp.Attributes {
			if a.Key == "format" {
				format, _ = a.Value.(string) //nolint:errcheck // type assertion ok to fail
			}
		}
	}
	assert.Equal(t, "yaml", format, "config.load span should carry format=yaml")
}

// compile-time guards that must not regress.
var _ = (*YAMLConfigLoader)(nil)
