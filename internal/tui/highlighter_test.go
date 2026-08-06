package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHighlightGoCodeKeywords verifies that Go keywords receive blue ANSI
// coloring and non-keyword identifiers are left uncolored.
func TestHighlightGoCodeKeywords(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "func main() { return }"
	result := h.highlightCode(code, "go")

	// "func" and "return" are Go keywords — should be blue.
	assert.Contains(t, result, hlBlue+"func"+hlReset, "func should be colored blue")
	assert.Contains(t, result, hlBlue+"return"+hlReset, "return should be colored blue")
	// "main" is not a keyword — should NOT be blue.
	assert.NotContains(t, result, hlBlue+"main"+hlReset, "main should not be colored blue")
}

// TestHighlightGoCodeStrings verifies that string literals are green.
func TestHighlightGoCodeStrings(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := `s := "hello world"`
	result := h.highlightCode(code, "go")

	assert.Contains(t, result, hlGreen+`"hello world"`+hlReset, "string literal should be green")
}

// TestHighlightGoCodeNumbers verifies that numeric literals are yellow.
func TestHighlightGoCodeNumbers(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "x := 42"
	result := h.highlightCode(code, "go")

	assert.Contains(t, result, hlYellow+"42"+hlReset, "number should be yellow")
}

// TestHighlightGoCodeComments verifies that // comments are gray.
func TestHighlightGoCodeComments(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "// this is a comment\nfunc main() {}"
	result := h.highlightCode(code, "go")

	lines := strings.Split(result, "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], hlGray, "comment line should be gray")
	assert.Contains(t, lines[0], "this is a comment")
}

// TestHighlightGoCodeInlineComment verifies inline // comments are gray.
func TestHighlightGoCodeInlineComment(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "x := 1 // inline comment"
	result := h.highlightCode(code, "go")

	assert.Contains(t, result, hlGray+"// inline comment"+hlReset, "inline comment should be gray")
}

// TestHighlightGoBlockComment verifies multi-line block comments are gray.
func TestHighlightGoBlockComment(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "/* line one\nline two */\nfunc main()"
	result := h.highlightCode(code, "go")

	lines := strings.Split(result, "\n")
	require.Len(t, lines, 3)
	assert.Contains(t, lines[0], hlGray, "first line of block comment should be gray")
	assert.Contains(t, lines[1], hlGray, "second line of block comment should be gray")
}

// TestHighlightPythonComments verifies that # comments are gray in Python.
func TestHighlightPythonComments(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "# python comment\nx = 1"
	result := h.highlightCode(code, "python")

	lines := strings.Split(result, "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], hlGray, "Python comment should be gray")
	assert.Contains(t, lines[0], "python comment")
}

// TestHighlightPythonKeywords verifies Python keywords are blue.
func TestHighlightPythonKeywords(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "def main():\n    return None"
	result := h.highlightCode(code, "python")

	assert.Contains(t, result, hlBlue+"def"+hlReset, "def should be blue")
	assert.Contains(t, result, hlBlue+"return"+hlReset, "return should be blue")
	assert.Contains(t, result, hlBlue+"None"+hlReset, "None should be blue")
}

// TestHighlightJavaScriptComments verifies that // comments are gray in JS.
func TestHighlightJavaScriptComments(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "// js comment\nconst x = 1"
	result := h.highlightCode(code, "javascript")

	lines := strings.Split(result, "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], hlGray, "JS comment should be gray")
}

// TestHighlightJavaScriptKeywords verifies JS keywords are blue.
func TestHighlightJavaScriptKeywords(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "async function fetch() { return await x }"
	result := h.highlightCode(code, "javascript")

	assert.Contains(t, result, hlBlue+"async"+hlReset)
	assert.Contains(t, result, hlBlue+"function"+hlReset)
	assert.Contains(t, result, hlBlue+"return"+hlReset)
	assert.Contains(t, result, hlBlue+"await"+hlReset)
}

// TestHighlightBashComments verifies that # comments are gray in Bash.
func TestHighlightBashComments(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "# bash comment\necho hello"
	result := h.highlightCode(code, "bash")

	lines := strings.Split(result, "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], hlGray, "Bash comment should be gray")
}

// TestHighlightBashKeywords verifies Bash keywords are blue.
func TestHighlightBashKeywords(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "if true; then echo hi; fi"
	result := h.highlightCode(code, "bash")

	assert.Contains(t, result, hlBlue+"if"+hlReset)
	assert.Contains(t, result, hlBlue+"then"+hlReset)
	assert.Contains(t, result, hlBlue+"fi"+hlReset)
}

// TestHighlightSQLCaseInsensitive verifies SQL keywords are colored regardless
// of case.
func TestHighlightSQLCaseInsensitive(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	// Uppercase SELECT.
	code1 := "SELECT * FROM users"
	result1 := h.highlightCode(code1, "sql")
	assert.Contains(t, result1, hlBlue+"SELECT"+hlReset, "uppercase SELECT should be blue")
	assert.Contains(t, result1, hlBlue+"FROM"+hlReset, "uppercase FROM should be blue")

	// Lowercase select.
	code2 := "select * from users"
	result2 := h.highlightCode(code2, "sql")
	assert.Contains(t, result2, hlBlue+"select"+hlReset, "lowercase select should be blue")
	assert.Contains(t, result2, hlBlue+"from"+hlReset, "lowercase from should be blue")

	// Mixed case.
	code3 := "Select * From users"
	result3 := h.highlightCode(code3, "sql")
	assert.Contains(t, result3, hlBlue+"Select"+hlReset, "mixed-case Select should be blue")
}

// TestHighlightSQLComments verifies that -- comments are gray in SQL.
func TestHighlightSQLComments(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "-- sql comment\nSELECT 1"
	result := h.highlightCode(code, "sql")

	lines := strings.Split(result, "\n")
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], hlGray, "SQL comment should be gray")
}

// TestHighlightRustKeywords verifies Rust keywords are blue.
func TestHighlightRustKeywords(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "fn main() { let x = 5; }"
	result := h.highlightCode(code, "rust")

	assert.Contains(t, result, hlBlue+"fn"+hlReset)
	assert.Contains(t, result, hlBlue+"let"+hlReset)
}

// TestHighlightJavaKeywords verifies Java keywords are blue.
func TestHighlightJavaKeywords(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "public class Main { }"
	result := h.highlightCode(code, "java")

	assert.Contains(t, result, hlBlue+"public"+hlReset)
	assert.Contains(t, result, hlBlue+"class"+hlReset)
}

// TestHighlightJSONKeywords verifies JSON keywords are blue.
func TestHighlightJSONKeywords(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := `{"active": true, "data": null}`
	result := h.highlightCode(code, "json")

	assert.Contains(t, result, hlBlue+"true"+hlReset)
	assert.Contains(t, result, hlBlue+"null"+hlReset)
}

// TestHighlightYAMLKeywords verifies YAML keywords are blue.
func TestHighlightYAMLKeywords(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "enabled: true\nname: null"
	result := h.highlightCode(code, "yaml")

	assert.Contains(t, result, hlBlue+"true"+hlReset)
	assert.Contains(t, result, hlBlue+"null"+hlReset)
}

// TestHighlightTypeScriptKeywords verifies TS-specific keywords are blue.
func TestHighlightTypeScriptKeywords(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "interface Foo { x: number }"
	result := h.highlightCode(code, "typescript")

	assert.Contains(t, result, hlBlue+"interface"+hlReset, "TS interface should be blue")
}

// TestHighlightUnknownLanguageFallback verifies that unknown languages fall
// back to commonKeywords.
func TestHighlightUnknownLanguageFallback(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "func main() { return }"
	result := h.highlightCode(code, "unknownlang")

	// commonKeywords includes func and return.
	assert.Contains(t, result, hlBlue+"func"+hlReset, "func should be colored via fallback")
	assert.Contains(t, result, hlBlue+"return"+hlReset, "return should be colored via fallback")
}

// TestHighlightUnknownLanguageComments verifies fallback comment handling
// (both // and #).
func TestHighlightUnknownLanguageComments(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	// // comment in fallback.
	code1 := "// c style comment\nx = 1"
	result1 := h.highlightCode(code1, "unknownlang")
	lines1 := strings.Split(result1, "\n")
	require.Len(t, lines1, 2)
	assert.Contains(t, lines1[0], hlGray, "// comment should be gray in fallback")

	// # comment in fallback.
	code2 := "# hash comment\nx = 1"
	result2 := h.highlightCode(code2, "unknownlang")
	lines2 := strings.Split(result2, "\n")
	require.Len(t, lines2, 2)
	assert.Contains(t, lines2[0], hlGray, "# comment should be gray in fallback")
}

// TestHighlightNonTTYReturnsRawCode verifies that Highlight returns code
// unchanged when stdout is not a TTY.
func TestHighlightNonTTYReturnsRawCode(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "func main() { return }"
	result := h.Highlight(code, "go")

	if isStdoutTerminal() {
		t.Skip("stdout is a TTY; non-TTY behavior cannot be tested")
	}

	assert.Equal(t, code, result, "non-TTY Highlight should return raw code")
	assert.NotContains(t, result, "\033[", "non-TTY output should not contain ANSI codes")
}

// TestHighlightMarkdownNonTTY verifies that HighlightMarkdown returns text
// unchanged when stdout is not a TTY.
func TestHighlightMarkdownNonTTY(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	text := "Some text\n```go\nfunc main() {}\n```\nMore text"
	result := h.HighlightMarkdown(text)

	if isStdoutTerminal() {
		t.Skip("stdout is a TTY; non-TTY behavior cannot be tested")
	}

	assert.Equal(t, text, result, "non-TTY HighlightMarkdown should return raw text")
}

// TestHighlightLanguageCaseInsensitive verifies language name matching is
// case-insensitive (e.g. "Go" and "GO" resolve to the Go spec).
func TestHighlightLanguageCaseInsensitive(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	code := "func main() {}"

	for _, lang := range []string{"go", "Go", "GO"} {
		result := h.highlightCode(code, lang)
		assert.Contains(t, result, hlBlue+"func"+hlReset, "language %q should highlight func", lang)
	}
}

// TestHighlightEmptyCode verifies that empty input produces empty output.
func TestHighlightEmptyCode(t *testing.T) {
	h := NewDefaultCodeHighlighter()
	result := h.highlightCode("", "go")
	assert.Equal(t, "", result)
}
