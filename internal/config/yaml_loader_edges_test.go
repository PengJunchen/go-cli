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
// space-indentation is measured. Tab characters count as 4 spaces (tab-stop
// rounding), so a single-tab-indented line reports indent 4.
func TestBuildYAMLLines_ListTaggingAndIndent(t *testing.T) {
	lines := buildYAMLLines([]byte("tools:\n\tbuiltin:\n  - a\n  plain: x\n"))
	// Five elements: four significant lines plus the trailing-newline blank.
	require.Len(t, lines, 5)

	// tools: is a plain mapping line.
	assert.False(t, lines[0].listItem)
	assert.Equal(t, 0, lines[0].indent)

	// Tab-indented "builtin:" — tab counts as 4 spaces.
	assert.False(t, lines[1].listItem)
	assert.Equal(t, 4, lines[1].indent)

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

// TestUnmarshalConfig_YAMLUnknownKeysIgnored verifies unrecognized top-level
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

// TestYAMLConfigLoader_ExplicitJSONPath verifies the loader honors an explicit
// .json path (no extension sniff surprise).
func TestYAMLConfigLoader_ExplicitJSONPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.cfg") // unknown ext
	require.NoError(t, os.WriteFile(path, []byte("not json"), 0o600))
	_, err := NewYAMLConfigLoader().Load(context.Background(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detect format")
}

// ---------------------------------------------------------------------------
// HI-12: YAML parser edge case tests (task 48-14)
//
// These tests exercise the hand-written YAML parser against edge cases that
// could cause configuration corruption or crashes. Where the parser has a
// known limitation, the test documents the actual behavior with a TODO comment
// rather than changing the parser.
// ---------------------------------------------------------------------------

// TestParseYAMLTree_CommentInQuotedString verifies that a '#' inside a
// double-quoted value is NOT treated as a comment (AC-1).
func TestParseYAMLTree_CommentInQuotedString(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: "value # not a comment"`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "value # not a comment", m["key"])
}

// TestParseYAMLTree_CommentInSingleQuotedString verifies that a '#' inside a
// single-quoted value is NOT treated as a comment.
func TestParseYAMLTree_CommentInSingleQuotedString(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: 'value # not a comment'`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "value # not a comment", m["key"])
}

// TestParseYAMLTree_QuotedHashFollowedByComment verifies a quoted value
// containing '#' followed by a real inline comment parses correctly.
func TestParseYAMLTree_QuotedHashFollowedByComment(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: "value#1" # real comment`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "value#1", m["key"])
}

// TestParseYAMLTree_VersionNumberUnquoted verifies that an unquoted "1.0" is
// parsed as a float (a known limitation of the simple scalar coercer).
// TODO: parser treats unquoted 1.0 as float64(1.0), not string "1.0".
func TestParseYAMLTree_VersionNumberUnquoted(t *testing.T) {
	tree, err := parseYAMLTree([]byte("version: 1.0\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	// The parser coerces 1.0 to float64; use quotes to preserve as string.
	assert.Equal(t, float64(1.0), m["version"])
}

// TestParseYAMLTree_VersionNumberQuoted verifies that a quoted "1.0" is
// preserved as a string (AC-2).
func TestParseYAMLTree_VersionNumberQuoted(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`version: "1.0"` + "\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "1.0", m["version"])
}

// TestParseYAMLTree_EmptyValueYieldsEmptyMap verifies that "key:" with nothing
// after it yields an empty map (tolerant default).
func TestParseYAMLTree_EmptyValueYieldsEmptyMap(t *testing.T) {
	tree, err := parseYAMLTree([]byte("key:\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{}, m["key"])
}

// TestParseYAMLTree_SpecialCharColonInQuotedValue verifies that ": " inside a
// quoted value is preserved.
func TestParseYAMLTree_SpecialCharColonInQuotedValue(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: "a: b"`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a: b", m["key"])
}

// TestParseYAMLTree_SpecialCharBracesInQuotedValue verifies that "{}" inside a
// quoted value is preserved as a string, not parsed as a flow map.
func TestParseYAMLTree_SpecialCharBracesInQuotedValue(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: "{}"`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "{}", m["key"])
}

// TestParseYAMLTree_SpecialCharBracketsInQuotedValue verifies that "[]" inside
// a quoted value is preserved as a string.
func TestParseYAMLTree_SpecialCharBracketsInQuotedValue(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: "[a, b]"`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "[a, b]", m["key"])
}

// TestParseYAMLTree_BackslashInSingleQuotedValue verifies that a backslash
// inside a single-quoted value is preserved literally (no escape processing).
func TestParseYAMLTree_BackslashInSingleQuotedValue(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: 'a\b\c'`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, `a\b\c`, m["key"])
}

// TestParseYAMLTree_UnicodeCJKInKeysAndValues verifies CJK characters in keys
// and values parse correctly.
func TestParseYAMLTree_UnicodeCJKInKeysAndValues(t *testing.T) {
	tree, err := parseYAMLTree([]byte("名称: 你好世界\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "你好世界", m["名称"])
}

// TestParseYAMLTree_UnicodeEmojiInValue verifies emoji in values parse
// correctly.
func TestParseYAMLTree_UnicodeEmojiInValue(t *testing.T) {
	tree, err := parseYAMLTree([]byte("key: 🚀🎉\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "🚀🎉", m["key"])
}

// TestParseYAMLTree_BooleanLikeStringYes verifies that a quoted "yes" is
// parsed as a string, not a boolean.
func TestParseYAMLTree_BooleanLikeStringYes(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: "yes"`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "yes", m["key"])
}

// TestParseYAMLTree_BooleanLikeStringNo verifies that a quoted "no" is parsed
// as a string, not a boolean.
func TestParseYAMLTree_BooleanLikeStringNo(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: "no"`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "no", m["key"])
}

// TestParseYAMLTree_UnquotedYesIsString verifies that an unquoted "yes" is
// parsed as a string (the parser only recognizes lowercase true/false as
// booleans, following YAML 1.2).
func TestParseYAMLTree_UnquotedYesIsString(t *testing.T) {
	tree, err := parseYAMLTree([]byte("key: yes\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "yes", m["key"])
}

// TestParseYAMLTree_LeadingZeroNumber verifies that a number with leading zeros
// like "007" is parsed as integer 7 (strconv.ParseInt base 10 strips leading
// zeros).
func TestParseYAMLTree_LeadingZeroNumber(t *testing.T) {
	tree, err := parseYAMLTree([]byte("key: 007\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, int64(7), m["key"])
}

// TestParseYAMLTree_LeadingZeroQuoted verifies that a quoted "007" is preserved
// as a string.
func TestParseYAMLTree_LeadingZeroQuoted(t *testing.T) {
	tree, err := parseYAMLTree([]byte(`key: "007"`))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "007", m["key"])
}

// TestParseYAMLTree_AnchorNotSupported verifies that YAML anchors (&anchor) are
// not processed — the value is kept as a literal string.
// TODO: parser does not support anchors/aliases (documented limitation).
func TestParseYAMLTree_AnchorNotSupported(t *testing.T) {
	tree, err := parseYAMLTree([]byte("key: &anchor value\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	// The anchor marker is not stripped; the whole thing is a string.
	assert.Equal(t, "&anchor value", m["key"])
}

// TestParseYAMLTree_AliasNotSupported verifies that YAML aliases (*anchor) are
// not processed — the value is kept as a literal string.
// TODO: parser does not support anchors/aliases (documented limitation).
func TestParseYAMLTree_AliasNotSupported(t *testing.T) {
	tree, err := parseYAMLTree([]byte("key: *anchor\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "*anchor", m["key"])
}

// TestParseYAMLTree_FlowSequenceNotSupported verifies that flow sequences
// ([a, b, c]) are not parsed as lists — the value is kept as a literal string.
// TODO: parser does not support flow sequences, only flow maps.
func TestParseYAMLTree_FlowSequenceNotSupported(t *testing.T) {
	tree, err := parseYAMLTree([]byte("key: [a, b, c]\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "[a, b, c]", m["key"])
}

// TestParseYAMLTree_TrailingWhitespaceTrimmed verifies that trailing whitespace
// after an unquoted value is trimmed.
func TestParseYAMLTree_TrailingWhitespaceTrimmed(t *testing.T) {
	tree, err := parseYAMLTree([]byte("key: value   \n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "value", m["key"])
}

// TestParseYAMLTree_NullRepresentations verifies the various null
// representations. The parser recognizes "null" and "~" as nil.
// TODO: parser does not recognize "NULL", "Null" as null (only lowercase
// "null" and "~").
func TestParseYAMLTree_NullRepresentations(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"null", nil},
		{"~", nil},
		// TODO: "NULL" and "Null" are not recognized as null by the parser.
		{"NULL", "NULL"},
		{"Null", "Null"},
	}
	for _, tc := range cases {
		tree, err := parseYAMLTree([]byte("key: " + tc.in + "\n"))
		require.NoError(t, err, "input %q", tc.in)
		m, ok := tree.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, tc.want, m["key"], "input %q", tc.in)
	}
}

// TestParseYAMLTree_BooleanRepresentations verifies boolean parsing. The parser
// only recognizes lowercase "true" and "false".
// TODO: parser does not recognize "True", "False", "TRUE", "FALSE", "yes",
// "no", "on", "off" as booleans.
func TestParseYAMLTree_BooleanRepresentations(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"true", true},
		{"false", false},
		// TODO: these are not recognized as booleans by the parser.
		{"True", "True"},
		{"False", "False"},
		{"TRUE", "TRUE"},
		{"FALSE", "FALSE"},
		{"yes", "yes"},
		{"no", "no"},
		{"on", "on"},
		{"off", "off"},
	}
	for _, tc := range cases {
		tree, err := parseYAMLTree([]byte("key: " + tc.in + "\n"))
		require.NoError(t, err, "input %q", tc.in)
		m, ok := tree.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, tc.want, m["key"], "input %q", tc.in)
	}
}

// TestParseYAMLTree_NegativeNumbersAndFloats verifies negative integers and
// floats are parsed correctly.
func TestParseYAMLTree_NegativeNumbersAndFloats(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"-123", int64(-123)},
		{"3.14", 3.14},
		{"-0.5", -0.5},
		{"-100", int64(-100)},
		{"0", int64(0)},
		{"-0", int64(0)},
	}
	for _, tc := range cases {
		tree, err := parseYAMLTree([]byte("key: " + tc.in + "\n"))
		require.NoError(t, err, "input %q", tc.in)
		m, ok := tree.(map[string]any)
		require.True(t, ok)
		assert.Equal(t, tc.want, m["key"], "input %q", tc.in)
	}
}

// TestParseYAMLTree_MixedTypesInBlockSequence verifies that a block-style
// sequence with mixed scalar types parses each element to the correct type.
func TestParseYAMLTree_MixedTypesInBlockSequence(t *testing.T) {
	tree, err := parseYAMLTree([]byte("key:\n  - 1\n  - \"two\"\n  - true\n  - null\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	list, ok := m["key"].([]any)
	require.True(t, ok)
	require.Len(t, list, 4)
	assert.Equal(t, int64(1), list[0])
	assert.Equal(t, "two", list[1])
	assert.Equal(t, true, list[2])
	assert.Nil(t, list[3])
}

// TestParseYAMLTree_CRLFLineEndings verifies that Windows-style CRLF line
// endings are handled correctly through the full parse pipeline.
func TestParseYAMLTree_CRLFLineEndings(t *testing.T) {
	doc := "a: 1\r\nb: 2\r\n"
	tree, err := parseYAMLTree([]byte(doc))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, int64(1), m["a"])
	assert.Equal(t, int64(2), m["b"])
}

// TestUnmarshalConfig_YAMLMultipleDocumentsNotSupported verifies that the YAML
// multi-document separator "---" is not supported. Since "---" starts with
// "-", the parser treats it as a list item, making the top level a list rather
// than a mapping, which UnmarshalConfig rejects.
// TODO: parser does not support multi-document streams.
func TestUnmarshalConfig_YAMLMultipleDocumentsNotSupported(t *testing.T) {
	doc := "---\na: 1\n---\nb: 2\n"
	var cfg Config
	err := UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a mapping")
}
