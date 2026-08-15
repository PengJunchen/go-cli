package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Tab indentation support (38-12-1)
// ---------------------------------------------------------------------------

// TestYAMLTabIndent verifies that tab-indented YAML parses correctly, with a
// tab counting as 4 spaces (tab-stop rounding).
func TestYAMLTabIndent(t *testing.T) {
	// Tab-indented YAML should parse correctly (tab = 4 spaces)
	doc := "model:\n\tname: gpt-4\n\tmax_tokens: 4096\n"
	var cfg Config
	require.NoError(t, UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg))
	assert.Equal(t, "gpt-4", cfg.Model.Name)
	assert.Equal(t, 4096, cfg.Model.MaxTokens)

	// Tab-indented nested mapping in a different section
	doc2 := "provider:\n\tname: openai\n\tapi_key: sk-test\n"
	var cfg2 Config
	require.NoError(t, UnmarshalConfig([]byte(doc2), ConfigFormatYAML, &cfg2))
	assert.Equal(t, "openai", cfg2.Provider.Name)
	assert.Equal(t, "sk-test", cfg2.Provider.APIKey)

	// indentWidth directly: tab = 4 spaces, snaps to next multiple of 4
	assert.Equal(t, 4, indentWidth("\thello"))
	assert.Equal(t, 8, indentWidth("\t\thello"))
	// 2 spaces (n=2), tab snaps to 4, 2 more spaces → 6
	assert.Equal(t, 6, indentWidth("  \t  hello"))
	assert.Equal(t, 0, indentWidth("hello"))
}

// ---------------------------------------------------------------------------
// Flow map support (38-12-2)
// ---------------------------------------------------------------------------

// TestYAMLFlowMap verifies that YAML flow maps ({key: value, ...}) parse into
// map[string]any, including nested flow maps, empty flow maps, and quoted keys.
func TestYAMLFlowMap(t *testing.T) {
	// Basic flow map assigned to a Config struct
	doc := "model: {name: gpt-4, max_tokens: 4096}\n"
	var cfg Config
	require.NoError(t, UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg))
	assert.Equal(t, "gpt-4", cfg.Model.Name)
	assert.Equal(t, 4096, cfg.Model.MaxTokens)

	// Nested flow map
	tree, err := parseYAMLTree([]byte("outer: {inner: {deep: value}}\n"))
	require.NoError(t, err)
	m := tree.(map[string]any)
	outer := m["outer"].(map[string]any)
	inner := outer["inner"].(map[string]any)
	assert.Equal(t, "value", inner["deep"])

	// Empty flow map
	tree2, err := parseYAMLTree([]byte("data: {}\n"))
	require.NoError(t, err)
	m2 := tree2.(map[string]any)
	emptyMap, ok := m2["data"].(map[string]any)
	require.True(t, ok)
	assert.Empty(t, emptyMap)

	// Flow map with quoted values
	tree3, err := parseYAMLTree([]byte(`config: {"key": "value", num: 42}`))
	require.NoError(t, err)
	m3 := tree3.(map[string]any)
	cfg3 := m3["config"].(map[string]any)
	assert.Equal(t, "value", cfg3["key"])
	assert.Equal(t, int64(42), cfg3["num"])
}

// ---------------------------------------------------------------------------
// Block scalar support (38-12-3)
// ---------------------------------------------------------------------------

// TestYAMLBlockScalar verifies that block scalars (| literal and > folded) are
// parsed correctly, preserving newlines for | and folding for >.
func TestYAMLBlockScalar(t *testing.T) {
	// Literal block scalar (|): preserves newlines
	tree, err := parseYAMLTree([]byte("prompt: |\n  Hello\n  World\n"))
	require.NoError(t, err)
	m := tree.(map[string]any)
	prompt, ok := m["prompt"].(string)
	require.True(t, ok)
	assert.Contains(t, prompt, "Hello\nWorld")

	// Folded block scalar (>): folds newlines into spaces
	tree2, err := parseYAMLTree([]byte("description: >\n  This is\n  a folded\n  paragraph.\n"))
	require.NoError(t, err)
	m2 := tree2.(map[string]any)
	desc, ok := m2["description"].(string)
	require.True(t, ok)
	assert.Contains(t, desc, "This is a folded paragraph.")

	// Literal block scalar preserves content exactly
	tree3, err := parseYAMLTree([]byte("text: |\n  Line 1\n  Line 2\n"))
	require.NoError(t, err)
	m3 := tree3.(map[string]any)
	text, ok := m3["text"].(string)
	require.True(t, ok)
	assert.Contains(t, text, "Line 1\nLine 2")

	// Block scalar with deeper indentation
	tree4, err := parseYAMLTree([]byte("data: |\n    Indented\n    Content\n"))
	require.NoError(t, err)
	m4 := tree4.(map[string]any)
	data, ok := m4["data"].(string)
	require.True(t, ok)
	assert.Contains(t, data, "Indented")
	assert.Contains(t, data, "Content")

	// Strip chomping indicator |- removes trailing newline
	tree5, err := parseYAMLTree([]byte("strip: |-\n  Alpha\n  Beta\n"))
	require.NoError(t, err)
	m5 := tree5.(map[string]any)
	strip, ok := m5["strip"].(string)
	require.True(t, ok)
	assert.Equal(t, "Alpha\nBeta", strip) // no trailing newline

	// Folded strip chomping >- removes trailing newline
	tree6, err := parseYAMLTree([]byte("folded: >-\n  Alpha\n  Beta\n"))
	require.NoError(t, err)
	m6 := tree6.(map[string]any)
	folded, ok := m6["folded"].(string)
	require.True(t, ok)
	assert.Equal(t, "Alpha Beta", folded) // folded + no trailing newline

	// Block scalar followed by a sibling key at the same indent must not
	// consume the sibling (regression test for parentIndent guard).
	tree7, err := parseYAMLTree([]byte("text: |\n  Hello\nsame: value\n"))
	require.NoError(t, err)
	m7 := tree7.(map[string]any)
	blockText, ok := m7["text"].(string)
	require.True(t, ok)
	assert.Contains(t, blockText, "Hello")
	val, ok := m7["same"]
	require.True(t, ok, "sibling key 'same' must not be consumed by block scalar")
	assert.Equal(t, "value", val)

	// Empty block scalar (no deeper lines) returns empty string, sibling preserved
	tree8, err := parseYAMLTree([]byte("text: |\nnext: ok\n"))
	require.NoError(t, err)
	m8 := tree8.(map[string]any)
	emptyText, ok := m8["text"].(string)
	require.True(t, ok)
	assert.Empty(t, emptyText)
	assert.Equal(t, "ok", m8["next"])
}

// ---------------------------------------------------------------------------
// HI-12: Deep nesting and large input hardening (task 48-14)
// ---------------------------------------------------------------------------

// TestYAMLFlowMap_DeepNesting100Levels verifies that a flow map nested 100
// levels deep does not crash or stack-overflow (AC-3).
func TestYAMLFlowMap_DeepNesting100Levels(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("key: ")
	for i := 0; i < 100; i++ {
		sb.WriteString("{a: ")
	}
	sb.WriteString("value")
	for i := 0; i < 100; i++ {
		sb.WriteString("}")
	}
	tree, err := parseYAMLTree([]byte(sb.String()))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	// Walk to the innermost value.
	inner := m["key"]
	for i := 0; i < 100; i++ {
		mp, ok := inner.(map[string]any)
		require.True(t, ok, "level %d", i)
		inner = mp["a"]
	}
	assert.Equal(t, "value", inner)
}

// TestYAMLFlowMap_DeepNestingExceedsLimit verifies that flow map nesting
// beyond the max depth returns an error rather than crashing.
func TestYAMLFlowMap_DeepNestingExceedsLimit(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("key: ")
	for i := 0; i < 102; i++ {
		sb.WriteString("{a: ")
	}
	sb.WriteString("value")
	for i := 0; i < 102; i++ {
		sb.WriteString("}")
	}
	_, err := parseYAMLTree([]byte(sb.String()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too deep")
}

// TestYAML_VeryLongValue verifies that a value exceeding 1MB does not crash
// the parser.
func TestYAML_VeryLongValue(t *testing.T) {
	longVal := strings.Repeat("x", 1<<20) // 1 MiB
	doc := "key: " + longVal + "\n"
	tree, err := parseYAMLTree([]byte(doc))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, longVal, m["key"])
}
