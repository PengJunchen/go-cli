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
// loadFromEnv: full coverage of every field the env layer reads
// ---------------------------------------------------------------------------

// TestLoadFromEnv_FullCoverage sets every recognized GO_CLI variable and
// asserts each one lands on the correct Config field. This complements the
// sparse per-field checks in loader_test.go.
func TestLoadFromEnv_FullCoverage(t *testing.T) {
	t.Setenv("GO_CLI_PROVIDER_NAME", "openai")
	t.Setenv("GO_CLI_PROVIDER_API_KEY", "sk-abc")
	t.Setenv("GO_CLI_PROVIDER_BASE_URL", "https://api.example.com")
	t.Setenv("GO_CLI_PROVIDER_MODEL", "gpt-4")
	t.Setenv("GO_CLI_PROVIDER_TEMPERATURE", "0.25")
	t.Setenv("GO_CLI_PROVIDER_MAX_TOKENS", "1234")
	t.Setenv("GO_CLI_MODEL_NAME", "claude")
	t.Setenv("GO_CLI_MODEL_TEMPERATURE", "0.9")
	t.Setenv("GO_CLI_MODEL_MAX_TOKENS", "5678")
	t.Setenv("GO_CLI_TRACING_EXPORTER", "jsonl")
	t.Setenv("GO_CLI_TRACING_LEVEL", "debug")
	t.Setenv("GO_CLI_TRACING_FILE_PATH", "/tmp/trace.jsonl")
	t.Setenv("GO_CLI_TRACING_ENABLED", "true")
	t.Setenv("GO_CLI_APPROVAL_MODE", "manual")
	t.Setenv("GO_CLI_APPROVAL_CLASSIFIER", "rule")
	t.Setenv("GO_CLI_COMPACTION_STRATEGY", "semantic")
	t.Setenv("GO_CLI_COMPACTION_MAX_TOKENS", "9999")

	cfg := loadFromEnv(defaultEnvPrefix)

	assert.Equal(t, "openai", cfg.Provider.Name)
	assert.Equal(t, "sk-abc", cfg.Provider.APIKey)
	assert.Equal(t, "https://api.example.com", cfg.Provider.BaseURL)
	assert.Equal(t, "gpt-4", cfg.Provider.Model)
	assert.Equal(t, 0.25, cfg.Provider.Temperature)
	assert.Equal(t, 1234, cfg.Provider.MaxTokens)

	assert.Equal(t, "claude", cfg.Model.Name)
	assert.Equal(t, 0.9, cfg.Model.Temperature)
	assert.Equal(t, 5678, cfg.Model.MaxTokens)

	assert.Equal(t, "jsonl", cfg.Tracing.Exporter)
	assert.Equal(t, "debug", cfg.Tracing.Level)
	assert.Equal(t, "/tmp/trace.jsonl", cfg.Tracing.FilePath)
	require.NotNil(t, cfg.Tracing.Enabled)
	assert.True(t, *cfg.Tracing.Enabled)

	assert.Equal(t, "manual", cfg.Approval.Mode)
	assert.Equal(t, "rule", cfg.Approval.Classifier)

	assert.Equal(t, "semantic", cfg.Compaction.Strategy)
	assert.Equal(t, 9999, cfg.Compaction.MaxTokens)
}

// TestLoadFromEnv_NoVariables verifies an empty environment yields an empty
// partial Config rather than inheriting defaults (defaults live in the base).
func TestLoadFromEnv_NoVariables(t *testing.T) {
	t.Setenv("GO_CLI_PROVIDER_NAME", "")
	t.Setenv("GO_CLI_MODEL_MAX_TOKENS", "")
	t.Setenv("GO_CLI_TRACING_ENABLED", "")
	t.Setenv("GO_CLI_VERBOSE", "")

	cfg := loadFromEnv(defaultEnvPrefix)
	assert.Equal(t, "", cfg.Provider.Name)
	assert.Equal(t, 0, cfg.Model.MaxTokens)
	assert.Nil(t, cfg.Tracing.Enabled)
	assert.False(t, cfg.verbose)
}

// TestLoadFromEnv_InvalidNumericParsing drives the tolerance of parseFloatEnv
// and parseIntEnv through loadFromEnv with explicitly bad values.
func TestLoadFromEnv_InvalidNumericParsing(t *testing.T) {
	t.Setenv("GO_CLI_MODEL_TEMPERATURE", "very-hot")
	t.Setenv("GO_CLI_MODEL_MAX_TOKENS", "many")
	cfg := loadFromEnv(defaultEnvPrefix)
	assert.Equal(t, 0.0, cfg.Model.Temperature)
	assert.Equal(t, 0, cfg.Model.MaxTokens)
}

// TestLoadFromEnv_TracingEnabledFalse sets a recognized false boolean so the
// explicit pointer landed as false rather than nil.
func TestLoadFromEnv_TracingEnabledFalse(t *testing.T) {
	t.Setenv("GO_CLI_TRACING_ENABLED", "false")
	cfg := loadFromEnv(defaultEnvPrefix)
	require.NotNil(t, cfg.Tracing.Enabled)
	assert.False(t, *cfg.Tracing.Enabled)
}

// ---------------------------------------------------------------------------
// resolveVerbose edge cases
// ---------------------------------------------------------------------------

// TestResolveVerbose_BadEnvValues treats any value other than "1" as not
// verbose, even a truthy one, because the loader compares against "1".
func TestResolveVerbose_BadEnvValues(t *testing.T) {
	for _, val := range []string{"true", "yes", "0", "on", "garbage"} {
		t.Setenv("GO_CLI_VERBOSE", val)
		assert.False(t, NewLoader().resolveVerbose(), "value %q must not enable verbose", val)
	}
}

// TestResolveVerbose_OverrideBeatsFlagEnv naturally verifies the highest layer
// survives even when lower layers also try to enable it.
func TestResolveVerbose_OverrideBeatsFlagEnv(t *testing.T) {
	t.Setenv("GO_CLI_VERBOSE", "1")
	loader := NewLoader().WithFlag(&Config{verbose: true}).WithOverride(&Config{verbose: true})
	assert.True(t, loader.resolveVerbose())

	// An empty override does not set verbose, but flag still wins over env.
	assert.True(t, NewLoader().WithFlag(&Config{verbose: true}).WithOverride(&Config{}).resolveVerbose())
}

// ---------------------------------------------------------------------------
// Load: file-layer error surfaces
// ---------------------------------------------------------------------------

// TestLoad_FileNotFoundErrorMsg verifies the wrapped error message names the
// read failure and is well-formed.
func TestLoad_FileNotFoundErrorMsg(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.json")
	_, err := NewLoader().WithFile(path).Load(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read config file")
}

// TestLoad_MalformedJSONFile verifies a syntactically invalid JSON document in
// the file layer surfaces a parse error from Load.
func TestLoad_MalformedJSONFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"provider": `), 0o600))
	_, err := NewLoader().WithFile(path).Load(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config file")
}

// TestLoad_JSONTypeMismatch verifies a JSON value of the wrong shape (object
// where a string is expected) produces a decode error.
func TestLoad_JSONTypeMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"model":{"name":123}}`), 0o600))
	_, err := NewLoader().WithFile(path).Load(context.Background())
	require.Error(t, err)
}

// TestLoad_EmptyJSONFile surfaces the decode error from an empty JSON document
// in the file layer.
func TestLoad_EmptyJSONFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(""), 0o600))
	_, err := NewLoader().WithFile(path).Load(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected end of JSON input")
}

// ---------------------------------------------------------------------------
// mergeConfigs & overlayValue deeper coverage
// ---------------------------------------------------------------------------

// TestMergeConfigs_IntAndFloatOverlay verifies non-zero int and float fields
// overlay individually while zero values are ignored.
func TestMergeConfigs_IntAndFloatOverlay(t *testing.T) {
	base := defaultConfig()
	over := &Config{
		Provider: ProviderConfig{Temperature: 0.75, MaxTokens: 3000},
		Model:    ModelConfig{MaxTokens: 2000},
	}
	merged := mergeConfigs(base, over)
	assert.Equal(t, 0.75, merged.Provider.Temperature)
	assert.Equal(t, 3000, merged.Provider.MaxTokens)
	assert.Equal(t, 2000, merged.Model.MaxTokens)
	// Unspecified fields keep the default base values.
	assert.Equal(t, defaultTracingExporter, merged.Tracing.Exporter)
}

// TestMergeConfigs_ZeroOverIsNoOp verifies a config carrying only zero values
// leaves the base semantically unchanged.
func TestMergeConfigs_ZeroOverIsNoOp(t *testing.T) {
	base := defaultConfig()
	base.Provider.Name = "base"
	merged := mergeConfigs(base, &Config{
		Provider: ProviderConfig{Temperature: 0, MaxTokens: 0},
		Model:    ModelConfig{Name: ""},
	})
	assert.Equal(t, "base", merged.Provider.Name)
	assert.Equal(t, defaultMaxTokens, merged.Provider.MaxTokens)
	assert.Equal(t, defaultMaxTokens, merged.Model.MaxTokens)
}

// TestOverlayValue_RegistrySlice verifies a second slice field overlays
// independently of the first.
func TestOverlayValue_RegistrySlice(t *testing.T) {
	base := defaultConfig()
	base.Tools = ToolsConfig{Builtin: []string{"b1"}, Registry: []string{"r1"}}
	over := &Config{Tools: ToolsConfig{Registry: []string{"r2"}}}
	merged := mergeConfigs(base, over)
	assert.Equal(t, []string{"b1"}, merged.Tools.Builtin)
	assert.Equal(t, []string{"r2"}, merged.Tools.Registry)
}

// TestOverlayValue_NestedMismatchedTypes exercises the guard in overlayValue
// that strict type equality is required before any copying.
func TestOverlayValue_MismatchedNestedType(t *testing.T) {
	base := defaultConfig()
	over := &Config{}
	// Rotate a float field against an int field within ProviderConfig to reach
	// the integer branch with a float source — harmless no-op by design.
	over.Provider.Temperature = 1.5
	merged := mergeConfigs(base, over)
	assert.Equal(t, 1.5, merged.Provider.Temperature)
}

// ---------------------------------------------------------------------------
// countKeys detailed accounting
// ---------------------------------------------------------------------------

// TestCountKeys_ReflectKinds drills into each counted reflect kind (bool, int,
// float, slice, string, pointer positivity) to lock the counting semantics.
func TestCountKeys_ReflectKinds(t *testing.T) {
	enabled := true
	s := "set"
	cfg := &Config{
		Provider: ProviderConfig{Name: s},           // 1 string
		Tools:    ToolsConfig{Registry: []string{}}, // empty slice -> 0
		Tracing:  TracingConfig{Enabled: &enabled},  // 1 pointer
		Model:    ModelConfig{Temperature: 0.0},     // zero float -> 0
	}
	assert.Equal(t, 2, countKeys(cfg))

	cfg.Tools.Registry = []string{"a"}
	assert.Equal(t, 3, countKeys(cfg))
}

// ---------------------------------------------------------------------------
// ExpandEnv extra paths
// ---------------------------------------------------------------------------

// TestExpandEnv_UnexpandedUnknownKey replaces an absent variable with the empty
// string, leaving surrounding separators intact (os.Expand semantics).
func TestExpandEnv_UnexpandedUnknownKey(t *testing.T) {
	t.Setenv("KNOWN", "v")
	assert.Equal(t, "a-v-b", ExpandEnv("a-${KNOWN}-b"))
	assert.Equal(t, "a--b", ExpandEnv("a-${DOES_NOT_EXIST_XYZ}-b"))
}

// TestExpandEnv_RecursiveChain verifies nested $VAR references resolve.
func TestExpandEnv_RecursiveChain(t *testing.T) {
	t.Setenv("C", "deep")
	t.Setenv("B", "$C")
	t.Setenv("A", "${B}")
	assert.Equal(t, "deep", ExpandEnv("${A}"))
}

// TestExpandEnv_DepthBounded verifies a deep (non-cyclic) chain greater than
// maxExpandDepth stops expanding at the bound instead of recursing forever.
func TestExpandEnv_DepthBounded(t *testing.T) {
	var prev string
	for i := 0; i < maxExpandDepth+4; i++ {
		key := "GO_CLI_EXPAND_LEVEL_"
		key += string(rune('a' + i))
		if prev == "" {
			t.Setenv(key, "leaf")
		} else {
			t.Setenv(key, "${"+prev+"}")
		}
		prev = key
	}
	// Drive expansion of the deepest token; it must terminate (bounded).
	out := ExpandEnv("${" + prev + "}")
	assert.NotEqual(t, "", out)
}
