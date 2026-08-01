package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestLoad_DefaultMerge(t *testing.T) {
	cfg, err := NewLoader().Load(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.False(t, cfg.Verbose())
	assert.Equal(t, defaultMaxTokens, cfg.Model.MaxTokens)
	assert.Equal(t, defaultTracingLevel, cfg.Tracing.Level)
}

func TestLoad_FromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	doc := `{"model":{"name":"gpt-4","temperature":0.7},"provider":{"name":"openai"},"compaction":{"max_tokens":64000}}`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	cfg, err := NewLoader().WithFile(path).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "gpt-4", cfg.Model.Name)
	assert.Equal(t, 0.7, cfg.Model.Temperature)
	assert.Equal(t, "openai", cfg.Provider.Name)
	assert.Equal(t, 64000, cfg.Compaction.MaxTokens)
}

func TestLoad_FromFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.json")
	_, err := NewLoader().WithFile(path).Load(context.Background())
	require.Error(t, err)
}

func TestLoad_FromEnv(t *testing.T) {
	t.Setenv("GO_CLI_PROVIDER_NAME", "anthropic")
	t.Setenv("GO_CLI_PROVIDER_TEMPERATURE", "0.5")
	t.Setenv("GO_CLI_MODEL_MAX_TOKENS", "8000")
	t.Setenv("GO_CLI_TRACING_EXPORTER", "stdout")

	cfg, err := NewLoader().Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "anthropic", cfg.Provider.Name)
	assert.Equal(t, 0.5, cfg.Provider.Temperature)
	assert.Equal(t, 8000, cfg.Model.MaxTokens)
	assert.Equal(t, "stdout", cfg.Tracing.Exporter)
}

func TestLoad_FiveLevelPriority(t *testing.T) {
	// File layer.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	doc := `{"provider":{"name":"file-provider","temperature":0.1},"model":{"name":"file-model"}}`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	// Env layer overrides provider.name and model.max_tokens.
	t.Setenv("GO_CLI_PROVIDER_NAME", "env-provider")
	t.Setenv("GO_CLI_MODEL_MAX_TOKENS", "8000")

	flag := &Config{Provider: ProviderConfig{Temperature: 0.5, BaseURL: "flag-url"}, Model: ModelConfig{MaxTokens: 9000}}
	override := &Config{Provider: ProviderConfig{Temperature: 1.0}, Model: ModelConfig{MaxTokens: 9500}}

	cfg, err := NewLoader().
		WithFile(path).
		WithFlag(flag).
		WithOverride(override).
		Load(context.Background())
	require.NoError(t, err)

	// Override beats flag for temperature.
	assert.Equal(t, 1.0, cfg.Provider.Temperature)
	// Override beats flag beats env for model.max_tokens.
	assert.Equal(t, 9500, cfg.Model.MaxTokens)
	// Env beats file for provider.name (flag/override do not set it).
	assert.Equal(t, "env-provider", cfg.Provider.Name)
	// File provides model.name (nothing higher sets it).
	assert.Equal(t, "file-model", cfg.Model.Name)
	// Flag provides base_url (override does not set it).
	assert.Equal(t, "flag-url", cfg.Provider.BaseURL)
}

func TestLoad_OverrideDisablesTracing(t *testing.T) {
	// A nil-by-default Enabled allows an override to explicitly disable it.
	off := false
	override := &Config{Tracing: TracingConfig{Enabled: &off}}
	cfg, err := NewLoader().WithOverride(override).Load(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg.Tracing.Enabled)
	assert.False(t, *cfg.Tracing.Enabled)
}

func TestLoad_FlagVerboseWinsOverride(t *testing.T) {
	flag := &Config{verbose: true}
	cfg, err := NewLoader().WithFlag(flag).Load(context.Background())
	require.NoError(t, err)
	assert.True(t, cfg.Verbose())
	// Override still wins if set.
	cfg2, err := NewLoader().WithFlag(flag).WithOverride(&Config{}).Load(context.Background())
	require.NoError(t, err)
	assert.True(t, cfg2.Verbose())
}

func TestExpandEnv_Braces(t *testing.T) {
	t.Setenv("FOO", "bar")
	assert.Equal(t, "hello bar", ExpandEnv("hello ${FOO}"))
}

func TestExpandEnv_Dollar(t *testing.T) {
	t.Setenv("FOO", "bar")
	assert.Equal(t, "bar-x", ExpandEnv("$FOO-x"))
}

func TestExpandEnv_Recursive(t *testing.T) {
	t.Setenv("INNER", "deep")
	t.Setenv("OUTER", "${INNER}")
	assert.Equal(t, "deep", ExpandEnv("${OUTER}"))
}

func TestExpandEnv_Unset(t *testing.T) {
	assert.Equal(t, "x", ExpandEnv("x${GO_CLI_UNSET_VAR_12345}"))
}

func TestLoad_TracingSpans(t *testing.T) {
	exp := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("test-trace-id", exp)
	root, ctx := tr.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"model":{"name":"gpt-4"}}`), 0o600))

	_, err := NewLoader().WithFile(path).Load(ctx)
	require.NoError(t, err)
	root.End()

	require.Eventually(t, func() bool {
		return exp.SpanCount() >= 4
	}, 2*time.Second, 5*time.Millisecond, "expected config.load spans to be exported")

	exp.AssertSpanExists(t, "config.load")
	exp.AssertSpanExists(t, "config.load.file")
	exp.AssertSpanExists(t, "config.merged")
	exp.AssertSpanChain(t)
}
