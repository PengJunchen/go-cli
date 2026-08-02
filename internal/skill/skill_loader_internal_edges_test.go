package skill

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseScalar preserves internal whitespace while stripping one layer of outer
// double or single quotes and surrounding padding.
func TestParseScalarInnerSpaceAndPadding(t *testing.T) {
	assert.Equal(t, "foo  bar", parseScalar("  foo  bar  "))
	assert.Equal(t, "a  b", parseScalar(`"a  b"`))
	assert.Equal(t, "trimmed", parseScalar("   trimmed"))
	assert.Equal(t, "", parseScalar(`""`))
	assert.Equal(t, "x", parseScalar(`'x'`))
	// A single trailing quote is not a matched pair, so it stays.
	assert.Equal(t, `a"`, parseScalar(`a"`))
}

// coerceParamValue handles negative numbers, floating point exponents, and
// numbers that overflow int (falling back to float64).
func TestCoerceParamValueNumericEdges(t *testing.T) {
	assert.Equal(t, -7, coerceParamValue("-7"))
	assert.Equal(t, 0, coerceParamValue("0"))
	assert.Equal(t, 1e3, coerceParamValue("1e3"))
	assert.Equal(t, -2.5, coerceParamValue("-2.5"))
	// 1e30 exceeds an int64/int32; Atoi fails first then ParseFloat wins.
	assert.Equal(t, 1e30, coerceParamValue("1e30"))
	// A lone sign is not a number and stays a string.
	assert.Equal(t, "-", coerceParamValue("-"))
}

// coerceParamValue treats "TRUE"/"False" (mixed case) as plain strings, only
// the exact lowercase tokens are booleans.
func TestCoerceParamValueBoolCaseSensitive(t *testing.T) {
	assert.Equal(t, "TRUE", coerceParamValue("TRUE"))
	assert.Equal(t, "False", coerceParamValue("False"))
	assert.Equal(t, true, coerceParamValue("true"))
	assert.Equal(t, false, coerceParamValue("false"))
}

// splitKeyValue tolerates a colon inside the value while keying on the first
// colon and handles the edge of a key followed immediately by a value.
func TestSplitKeyValueColonInsideValue(t *testing.T) {
	key, rest, ok := splitKeyValue("url: https://example.com:8080/path")
	assert.True(t, ok)
	assert.Equal(t, "url", key)
	assert.Equal(t, " https://example.com:8080/path", rest)

	key, rest, ok = splitKeyValue("prompt: |")
	assert.True(t, ok)
	assert.Equal(t, "prompt", key)
	assert.Equal(t, " |", rest)

	// A whitespace-only key is rejected.
	_, _, ok = splitKeyValue("   : value")
	assert.False(t, ok)
}

// stripIndent removes only a single indentation unit: two spaces or one tab.
func TestStripIndentExactlyOneLevel(t *testing.T) {
	assert.Equal(t, "ab", stripIndent("  ab"))
	assert.Equal(t, "ab", stripIndent("\tab"))
	// A single leading space is not a recognized indent unit and is kept.
	assert.Equal(t, " ab", stripIndent(" ab"))
	// Deeper indentation only drops one level.
	assert.Equal(t, "  ab", stripIndent("    ab"))
	assert.Equal(t, "", stripIndent("  "))
}

// matchScore treats an empty query as a degenerate prefix match on any name.
func TestMatchScoreEmptyQueryIsPrefix(t *testing.T) {
	def := &DefaultSkillDefinition{name: "anything"}
	assert.Equal(t, 4, matchScore(def, ""))
}

// matchScore trims case via strings.ToLower on both sides of the comparison.
func TestMatchScoreIsCaseInsensitive(t *testing.T) {
	def := &DefaultSkillDefinition{name: "DEPLOY", description: "PUSH SHIPPING", triggerHint: "GO LIVE"}
	assert.Equal(t, 5, matchScore(def, "deploy"))
	assert.Equal(t, 3, matchScore(def, "ploy"))
	assert.Equal(t, 2, matchScore(def, "shipping"))
	assert.Equal(t, 2, matchScore(def, "go live"))
	assert.Equal(t, 0, matchScore(def, "nothing"))
}

// parseFrontmatter only treats the first closing delimiter as the end of the
// frontmatter; a later `---` inside the body does not split the payload.
func TestParseFrontmatterFirstClosingDelimiterWins(t *testing.T) {
	def, err := parseFrontmatter([]string{
		"---",
		"name: x",
		"---",
		"body line",
		"---",
		"trailing treated as body too",
	})
	require.NoError(t, err)
	assert.Equal(t, "body line\n---\ntrailing treated as body too", def.prompt)
}

// parseFrontmatterBlock coerces typed parameter values declared as indented
// key: value lines, including negatives and quoted strings.
func TestParseFrontmatterBlockParameterCoercion(t *testing.T) {
	def, err := parseFrontmatterBlock([]string{
		"name: params",
		"parameters:",
		"  retries: 3",
		"  warmup: -1",
		"  timeout: 1.5",
		"  enabled: false",
		"  label: text",
	})
	require.NoError(t, err)
	require.Equal(t, 3, def.parameters["retries"])
	require.Equal(t, -1, def.parameters["warmup"])
	require.Equal(t, 1.5, def.parameters["timeout"])
	require.Equal(t, false, def.parameters["enabled"])
	require.Equal(t, "text", def.parameters["label"])
}

// parseFrontmatterBlock collects a tools list under the bare `tools:` key and
// tolerates an unknown key inside that nested context.
func TestParseFrontmatterBlockToolsListWithUnknown(t *testing.T) {
	def, err := parseFrontmatterBlock([]string{
		"name: t",
		"tools:",
		"  - bash",
		"  - read",
		"  some_unknown: ignored",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"bash", "read"}, def.tools)
	assert.Empty(t, def.parameters)
}

// parseFrontmatterBlock ignores an unknown top-level key and does not mistake
// it for a parameter when the parameter context has not been opened.
func TestParseFrontmatterBlockUnknownTopLevelIgnored(t *testing.T) {
	def, err := parseFrontmatterBlock([]string{
		"name: u",
		"mystery: 42",
		"description: kept",
	})
	require.NoError(t, err)
	assert.Equal(t, "u", def.name)
	assert.Equal(t, "kept", def.description)
	assert.Empty(t, def.parameters, "unknown non-parameter key is not stored")
}

// NewSkill with no options yields only the name; all other fields stay empty.
func TestNewSkillDefaultZeroFields(t *testing.T) {
	empty := NewSkill("onlyname")
	assert.Equal(t, "onlyname", empty.Name())
	assert.Equal(t, "", empty.Description())
	assert.Equal(t, "", empty.Version())
	assert.Equal(t, "", empty.Category())
	assert.Equal(t, "", empty.Prompt())
	assert.Nil(t, empty.Tools())
	assert.Nil(t, empty.Parameters())
	assert.Equal(t, "", empty.TriggerHint())
}

// Options applied in sequence compose and later options override earlier ones.
func TestSkillOptionCompositionOrder(t *testing.T) {
	d := NewSkill("compose",
		WithDescription("first"),
		WithDescription("second"),
		WithVersion("v1"),
		WithParameters(map[string]any{"a": 1}),
		WithParameters(map[string]any{"b": 2}),
	)
	assert.Equal(t, "second", d.Description())
	assert.Equal(t, "v1", d.Version())
	// The last WithParameters call wins outright (maps are replaced, not merged).
	assert.Equal(t, map[string]any{"b": 2}, d.Parameters())
}

// WithParameters behaves as replacement, never merging with prior params.
func TestWithParametersReplacesNotMerges(t *testing.T) {
	d := NewSkill("repl", WithParameters(map[string]any{"keep": true}))
	assert.Equal(t, map[string]any{"keep": true}, d.Parameters())
	assert.Empty(t, d.Tools())
}

// DefaultSkillDefinition getters all return the stored backing values.
func TestDefaultSkillDefinitionGetters(t *testing.T) {
	d := &DefaultSkillDefinition{
		name:        "g",
		description: "d",
		version:     "v",
		category:    "c",
		prompt:      "p",
		tools:       []string{"t"},
		parameters:  map[string]any{"k": "v"},
		triggerHint: "h",
	}
	assert.Equal(t, "g", d.Name())
	assert.Equal(t, "d", d.Description())
	assert.Equal(t, "v", d.Version())
	assert.Equal(t, "c", d.Category())
	assert.Equal(t, "p", d.Prompt())
	assert.Equal(t, []string{"t"}, d.Tools())
	assert.Equal(t, map[string]any{"k": "v"}, d.Parameters())
	assert.Equal(t, "h", d.TriggerHint())
}

// parseFrontmatterBlock returns a definition even for a purely empty block
// (no closing delimiter issues are the caller's concern).
func TestParseFrontmatterBlockEmpty(t *testing.T) {
	def, err := parseFrontmatterBlock(nil)
	require.NoError(t, err)
	assert.Equal(t, "", def.name)
	assert.Nil(t, def.tools)
	assert.Empty(t, def.parameters)
}

// A block scalar assigned to a key other than prompt is collected and then
// dropped (only prompt is stored from block scalars).
func TestParseFrontmatterBlockUnknownBlockScalarDropped(t *testing.T) {
	def, err := parseFrontmatterBlock([]string{
		"name: bs",
		"custom_block: |",
		"  line one",
		"  line two",
		"description: after",
	})
	require.NoError(t, err)
	assert.Equal(t, "bs", def.name)
	assert.Equal(t, "after", def.description)
	// block scalar content is only wired to prompt, so prompt stays empty.
	assert.Equal(t, "", def.prompt)
}
