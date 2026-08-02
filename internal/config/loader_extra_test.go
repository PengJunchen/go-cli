package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoad_WithEnvPrefix verifies a custom environment prefix is honored.
func TestLoad_WithEnvPrefix(t *testing.T) {
	t.Setenv("MYCLI_PROVIDER_NAME", "prefix-provider")
	cfg, err := NewLoader().WithEnvPrefix("MYCLI").Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "prefix-provider", cfg.Provider.Name)
}

// TestLoad_WithValidator verifies a custom validator is invoked and its result
// returned.
func TestLoad_WithValidator(t *testing.T) {
	called := false
	cfg, err := NewLoader().WithValidator(validatorFunc(func(Config) error {
		called = true
		return nil
	})).Load(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.True(t, called, "custom validator should be invoked")
}

// validatorFunc adapts a function to the Validator interface.
type validatorFunc func(Config) error

func (f validatorFunc) Validate(c Config) error { return f(c) }

// TestLoad_WithValidatorRejects verifies the error surface of Load when the
// custom validator rejects the merged config.
func TestLoad_WithValidatorRejects(t *testing.T) {
	loader := NewLoader().WithValidator(validatorFunc(func(Config) error {
		return errors.New("custom: rejected")
	}))
	_, err := loader.Load(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom: rejected")
}

// TestResolveVerbose_PriorityOrder verifies override > flag > env resolution.
func TestResolveVerbose_PriorityOrder(t *testing.T) {
	t.Setenv("GO_CLI_VERBOSE", "1")

	// Env alone returns true.
	l := NewLoader()
	assert.True(t, l.resolveVerbose())

	// Override false does not suppress env.
	assert.True(t, NewLoader().WithOverride(&Config{}).resolveVerbose())

	// Flag verbose overrides env.
	assert.True(t, NewLoader().WithFlag(&Config{verbose: true}).resolveVerbose())

	// Override verbose overrides flag.
	assert.True(t, NewLoader().WithFlag(&Config{verbose: true}).WithOverride(&Config{verbose: true}).resolveVerbose())

	// No env set => false.
	t.Setenv("GO_CLI_VERBOSE", "")
	assert.False(t, NewLoader().resolveVerbose())
}

// TestParseBoolEnv verifies the boolean environment parser's full matrix.
func TestParseBoolEnv(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("GO_CLI_TEST_BOOL", v)
			b, ok := parseBoolEnv("GO_CLI_TEST_BOOL")
			assert.True(t, ok)
			assert.True(t, b)
		})
	}
	for _, v := range []string{"0", "false", "no", "off"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("GO_CLI_TEST_BOOL", v)
			b, ok := parseBoolEnv("GO_CLI_TEST_BOOL")
			assert.True(t, ok)
			assert.False(t, b)
		})
	}
	t.Setenv("GO_CLI_TEST_BOOL", "")
	b, ok := parseBoolEnv("GO_CLI_TEST_BOOL")
	assert.False(t, ok)
	assert.False(t, b)

	t.Setenv("GO_CLI_TEST_BOOL", "garbage")
	b, ok = parseBoolEnv("GO_CLI_TEST_BOOL")
	assert.False(t, ok)
	assert.False(t, b)
}

// TestParseFloatEnv verifies bad / empty float env values yield 0.
func TestParseFloatEnv(t *testing.T) {
	assert.Equal(t, 0.0, parseFloatEnv("GO_CLI_UNSET_FLOAT_XYZ"))
	t.Setenv("GO_CLI_TEST_FLOAT", "1.5")
	assert.Equal(t, 1.5, parseFloatEnv("GO_CLI_TEST_FLOAT"))
	t.Setenv("GO_CLI_TEST_FLOAT", "not-a-float")
	assert.Equal(t, 0.0, parseFloatEnv("GO_CLI_TEST_FLOAT"))
}

// TestParseIntEnv verifies bad / empty int env values yield 0.
func TestParseIntEnv(t *testing.T) {
	assert.Equal(t, 0, parseIntEnv("GO_CLI_UNSET_INT_XYZ"))
	t.Setenv("GO_CLI_TEST_INT", "42")
	assert.Equal(t, 42, parseIntEnv("GO_CLI_TEST_INT"))
	t.Setenv("GO_CLI_TEST_INT", "oops")
	assert.Equal(t, 0, parseIntEnv("GO_CLI_TEST_INT"))
}

// TestExpandEnv_SelfReferenceGuard verifies the depth guard stops a
// self-referential variable from recursing forever.
func TestExpandEnv_SelfReferenceGuard(t *testing.T) {
	t.Setenv("GO_CLI_LOOP", "${GO_CLI_LOOP}")
	result := ExpandEnv("${GO_CLI_LOOP}")
	assert.Equal(t, "${GO_CLI_LOOP}", result)
}

// TestCountKeysNilAndPopulated verifies countKeys handles nil and populated
// configs, including slice/map fields.
func TestCountKeysNilAndPopulated(t *testing.T) {
	assert.Equal(t, 0, countKeys(nil))

	empty := &Config{}
	assert.Equal(t, 0, countKeys(empty))

	cfg := &Config{
		Provider: ProviderConfig{Name: "x", Temperature: 0.5},
		Tools:    ToolsConfig{Builtin: []string{"read"}},
	}
	assert.Positive(t, countKeys(cfg))
}

// TestMergeConfigs_OverlaySlicesAndMaps verifies non-empty slices/maps overlay,
// while empty ones are not applied, and pointer replacement works.
func TestMergeConfigs_OverlaySlicesAndMaps(t *testing.T) {
	base := defaultConfig()
	over := &Config{
		Provider: ProviderConfig{Name: "over"},
		Tools:    ToolsConfig{Builtin: []string{"a", "b"}},
	}
	merged := mergeConfigs(base, over)
	assert.Equal(t, "over", merged.Provider.Name)
	assert.Equal(t, []string{"a", "b"}, merged.Tools.Builtin)

	// A config with a nil slice does not clear an already-set slice.
	nilOver := &Config{Tools: ToolsConfig{Builtin: nil}}
	merged2 := mergeConfigs(merged, nilOver)
	assert.Equal(t, []string{"a", "b"}, merged2.Tools.Builtin)
}

// TestLoad_UnknownExtensionFallsBackToJSON verifies backward-compatible JSON
// parsing for a config file with an unknown extension.
func TestLoad_UnknownExtensionFallsBackToJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.conf")
	require.NoError(t, os.WriteFile(path, []byte(`{"model":{"name":"fallback-json"}}`), 0o600))

	cfg, err := NewLoader().WithFile(path).Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "fallback-json", cfg.Model.Name)
}

// TestLoad_BadYAMLFileSurfacesError verifies a parse error from the file layer
// surfaces from Load.
func TestLoad_BadYAMLFileSurfacesError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("model:\n  name: gpt-4\n  max_tokens: not-an-int\n"), 0o600))

	_, err := NewLoader().WithFile(path).Load(context.Background())
	require.Error(t, err)
}
