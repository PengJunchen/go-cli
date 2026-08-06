package highlighters

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSpecsContainAllLanguages verifies that every expected language name and
// alias is present in the Specs map.
func TestSpecsContainAllLanguages(t *testing.T) {
	expected := []string{
		"go", "golang",
		"python", "py",
		"javascript", "js",
		"typescript", "ts",
		"rust", "rs",
		"java",
		"bash", "sh", "shell",
		"json",
		"yaml", "yml",
		"sql",
	}
	for _, name := range expected {
		t.Run(name, func(t *testing.T) {
			spec, ok := Specs[name]
			assert.True(t, ok, "language %q should be in Specs", name)
			assert.NotNil(t, spec.Keywords, "language %q should have non-nil Keywords", name)
		})
	}
}

// TestGoSpecKeywords verifies the Go keyword set.
func TestGoSpecKeywords(t *testing.T) {
	spec := Specs["go"]
	keywords := []string{
		"func", "var", "const", "type", "struct", "interface", "package",
		"import", "return", "if", "else", "for", "range", "switch", "case",
		"default", "break", "continue", "go", "defer", "select", "chan",
		"map", "nil", "true", "false",
	}
	for _, kw := range keywords {
		assert.True(t, spec.Keywords[kw], "Go keyword %q should be present", kw)
	}
	assert.Equal(t, "//", spec.CommentLine, "Go CommentLine should be //")
	assert.Equal(t, [2]string{"/*", "*/"}, spec.CommentBlock, "Go CommentBlock should be /* */")
	assert.Contains(t, spec.StringDelims, "\"", "Go should support double-quote strings")
	assert.Contains(t, spec.StringDelims, "'", "Go should support single-quote strings")
	assert.Contains(t, spec.StringDelims, "`", "Go should support backtick strings")
	assert.False(t, spec.CaseInsensitive, "Go should be case-sensitive")
}

// TestPythonSpecKeywords verifies the Python keyword set.
func TestPythonSpecKeywords(t *testing.T) {
	spec := Specs["python"]
	keywords := []string{
		"def", "class", "import", "from", "return", "if", "elif", "else",
		"for", "while", "try", "except", "finally", "with", "as", "lambda",
		"yield", "pass", "break", "continue", "raise", "None", "True",
		"False", "and", "or", "not", "in", "is",
	}
	for _, kw := range keywords {
		assert.True(t, spec.Keywords[kw], "Python keyword %q should be present", kw)
	}
	assert.Equal(t, "#", spec.CommentLine, "Python CommentLine should be #")
}

// TestJavaScriptSpecKeywords verifies the JavaScript keyword set.
func TestJavaScriptSpecKeywords(t *testing.T) {
	spec := Specs["javascript"]
	keywords := []string{
		"function", "var", "let", "const", "class", "extends", "return",
		"if", "else", "for", "while", "switch", "case", "break", "continue",
		"new", "this", "typeof", "instanceof", "null", "undefined", "true",
		"false", "async", "await", "import", "export", "from", "default",
	}
	for _, kw := range keywords {
		assert.True(t, spec.Keywords[kw], "JavaScript keyword %q should be present", kw)
	}
	assert.Equal(t, "//", spec.CommentLine, "JavaScript CommentLine should be //")
}

// TestTypeScriptSpecKeywords verifies the TypeScript keyword set includes
// JavaScript keywords plus TS-specific ones.
func TestTypeScriptSpecKeywords(t *testing.T) {
	spec := Specs["typescript"]
	// TypeScript-specific keywords.
	tsKeywords := []string{"type", "interface", "enum", "namespace", "as", "is"}
	for _, kw := range tsKeywords {
		assert.True(t, spec.Keywords[kw], "TypeScript keyword %q should be present", kw)
	}
	// Should also contain JS keywords.
	assert.True(t, spec.Keywords["function"], "TypeScript should include JS keyword function")
	assert.True(t, spec.Keywords["async"], "TypeScript should include JS keyword async")
}

// TestRustSpecKeywords verifies the Rust keyword set.
func TestRustSpecKeywords(t *testing.T) {
	spec := Specs["rust"]
	keywords := []string{
		"fn", "let", "mut", "const", "static", "struct", "enum", "trait",
		"impl", "pub", "use", "mod", "return", "if", "else", "for", "while",
		"loop", "match", "break", "continue", "unsafe", "ref", "self", "Self",
		"true", "false",
	}
	for _, kw := range keywords {
		assert.True(t, spec.Keywords[kw], "Rust keyword %q should be present", kw)
	}
	assert.Equal(t, "//", spec.CommentLine, "Rust CommentLine should be //")
}

// TestJavaSpecKeywords verifies the Java keyword set.
func TestJavaSpecKeywords(t *testing.T) {
	spec := Specs["java"]
	keywords := []string{
		"public", "private", "protected", "class", "interface", "extends",
		"implements", "static", "final", "void", "int", "long", "double",
		"float", "boolean", "char", "String", "return", "if", "else", "for",
		"while", "switch", "case", "break", "continue", "new", "this", "null",
		"true", "false",
	}
	for _, kw := range keywords {
		assert.True(t, spec.Keywords[kw], "Java keyword %q should be present", kw)
	}
	assert.Equal(t, "//", spec.CommentLine, "Java CommentLine should be //")
}

// TestBashSpecKeywords verifies the Bash keyword set.
func TestBashSpecKeywords(t *testing.T) {
	spec := Specs["bash"]
	keywords := []string{
		"if", "then", "else", "elif", "fi", "for", "while", "do", "done",
		"case", "esac", "function", "return", "export", "local", "echo",
		"read", "source", "exit", "true", "false",
	}
	for _, kw := range keywords {
		assert.True(t, spec.Keywords[kw], "Bash keyword %q should be present", kw)
	}
	assert.Equal(t, "#", spec.CommentLine, "Bash CommentLine should be #")
}

// TestJSONSpecKeywords verifies the JSON keyword set.
func TestJSONSpecKeywords(t *testing.T) {
	spec := Specs["json"]
	for _, kw := range []string{"true", "false", "null"} {
		assert.True(t, spec.Keywords[kw], "JSON keyword %q should be present", kw)
	}
	assert.Equal(t, "", spec.CommentLine, "JSON has no line comments")
	assert.Equal(t, [2]string{}, spec.CommentBlock, "JSON has no block comments")
	assert.Equal(t, []string{"\""}, spec.StringDelims, "JSON only uses double quotes")
}

// TestYAMLSpecKeywords verifies the YAML keyword set.
func TestYAMLSpecKeywords(t *testing.T) {
	spec := Specs["yaml"]
	keywords := []string{"true", "false", "null", "yes", "no", "on", "off"}
	for _, kw := range keywords {
		assert.True(t, spec.Keywords[kw], "YAML keyword %q should be present", kw)
	}
	assert.Equal(t, "#", spec.CommentLine, "YAML CommentLine should be #")
}

// TestSQLSpecKeywords verifies the SQL keyword set.
func TestSQLSpecKeywords(t *testing.T) {
	spec := Specs["sql"]
	keywords := []string{
		"SELECT", "FROM", "WHERE", "INSERT", "UPDATE", "DELETE", "CREATE",
		"TABLE", "DROP", "ALTER", "INDEX", "VIEW", "JOIN", "LEFT", "RIGHT",
		"INNER", "OUTER", "ON", "AS", "ORDER", "BY", "GROUP", "HAVING",
		"LIMIT", "OFFSET", "DISTINCT", "UNION", "ALL", "AND", "OR", "NOT",
		"NULL", "IN", "EXISTS", "BETWEEN", "LIKE", "CASE", "WHEN", "THEN",
		"ELSE", "END", "COUNT", "SUM", "AVG", "MIN", "MAX",
	}
	for _, kw := range keywords {
		assert.True(t, spec.Keywords[kw], "SQL keyword %q should be present", kw)
	}
	assert.Equal(t, "--", spec.CommentLine, "SQL CommentLine should be --")
	assert.True(t, spec.CaseInsensitive, "SQL should be case-insensitive")
}

// TestAliasesMapToSameSpec verifies that language aliases resolve to the same
// keyword set as the canonical name.
func TestAliasesMapToSameSpec(t *testing.T) {
	assert.Equal(t, Specs["go"].Keywords, Specs["golang"].Keywords)
	assert.Equal(t, Specs["python"].Keywords, Specs["py"].Keywords)
	assert.Equal(t, Specs["javascript"].Keywords, Specs["js"].Keywords)
	assert.Equal(t, Specs["typescript"].Keywords, Specs["ts"].Keywords)
	assert.Equal(t, Specs["rust"].Keywords, Specs["rs"].Keywords)
	assert.Equal(t, Specs["bash"].Keywords, Specs["sh"].Keywords)
	assert.Equal(t, Specs["yaml"].Keywords, Specs["yml"].Keywords)
}
