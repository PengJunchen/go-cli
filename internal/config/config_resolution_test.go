package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Five-layer priority resolution (expanded permutations)
// ---------------------------------------------------------------------------

// TestLoad_Priority_FileWinsDefaults builds a file layer and confirms it
// overrides built-in default values on a per-field basis while untouched
// defaults survive.
func TestLoad_Priority_FileWinsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	doc := `{"provider":{"name":"file","max_tokens":2048},"compaction":{"max_tokens":96000}}`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))

	cfg, err := NewLoader().WithFile(path).Load(context.Background())
	require.NoError(t, err)

	// File overrides the default provider max_tokens.
	assert.Equal(t, 2048, cfg.Provider.MaxTokens)
	// Model default remains untouched by the file.
	assert.Equal(t, defaultMaxTokens, cfg.Model.MaxTokens)
	assert.True(t, *cfg.Tracing.Enabled)
}

// TestLoad_Priority_EnvWinsFile verifies the env layer beats the file layer,
// while fields only set in the file still come through.
func TestLoad_Priority_EnvWinsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	doc := `{"model":{"name":"file-model","max_tokens":1000}}`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	t.Setenv("GO_CLI_MODEL_MAX_TOKENS", "2000")

	cfg, err := NewLoader().WithFile(path).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "file-model", cfg.Model.Name)
	assert.Equal(t, 2000, cfg.Model.MaxTokens)
}

// TestLoad_Priority_FlagWinsEnv verifies the flag layer beats both file and
// env layers.
func TestLoad_Priority_FlagWinsEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	doc := `{"model":{"name":"file","max_tokens":1000}}`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	t.Setenv("GO_CLI_MODEL_MAX_TOKENS", "2000")

	flag := &Config{Model: ModelConfig{Name: "flag-model", MaxTokens: 3000}}
	cfg, err := NewLoader().WithFile(path).WithFlag(flag).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "flag-model", cfg.Model.Name)
	assert.Equal(t, 3000, cfg.Model.MaxTokens)
}

// TestLoad_Priority_OverrideWinsEverything verifies the programmatic override
// layer beats a flag layer of otherwise-equal strength.
func TestLoad_Priority_OverrideWinsEverything(t *testing.T) {
	t.Setenv("GO_CLI_MODEL_NAME", "env-name")

	flag := &Config{Model: ModelConfig{Name: "flag-name", MaxTokens: 3000}}
	override := &Config{Model: ModelConfig{MaxTokens: 4000}}
	cfg, err := NewLoader().WithFlag(flag).WithOverride(override).Load(context.Background())
	require.NoError(t, err)
	// Override only supplies max_tokens; flag supplies name; env name is beaten
	// by flag name.
	assert.Equal(t, "flag-name", cfg.Model.Name)
	assert.Equal(t, 4000, cfg.Model.MaxTokens)
}

// TestLoad_Priority_IndependentFieldsAcrossFourSources spreads each config
// section across a different layer, proving fields resolve independently.
func TestLoad_Priority_IndependentFieldsAcrossFourSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	doc := `{"provider":{"name":"file"},"approval":{"mode":"from-file"}}`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	t.Setenv("GO_CLI_MODEL_NAME", "from-env")

	flag := &Config{Session: SessionConfig{ID: "from-flag"}}
	override := &Config{Tracing: TracingConfig{Level: "error"}}

	cfg, err := NewLoader().
		WithFile(path).
		WithFlag(flag).
		WithOverride(override).
		Load(context.Background())
	require.NoError(t, err)

	assert.Equal(t, "file", cfg.Provider.Name)      // file
	assert.Equal(t, "from-file", cfg.Approval.Mode) // file
	assert.Equal(t, "from-env", cfg.Model.Name)     // env
	assert.Equal(t, "from-flag", cfg.Session.ID)    // flag
	assert.Equal(t, "error", cfg.Tracing.Level)     // override
}

// ---------------------------------------------------------------------------
// Validator boundary conditions
// ---------------------------------------------------------------------------

// TestValidator_TemperatureBoundary verifies the inclusive endpoints 0.0 and
// 2.0 are valid, and just-beyond values fail.
func TestValidator_TemperatureBoundary(t *testing.T) {
	v := NewDefaultValidator()

	cfg := defaultConfig()
	cfg.Provider.Temperature = minTemperature
	cfg.Model.Temperature = maxTemperature
	require.NoError(t, v.Validate(*cfg))

	cfg.Provider.Temperature = maxTemperature + 0.001
	require.Error(t, v.Validate(*cfg))

	cfg.Provider.Temperature = 0
	cfg.Model.Temperature = minTemperature - 0.001
	require.Error(t, v.Validate(*cfg))
}

// TestValidator_NonNegativeTokens verifies negative provider/model max_tokens
// are rejected.
func TestValidator_NonNegativeTokens(t *testing.T) {
	v := NewDefaultValidator()

	cfg := defaultConfig()
	cfg.Provider.MaxTokens = -1
	require.Error(t, v.Validate(*cfg))
	assert.Contains(t, v.Validate(*cfg).Error(), "max_tokens")

	cfg = defaultConfig()
	cfg.Model.MaxTokens = -5
	require.Error(t, v.Validate(*cfg))
}

// TestValidator_CompactionBoundary verifies zero/negative compaction max_tokens
// are rejected while the default positive value passes.
func TestValidator_CompactionBoundary(t *testing.T) {
	v := NewDefaultValidator()
	cfg := defaultConfig()
	cfg.Compaction.MaxTokens = 0
	err := v.Validate(*cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "compaction")

	cfg.Compaction.MaxTokens = 1
	require.NoError(t, v.Validate(*cfg))

	cfg.Compaction.MaxTokens = -10
	require.Error(t, v.Validate(*cfg))
}

// TestValidator_CaseSensitiveTracingValues verifies tracing level/exporter are
// matched case-sensitively so an uppercase value is rejected.
func TestValidator_CaseSensitiveTracingValues(t *testing.T) {
	v := NewDefaultValidator()

	cfg := defaultConfig()
	cfg.Tracing.Level = "DEBUG"
	require.Error(t, v.Validate(*cfg))

	cfg = defaultConfig()
	cfg.Tracing.Exporter = "JSONL"
	require.Error(t, v.Validate(*cfg))
}

// TestValidator_MultipleErrorsCombined verifies several simultaneous violations
// are reported together in a single error string.
func TestValidator_MultipleErrorsCombined(t *testing.T) {
	cfg := &Config{
		Provider:   ProviderConfig{Temperature: 5.0},
		Model:      ModelConfig{Temperature: -2.0},
		Tracing:    TracingConfig{Level: "verbose", Exporter: "otlp"},
		Compaction: CompactionConfig{MaxTokens: 0},
	}
	err := NewDefaultValidator().Validate(*cfg)
	require.Error(t, err)
	for _, fragment := range []string{
		"provider temperature",
		"model temperature",
		"tracing level",
		"tracing exporter",
		"compaction",
	} {
		assert.Contains(t, err.Error(), fragment)
	}
}

// TestValidator_ContainsHelper directly exercises the unexported membership
// helper used by tracing-level/exporter validation.
func TestValidator_ContainsHelper(t *testing.T) {
	assert.True(t, contains(validTracingLevels, "info"))
	assert.True(t, contains(validTracingExporter, "none"))
	assert.False(t, contains(validTracingLevels, "panic"))
	assert.False(t, contains(nil, "x"))
}

// ---------------------------------------------------------------------------
// Load path behaviour
// ---------------------------------------------------------------------------

// TestLoad_ValidationFailureViaLoader verifies a config that fails validation
// propagates the validator error out of Load (rather than a file error).
func TestLoad_ValidationFailureViaLoader(t *testing.T) {
	t.Setenv("GO_CLI_PROVIDER_TEMPERATURE", "9.0")
	_, err := NewLoader().Load(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "temperature")
}

// TestLoad_FlagProvidesRequiredField confirms a flag layer can satisfy the
// validator and avoid a validation error.
func TestLoad_FlagProvidesRequiredField(t *testing.T) {
	// Default config is already valid, but force a positive provider max_tokens
	// through the flag layer to confirm flag values propagate into validation.
	flag := &Config{Provider: ProviderConfig{MaxTokens: 128}}
	cfg, err := NewLoader().WithFlag(flag).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 128, cfg.Provider.MaxTokens)
}

// ---------------------------------------------------------------------------
// Defaults and zero-length inputs
// ---------------------------------------------------------------------------

// TestLoader_ReusesConfigPointer verifies the With* setters all return the same
// loader pointer, enabling fluent chaining.
func TestLoader_ReusesConfigPointer(t *testing.T) {
	l := NewLoader()
	assert.Same(t, l, l.WithFile("a.json"))
	assert.Same(t, l, l.WithEnvPrefix("P"))
	assert.Same(t, l, l.WithFlag(&Config{}))
	assert.Same(t, l, l.WithOverride(&Config{}))
	assert.Same(t, l, l.WithValidator(validatorFunc(func(Config) error { return nil })))
}

// TestLoader_NilValidatorNotRequired verifies a nil-carrying loader still
// defaults validation to the built-in validator via NewLoader.
func TestLoader_PassesValidationAfterDefaults(t *testing.T) {
	cfg, err := NewLoader().Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, defaultCompactionMaxTokens, cfg.Compaction.MaxTokens)
	assert.Equal(t, defaultTracingLevel, cfg.Tracing.Level)
}

// ---------------------------------------------------------------------------
// SettingLayer & Settings helpers (complement edges already covered)
// ---------------------------------------------------------------------------

// TestSettingLayerString verifies the layer identifier string form.
func TestSettingLayerString(t *testing.T) {
	assert.Equal(t, "global", SettingGlobal.String())
	assert.Equal(t, "project", SettingProject.String())
}

// TestDefaultSettings_GlobalOnlyResolvable verifies a key present only in the
// global layer resolves through Get, and List with no layer returns both.
func TestDefaultSettings_GlobalOnlyResolvable(t *testing.T) {
	s := NewDefaultSettings()
	require.NoError(t, s.Set(context.Background(), "g", 1, SettingGlobal))

	v, err := s.Get(context.Background(), "g")
	require.NoError(t, err)
	assert.Equal(t, 1, v)

	all, err := s.List(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, all["g"])
}

// TestDefaultSettings_ListOverridesForExplicitLayerOrder verifies the later
// requested layer wins in the merged list, mirroring Layer.Get priority.
func TestDefaultSettings_ListOverridesForExplicitLayerOrder(t *testing.T) {
	ctx := context.Background()
	s := NewDefaultSettings()
	require.NoError(t, s.Set(ctx, "k", "global", SettingGlobal))
	require.NoError(t, s.Set(ctx, "k", "project", SettingProject))

	merged, err := s.List(ctx, SettingProject, SettingGlobal)
	require.NoError(t, err)
	assert.Equal(t, "global", merged["k"])

	merged2, err := s.List(ctx, SettingGlobal, SettingProject)
	require.NoError(t, err)
	assert.Equal(t, "project", merged2["k"])
}

// TestDefaultSettings_DeleteNonExistentIsIdempotent confirms deleting a key
// that was never set in a layer returns nil cleanly.
func TestDefaultSettings_DeleteNonExistentIsIdempotent(t *testing.T) {
	s := NewDefaultSettings()
	require.NoError(t, s.Delete(context.Background(), "nope", SettingProject))
	require.NoError(t, s.Delete(context.Background(), "nope", SettingGlobal))
}
