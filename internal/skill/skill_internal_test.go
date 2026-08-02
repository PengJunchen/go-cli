package skill

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// parseScalar strips surrounding double/single quotes and trims spaces.
func TestParseScalar(t *testing.T) {
	assert.Equal(t, "hello", parseScalar(`  "hello"  `))
	assert.Equal(t, "it''s", parseScalar(`'it''s'`)) // only outer quotes stripped
	assert.Equal(t, "raw", parseScalar("raw"))
	assert.Equal(t, `"a`, parseScalar(`"a`)) // unmatched quote kept as-is
	assert.Equal(t, "", parseScalar("   "))
}

// coerceParamValue: integers, floats, bools, quoted strings, and empty.
func TestCoerceParamValue(t *testing.T) {
	assert.Equal(t, 42, coerceParamValue("42"))
	assert.Equal(t, 1.5, coerceParamValue("1.5"))
	assert.Equal(t, true, coerceParamValue("true"))
	assert.Equal(t, false, coerceParamValue("false"))
	assert.Equal(t, "hello world", coerceParamValue(`"hello world"`))
	assert.Equal(t, "", coerceParamValue("   "), "whitespace-only trims to empty")
	assert.Equal(t, "", coerceParamValue(""))
	assert.Equal(t, 7, coerceParamValue("007"), "leading-zero numeric is parsed as an int")
}

// splitKeyValue parses "key: rest" and rejects lines without a colon.
func TestSplitKeyValue(t *testing.T) {
	key, rest, ok := splitKeyValue("name: example")
	assert.True(t, ok)
	assert.Equal(t, "name", key)
	assert.Equal(t, " example", rest)

	_, _, ok = splitKeyValue(":no-key")
	assert.False(t, ok)
	_, _, ok = splitKeyValue("no-colon-here")
	assert.False(t, ok)
	_, _, ok = splitKeyValue("")
	assert.False(t, ok)
}

// isIndented / stripIndent handle spaces and tabs.
func TestIndentHelpers(t *testing.T) {
	assert.True(t, isIndented("  x"))
	assert.True(t, isIndented("\tx"))
	assert.False(t, isIndented("x"))
	assert.False(t, isIndented(""))

	assert.Equal(t, "x", stripIndent("  x"))
	assert.Equal(t, "x", stripIndent("\tx"))
	assert.Equal(t, "  x", stripIndent("    x"), "only one indent level is stripped")
}

// isSkillFileName recognizes the supported suffixes and rejects others.
func TestIsSkillFileName(t *testing.T) {
	for _, name := range []string{"a.md", "b.skill.md", "c.yaml", "d.yml"} {
		assert.True(t, isSkillFileName(name), name)
	}
	for _, name := range []string{"e.txt", "f.md.bak", "noext", ""} {
		assert.False(t, isSkillFileName(name), name)
	}
}

// parseFrontmatter mutation safety: the caller's slice is not relied on after
// the call; empty input and bare delimiter cases are covered.
func TestParseFrontmatterBlockEmptyAndLists(t *testing.T) {
	def, err := parseFrontmatterBlock([]string{
		"name: list-skill",
		"tools:", // bare key starts a list with zero items
	})
	require.NoError(t, err)
	assert.Equal(t, "list-skill", def.name)
	assert.Nil(t, def.tools, "bare list key with no items yields nil tools")

	// A prompt: | block scalar with trailing blank line is trimmed.
	def, err = parseFrontmatterBlock([]string{
		"name: block",
		"prompt: |",
		"  line 1",
		"",
		"  line 2",
		"",
	})
	require.NoError(t, err)
	assert.Equal(t, "line 1\n\nline 2", def.prompt)
}

// parseFrontmatter: missing opening or closing delimiter yields a parse error.
func TestParseFrontmatterDelimiterErrors(t *testing.T) {
	_, err := parseFrontmatter([]string{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errParse)

	_, err = parseFrontmatter([]string{"not-a-delimiter", "name: x"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errParse)

	_, err = parseFrontmatter([]string{"---", "name: x"}) // no closing delimiter
	require.Error(t, err)
	assert.ErrorIs(t, err, errParse)
}

// parseFrontmatter: body is used only when no prompt is declared.
func TestParseFrontmatterBodyFallback(t *testing.T) {
	def, err := parseFrontmatter([]string{"---", "name: x", "---", "the body"})
	require.NoError(t, err)
	assert.Equal(t, "the body", def.prompt)

	// Explicit prompt wins over the body.
	def, err = parseFrontmatter([]string{"---", "name: x", "prompt: explicit", "---", "the body"})
	require.NoError(t, err)
	assert.Equal(t, "explicit", def.prompt)
}

// matchScore ranks exact > prefix > substring name > description/hint.
func TestMatchScoreRanking(t *testing.T) {
	lower := func(name, desc, hint string) SkillDefinition {
		return &DefaultSkillDefinition{name: name, description: desc, triggerHint: hint}
	}
	assert.Equal(t, 5, matchScore(lower("fix-bug", "d", "h"), "fix-bug"))
	assert.Equal(t, 4, matchScore(lower("fix-bug", "d", "h"), "fix"))
	assert.Equal(t, 3, matchScore(lower("fix-bug", "d", "h"), "bug"))
	assert.Equal(t, 2, matchScore(lower("fix-bug", "fix the bug", "h"), "the"))
	// Description wins over trigger hint only by term presence; both score 2.
	assert.Equal(t, 2, matchScore(lower("fix-bug", "d", "fix thing"), "thing"))
	assert.Equal(t, 0, matchScore(lower("fix-bug", "d", "h"), "nomatch"))
}

// containsString is a simple linear membership check.
func TestContainsString(t *testing.T) {
	assert.True(t, containsString([]string{"a", "b"}, "b"))
	assert.False(t, containsString([]string{"a", "b"}, "c"))
	assert.False(t, containsString(nil, "a"))
}

// NewSkill applies options onto a fresh default definition.
func TestNewSkillWithOptions(t *testing.T) {
	d := NewSkill(
		"opts",
		WithDescription("desc"),
		WithVersion("v1"),
		WithCategory("cat"),
		WithPrompt("prompt"),
		WithTools("a", "b"),
		WithParameters(map[string]any{"k": "v"}),
		WithTriggerHint("hint"),
	)
	assert.Equal(t, "opts", d.Name())
	assert.Equal(t, "desc", d.Description())
	assert.Equal(t, "v1", d.Version())
	assert.Equal(t, "cat", d.Category())
	assert.Equal(t, "prompt", d.Prompt())
	assert.Equal(t, []string{"a", "b"}, d.Tools())
	assert.Equal(t, map[string]any{"k": "v"}, d.Parameters())
	assert.Equal(t, "hint", d.TriggerHint())
}

// WithTools/WithParameters copy inputs so later mutation does not leak in.
func TestSkillOptionsCopySemantics(t *testing.T) {
	tools := []string{"bash"}
	params := map[string]any{"x": 1}
	d := NewSkill("copy", WithTools(tools...), WithParameters(params))

	tools[0] = "mutated"
	params["x"] = 999

	assert.Equal(t, []string{"bash"}, d.Tools(), "tools must be a defensive copy")
	assert.Equal(t, map[string]any{"x": 1}, d.Parameters(), "parameters must be a defensive copy")
}
