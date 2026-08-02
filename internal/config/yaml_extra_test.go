package config

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// parseScalar coercion
// ---------------------------------------------------------------------------

func TestParseScalarCoercions(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{`"hello"`, "hello"},
		{`'single'`, "single"},
		{"true", true},
		{"false", false},
		{"null", nil},
		{"~", nil},
		{"42", int64(42)},
		{"-7", int64(-7)},
		{"3.14", 3.14},
		{"0.5", 0.5},
		{"plain-text", "plain-text"},
		{"", ""},
		{`"nested#comment"`, "nested#comment"},
		{"0123", int64(123)},
	}
	for _, tc := range cases {
		got := parseScalar(tc.in)
		assert.Equal(t, tc.want, got, "parseScalar(%q)", tc.in)
	}
}

// TestParseScalarQuotedKeepsLeadingSpace verifies quoted strings keep their
// interior formatting but surrounding whitespace is trimmed first.
func TestParseScalarQuoted(t *testing.T) {
	assert.Equal(t, "spaced  value", parseScalar(` "spaced  value" `))
}

// ---------------------------------------------------------------------------
// parseYAMLTree structure
// ---------------------------------------------------------------------------

// TestParseYAMLTreeEmpty verifies empty and comment-only documents parse to an
// empty mapping.
func TestParseYAMLTreeEmpty(t *testing.T) {
	tree, err := parseYAMLTree(nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{}, tree)

	tree, err = parseYAMLTree([]byte("\n\n# only a comment\n"))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{}, tree)
}

// TestParseYAMLTreeListAtTop verifies a top-level list is parsed.
func TestParseYAMLTreeListAtTop(t *testing.T) {
	tree, err := parseYAMLTree([]byte("- a\n- b\n"))
	require.NoError(t, err)
	assert.Equal(t, []any{"a", "b"}, tree)
}

// TestParseYAMLTreeNestedListItemsMap verifies list items that are mappings are
// parsed into map[string]any elements.
func TestParseYAMLTreeNestedListItemsMap(t *testing.T) {
	tree, err := parseYAMLTree([]byte("- key: value\n- plain\n"))
	require.NoError(t, err)
	list, ok := tree.([]any)
	require.True(t, ok)
	require.Len(t, list, 2)
	m, ok := list[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "value", m["key"])
	assert.Equal(t, "plain", list[1])
}

// TestParseYAMLTreeEmptyListItem verifies `-` with nothing parses to an empty
// mapping element.
func TestParseYAMLTreeEmptyListItem(t *testing.T) {
	tree, err := parseYAMLTree([]byte("-\n"))
	require.NoError(t, err)
	list, ok := tree.([]any)
	require.True(t, ok)
	require.Len(t, list, 1)
	_, isMap := list[0].(map[string]any)
	assert.True(t, isMap, "empty list item should be an empty mapping")
}

// TestParseYAMLTreeInvalidMappingLine verifies a top-level scalar line (no
// colon, no dash) cannot be parsed as a mapping and errors.
func TestParseYAMLTreeInvalidMappingLine(t *testing.T) {
	_, err := parseYAMLTree([]byte("not-a-key-value-line\n"))
	require.Error(t, err)
}

// TestParseYAMLTreeScalarMapValues verifies multiple scalar value kinds in a
// single mapping.
func TestParseYAMLTreeScalarMapValues(t *testing.T) {
	tree, err := parseYAMLTree([]byte("a: 1\nb: x\nc: true\nd: 1.5\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, int64(1), m["a"])
	assert.Equal(t, "x", m["b"])
	assert.Equal(t, true, m["c"])
	assert.Equal(t, 1.5, m["d"])
}

// TestParseYAMLTreeDeferredMap verifies an empty-value key collects the deeper
// block that follows it (sub-map).
func TestParseYAMLTreeDeferredMap(t *testing.T) {
	tree, err := parseYAMLTree([]byte("outer:\n  inner: 1\n"))
	require.NoError(t, err)
	m, ok := tree.(map[string]any)
	require.True(t, ok)
	inner, exists := m["outer"].(map[string]any)
	require.True(t, exists)
	assert.Equal(t, int64(1), inner["inner"])
}

// ---------------------------------------------------------------------------
// UnmarshalConfig error paths
// ---------------------------------------------------------------------------

// TestUnmarshalConfig_YAMLNonMappingTop verifies a non-mapping top level errors.
func TestUnmarshalConfig_YAMLNonMappingTop(t *testing.T) {
	var cfg Config
	err := UnmarshalConfig([]byte("- a\n- b\n"), ConfigFormatYAML, &cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be a mapping")
}

// TestUnmarshalConfig_NonPointerTarget verifies a non-pointer target errors.
func TestUnmarshalConfig_NonPointerTarget(t *testing.T) {
	// assignFromMap with a non-pointer target (reached via YAML path).
	err := assignFromMap(Config{}, map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-nil pointer")
}

// TestUnmarshalConfig_TypeMismatch verifies a scalar/type mismatch surfaces an
// error describing the offending key.
func TestUnmarshalConfig_TypeMismatch(t *testing.T) {
	// model.name is a string; assign an integer to force a string coercion path.
	doc := "model:\n  name: 42\n  max_tokens: notanint\n"
	var cfg Config
	err := UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_tokens")
}

// TestUnmarshalConfig_AssignSliceFromMapping verifies assigning a mapping to a
// slice field errors.
func TestUnmarshalConfig_AssignSliceFromMapping(t *testing.T) {
	doc := "tools:\n  builtin:\n    key: value\n"
	var cfg Config
	err := UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg)
	require.Error(t, err)
}

// TestUnmarshalConfig_MappingToSliceString verifies that a `{}` literal (parsed
// as the string "{}", not a map) assigned to a slice surfaces an error.
func TestUnmarshalConfig_MappingToSliceString(t *testing.T) {
	doc := "tools:\n  builtin: {}\n"
	var cfg Config
	require.Error(t, UnmarshalConfig([]byte(doc), ConfigFormatYAML, &cfg))
}

// ---------------------------------------------------------------------------
// Scalar assignment helpers
// ---------------------------------------------------------------------------

func TestScalarToString(t *testing.T) {
	str, ok := scalarToString("s")
	assert.True(t, ok)
	assert.Equal(t, "s", str)

	i, ok := scalarToString(int64(7))
	assert.True(t, ok)
	assert.Equal(t, "7", i)

	in, ok := scalarToString(7)
	assert.True(t, ok)
	assert.Equal(t, "7", in)

	f, ok := scalarToString(1.5)
	assert.True(t, ok)
	assert.Equal(t, "1.5", f)

	b, ok := scalarToString(true)
	assert.True(t, ok)
	assert.Equal(t, "true", b)

	_, ok = scalarToString(nil)
	assert.False(t, ok)
}

func TestScalarToBool(t *testing.T) {
	b, ok := scalarToBool(true)
	assert.True(t, ok)
	assert.True(t, b)

	b, ok = scalarToBool(int64(0))
	assert.True(t, ok)
	assert.False(t, b)

	b, ok = scalarToBool("true")
	assert.True(t, ok)
	assert.True(t, b)

	b, ok = scalarToBool("never")
	assert.False(t, ok)
	assert.False(t, b)

	_, ok = scalarToBool(1.5)
	assert.False(t, ok)
}

func TestScalarToInt64(t *testing.T) {
	i, ok := scalarToInt64(int64(9))
	assert.True(t, ok)
	assert.Equal(t, int64(9), i)

	i, ok = scalarToInt64(9)
	assert.True(t, ok)
	assert.Equal(t, int64(9), i)

	i, ok = scalarToInt64(9.7)
	assert.True(t, ok)
	assert.Equal(t, int64(9), i)

	i, ok = scalarToInt64("42")
	assert.True(t, ok)
	assert.Equal(t, int64(42), i)

	_, ok = scalarToInt64("abc")
	assert.False(t, ok)

	_, ok = scalarToInt64(true)
	assert.False(t, ok)
}

func TestScalarToFloat64(t *testing.T) {
	f, ok := scalarToFloat64(2.5)
	assert.True(t, ok)
	assert.Equal(t, 2.5, f)

	f, ok = scalarToFloat64(int64(3))
	assert.True(t, ok)
	assert.Equal(t, 3.0, f)

	f, ok = scalarToFloat64("1.25")
	assert.True(t, ok)
	assert.Equal(t, 1.25, f)

	_, ok = scalarToFloat64("nope")
	assert.False(t, ok)
}

// TestAssignScalarUnsupportedKind verifies assigning a scalar to a non-primitive
// destination kind surfaces an error.
func TestAssignScalarUnsupportedKind(t *testing.T) {
	var out Config
	// A struct destination cannot accept a scalar directly.
	err := assignScalar(reflect.ValueOf(&out).Elem(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported destination kind")
}

// ---------------------------------------------------------------------------
// YAMLConfigLoader.Load error paths
// ---------------------------------------------------------------------------

// TestYAMLConfigLoader_ReadError verifies a missing-file read failure surfaces
// with a wrapped error.
func TestYAMLConfigLoader_ReadError(t *testing.T) {
	l := NewYAMLConfigLoader()
	_, err := l.Load(context.Background(), filepath.Join(t.TempDir(), "gone.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read")
}

// TestYAMLConfigLoader_UnknownExt verifies unknown extension surfaces the
// detect-format error wrapped with the path.
func TestYAMLConfigLoader_UnknownExt(t *testing.T) {
	l := NewYAMLConfigLoader()
	_, err := l.Load(context.Background(), "config.cfg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detect format")
}

// TestStripYAMLComment verifies comment handling respects quoted regions.
func TestStripYAMLComment(t *testing.T) {
	assert.Equal(t, "a: 1", stripYAMLComment("a: 1 # trailing"))
	assert.Equal(t, `"# not a comment"`, stripYAMLComment(`"# not a comment"`))
	assert.Equal(t, `'# also not'`, stripYAMLComment(`'# also not'`))
	// A '#' glued to text is not a comment.
	assert.Equal(t, "url#anchor", stripYAMLComment("url#anchor"))
	// Tabs before '#' are treated as separators too.
	assert.Equal(t, "x: 1", stripYAMLComment("x: 1\t#tab"))
}

// TestSplitKeyValue verifies key/value splitting at the first colon.
func TestSplitKeyValue(t *testing.T) {
	key, rest, ok := splitKeyValue("name: value")
	require.True(t, ok)
	assert.Equal(t, "name", key)
	assert.Equal(t, "value", rest)

	// A colon inside the value does not affect the split.
	key, rest, ok = splitKeyValue("url: http://x#frag")
	require.True(t, ok)
	assert.Equal(t, "url", key)
	assert.Equal(t, "http://x#frag", rest)

	// No colon => not a key/value line.
	_, _, ok = splitKeyValue("justtext")
	assert.False(t, ok)

	// Empty key => not ok.
	_, _, ok = splitKeyValue(": value")
	assert.False(t, ok)
}

// TestBuildYAMLLinesCRLF verifies Windows line endings are stripped and the
// trailing newline yields a blank line.
func TestBuildYAMLLinesCRLF(t *testing.T) {
	lines := buildYAMLLines([]byte("a: 1\r\nb: 2\r\n"))
	require.Len(t, lines, 3)
	assert.Equal(t, "a: 1", lines[0].text)
	assert.Equal(t, "b: 2", lines[1].text)
	assert.True(t, lines[2].isBlank)
}
