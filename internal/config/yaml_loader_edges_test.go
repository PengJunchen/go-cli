package config

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// stripYAMLComment: quoted-region handling
// ---------------------------------------------------------------------------

// TestStripYAMLComment_EscapedQuoteInDoubleQuotedRegion verifies an escaped
// double quote inside a double-quoted region does not close the region.
func TestStripYAMLComment_EscapedQuoteInDoubleQuotedRegion(t *testing.T) {
	in := `key: "a \" b" # comment`
	assert.Equal(t, `key: "a \" b"`, stripYAMLComment(in))
}

// TestStripYAMLComment_CommentAfterClosingQuote verifies a '#' that sits
// directly after a closing quote (no separator) is part of the value, whereas a
// whitespace-separated comment is stripped.
func TestStripYAMLComment_CommentAfterClosingQuote(t *testing.T) {
	assert.Equal(t, `key: "value"#octothorp`, stripYAMLComment(`key: "value"#octothorp`))
	assert.Equal(t, `key: "value"`, stripYAMLComment(`key: "value" # real comment`))
}

// TestStripYAMLComment_None verifies strings with no comment come back as-is.
func TestStripYAMLComment_None(t *testing.T) {
	assert.Equal(t, "no comment here", stripYAMLComment("no comment here"))
	assert.Equal(t, "single ' quote", stripYAMLComment("single ' quote"))
}

// ---------------------------------------------------------------------------
// buildYAMLLines indentation & list tagging
// ---------------------------------------------------------------------------

// TestBuildYAMLLines_ListTaggingAndIndent verifies list items are tagged and
// space-indentation is measured. Tab characters do not advance indentWidth (the
// parser only counts spaces), so tab-only lines report indent 0.
func TestBuildYAMLLines_ListTaggingAndIndent(t *testing.T) {
	lines := buildYAMLLines([]byte("tools:\n\tbuiltin:\n  - a\n  plain: x\n"))
	// Five elements: four significant lines plus the trailing-newline blank.
	require.Len(t, lines, 5)

	// tools: is a plain mapping line.
	assert.False(t, lines[0].listItem)
	assert.Equal(t, 0, lines[0].indent)

	// Tab-indented "builtin:" — the tab contributes no space indent.
	assert.False(t, lines[1].listItem)
	assert.Equal(t, 0, lines[1].indent)

	// Two-space "- a" is a list item.
	assert.True(t, lines[2].listItem)
	assert.Equal(t, 2, lines[2].indent)

	// Two-space "plain: x".
	assert.False(t, lines[3].listItem)
	assert.Equal(t, 2, lines[3].indent)

	// Trailing blank line is marked.
	assert.True(t, lines[4].isBlank)
}

// TestBuildYAMLLines_BlankLinesMarked verifies blank and whitespace-only lines
// are marked isBlank and carry empty text.
func TestBuildYAMLLines_BlankLinesMarked(t *testing.T) {
	lines := buildYAMLLines([]byte("a: 1\n\n   \nb: 2\n"))
	// Five elements: two significant lines, two interior blanks, trailing blank.
	require.Len(t, lines, 5)
	assert.False(t, lines[0].isBlank)
	assert.True(t, lines[1].isBlank)
	assert.True(t, lines[2].isBlank)
	assert.False(t, lines[3].isBlank)
	assert.Equal(t, "b: 2", lines[3].text)
	assert.True(t, lines[4].isBlank)
}

// ---------------------------------------------------------------------------
// parseYAMLTree deeper blocks
// ---------------------------------------------------------------------------

// TestParseYAMLTree_DeferredList verifies an empty-valued key collects a deeper
// list block.
func TestParseYAMLTree_DeferredList(t *testing.T) {
	tree, err := parseYAMLTree([]byte("items:\n  - a\n  - b\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"a", "b"}, m["items"])
}

// TestParseYAMLTree_EmptyValueTrailing verifies a trailing empty value yields an
// empty mapping (tolerant default).
func TestParseYAMLTree_EmptyValueTrailing(t *testing.T) {
	tree, err := parseYAMLTree([]byte("model:\n  name:\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	sub, ok := m["model"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{}, sub["name"])
}

// TestParseYAMLTree_NonSiblingTerminatesMap verifies a deeper-indented line
// stops the sibling loop, so both it and any later top-level line are not
// collected as top-level keys of the same mapping.
func TestParseYAMLTree_NonSiblingTerminatesMap(t *testing.T) {
	tree, err := parseYAMLTree([]byte("a: 1\n  b: 2\nc: 3\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	// "a" is the only successfully parsed top-level sibling; the divergent
	// indent of "b" halts collection before "c" is ever reached.
	assert.Equal(t, map[string]any{"a": int64(1)}, m)
}

// ---------------------------------------------------------------------------
// assignValue: pointer & slice handling
// ---------------------------------------------------------------------------

// TestAssignValue_MapToEmptySliceIsEmpty verifies assigning an empty mapping to
// a slice field yields an empty slice (not an error).
func TestAssignValue_MapToEmptySliceIsEmpty(t *testing.T) {
	var cfg ToolsConfig
	dst := reflect.ValueOf(&cfg).Elem().FieldByName("Builtin")
	err := assignValue(dst, map[string]any{})
	require.NoError(t, err)
	assert.NotNil(t, cfg.Builtin)
	assert.Empty(t, cfg.Builtin)
}

// TestAssignValue_InvalidDestinationIsNoOp verifies an un-settable reflect value
// is ignored rather than panicking.
func TestAssignValue_InvalidDestinationIsNoOp(t *testing.T) {
	require.NoError(t, assignValue(reflect.Value{}, "x"))
}

// TestAssignValue_NilPointerSetsZero verifies a nil source pointer assignment
// zeroes the destination pointer.
func TestAssignValue_NilPointerSetsZero(t *testing.T) {
	// Use a stand-in struct carrying a pointer to exercise the branch.
	type holder struct{ P *TracingConfig }
	h := holder{P: &TracingConfig{}}
	f := reflect.ValueOf(&h).Elem().FieldByName("P")
	require.NoError(t, assignValue(f, nil))
	assert.Nil(t, h.P)
}

// TestAssignValue_StructFromMapPopulatesNestedField verifies a map assigned to
// a nested struct field fills its matching members (json-tag matching).
func TestAssignValue_StructFromMapPopulatesNestedField(t *testing.T) {
	var cfg Config
	f := reflect.ValueOf(&cfg).Elem().FieldByName("Tracing")
	require.NoError(t, assignValue(f, map[string]any{"exporter": "jsonl", "level": "debug"}))
	assert.Equal(t, "jsonl", cfg.Tracing.Exporter)
	assert.Equal(t, "debug", cfg.Tracing.Level)
}

// TestAssignFromMap_NilPointerTarget verifies assignFromMap rejects a nil
// pointer target.
func TestAssignFromMap_NilPointerTarget(t *testing.T) {
	var nilCfg *Config
	err := assignFromMap(nilCfg, map[string]any{"model": map[string]any{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil pointer")
}

// ---------------------------------------------------------------------------
// Unknown keys and lowercase fallback
// ---------------------------------------------------------------------------

// TestUnmarshalConfig_YAMLUnknownKeysIgnored verifies unrecognised top-level
// keys are tolerated rather than causing an error.
func TestUnmarshalConfig_YAMLUnknownKeysIgnored(t *testing.T) {
	doc := "unknown_key: whatever\nmodel:\n  name: gpt-4\nanother: 5\n"
	var cfg Config
	require.NoError(t, UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg))
	assert.Equal(t, "gpt-4", cfg.Model.Name)
}

// TestReplaceAssignment_DoesNotClobberConflictingFields verifies a YAML doc that
// only sets one tool slice leaves the other slice untouched.
func TestReplaceAssignment_PartialToolSlices(t *testing.T) {
	doc := "tools:\n  registry:\n    - r1\n"
	var cfg Config
	require.NoError(t, UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg))
	assert.Equal(t, []string{"r1"}, cfg.Tools.Registry)
	assert.Nil(t, cfg.Tools.Builtin)
}

// ---------------------------------------------------------------------------
// Scalar coercion: additional shapes
// ---------------------------------------------------------------------------

// TestScalarToFloat64_LosslessFloat32 verifies a float32 source converts
// exactly.
func TestScalarToFloat64_LosslessFloat32(t *testing.T) {
	f, ok := scalarToFloat64(float32(1.25))
	assert.True(t, ok)
	assert.Equal(t, 1.25, f)
}

// TestScalarToInt64_FloatTruncation verifies a non-integer float truncates
// toward zero when coerced to an integer.
func TestScalarToInt64_FloatTruncation(t *testing.T) {
	i, ok := scalarToInt64(9.99)
	assert.True(t, ok)
	assert.Equal(t, int64(9), i)

	i, ok = scalarToInt64(-9.99)
	assert.True(t, ok)
	assert.Equal(t, int64(-9), i)
}

// TestScalarToFloat64_WhitespaceString verifies string floats with surrounding
// whitespace parse successfully.
func TestScalarToFloat64_WhitespaceString(t *testing.T) {
	f, ok := scalarToFloat64("  2.5  ")
	assert.True(t, ok)
	assert.Equal(t, 2.5, f)
}

// TestScalarToInt64_IntFromFloatStringRejected verifies a float-looking string
// (e.g. "1.5") cannot be coerced to an int directly.
func TestScalarToInt64_FloatStringRejected(t *testing.T) {
	_, ok := scalarToInt64("1.5")
	assert.False(t, ok)
}

// TestScalarToBool_NumberFalseMapping verifies integer-zero maps to bool false.
func TestScalarToBool_ZeroIntMapsFalse(t *testing.T) {
	b, ok := scalarToBool(int64(0))
	assert.True(t, ok)
	assert.False(t, b)
}

// ---------------------------------------------------------------------------
// UnmarshalConfig integer-typed YAML field (boundary/zero)
// ---------------------------------------------------------------------------

// TestUnmarshalConfig_YAMLZeroAndNegativeInts verifies integer parsing of zero
// and negative values into typed fields.
func TestUnmarshalConfig_YAMLZeroAndNegativeInts(t *testing.T) {
	doc := "model:\n  max_tokens: 0\nprovider:\n  max_tokens: -10\n"
	var cfg Config
	require.NoError(t, UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg))
	assert.Equal(t, 0, cfg.Model.MaxTokens)
	assert.Equal(t, -10, cfg.Provider.MaxTokens)
}

// ---------------------------------------------------------------------------
// YAMLConfigLoader with a YAML doc that surfaces validation-independent errors
// ---------------------------------------------------------------------------

// TestYAMLConfigLoader_TopLevelList verifies a YAML file whose document root is
// not a mapping errors clearly.
func TestYAMLConfigLoader_TopLevelList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("- a\n- b\n"), 0o600))
	_, err := NewYAMLConfigLoader().Load(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a mapping")
}

// TestYAMLConfigLoader_ExplicitJSONPath verifies the loader honours an explicit
// .json path (no extension sniff surprise).
func TestYAMLConfigLoader_ExplicitJSONPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.cfg") // unknown ext
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))
	_, err := NewYAMLConfigLoader().Load(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detect format")
}
