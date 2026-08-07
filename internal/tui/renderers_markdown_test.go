package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/pengjunchen/go-cli/internal/tui/markdown"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Compile-time assertion that themeAdapter satisfies markdown.ThemeAdapter.
var _ markdown.ThemeAdapter = themeAdapter{}

// mockMarkdownHighlighter is a test double for CodeHighlighter that records
// its calls and wraps the output in markers so tests can verify the
// highlighter was invoked through the AST renderer.
type mockMarkdownHighlighter struct {
	called bool
	code   string
	lang   string
}

func (m *mockMarkdownHighlighter) Highlight(code, lang string) string {
	m.called = true
	m.code = code
	m.lang = lang
	return "[HL]" + code + "[/HL]"
}

// TestMarkdownRendererASTHeadingProducesBold verifies that the AST-based
// MarkdownRenderer applies bold ANSI styling to headings.
func TestMarkdownRendererASTHeadingProducesBold(t *testing.T) {
	r := NewMarkdownRenderer(NewDefaultCodeHighlighter())
	out := r.Render(context.Background(), "# Heading", RenderOpts{Theme: DarkTheme{}, Width: 80})
	assert.Contains(t, out, "\x1b[1m", "heading should be rendered with bold ANSI code")
	assert.Contains(t, out, "# Heading", "heading text should be preserved")
}

// TestMarkdownRendererASTHeadingLevel1HasSeparator verifies that level-1
// headings include an underline separator line.
func TestMarkdownRendererASTHeadingLevel1HasSeparator(t *testing.T) {
	r := NewMarkdownRenderer(NewDefaultCodeHighlighter())
	out := r.Render(context.Background(), "# Title", RenderOpts{Theme: DarkTheme{}, Width: 80})
	lines := strings.Split(out, "\n")
	require.GreaterOrEqual(t, len(lines), 2, "level-1 heading should have a separator line")
	assert.True(t, strings.Trim(lines[len(lines)-1], "─") == "", "separator should be all ─")
}

// TestMarkdownRendererASTCodeBlockInvokesHighlighter verifies that the
// MarkdownRenderer passes fenced code blocks through the CodeHighlighter and
// the highlighter output appears in the rendered string.
func TestMarkdownRendererASTCodeBlockInvokesHighlighter(t *testing.T) {
	hl := &mockMarkdownHighlighter{}
	r := NewMarkdownRenderer(hl)
	out := r.Render(context.Background(), "```go\nfmt.Println(\"hi\")\n```",
		RenderOpts{Theme: DarkTheme{}, Width: 80})

	require.True(t, hl.called, "highlighter should have been called")
	assert.Equal(t, "go", hl.lang, "highlighter should receive the correct language")
	assert.Equal(t, "fmt.Println(\"hi\")", hl.code, "highlighter should receive the code text")
	assert.Contains(t, out, "[HL]", "output should contain highlighted code marker")
	// Code block lines should be indented with 2 spaces.
	for _, line := range strings.Split(out, "\n") {
		assert.True(t, strings.HasPrefix(line, "  "),
			"code block line should be indented with 2 spaces: %q", line)
	}
}

// TestMarkdownRendererASTNilHighlighterUsesDefault verifies that a
// MarkdownRenderer with a nil highlighter falls back to the default
// highlighter and still renders code blocks without panicking.
func TestMarkdownRendererASTNilHighlighterUsesDefault(t *testing.T) {
	r := NewMarkdownRenderer(nil)
	out := r.Render(context.Background(), "```go\nfmt.Println(\"hi\")\n```",
		RenderOpts{Theme: DarkTheme{}, Width: 80})

	// The default highlighter returns code unchanged in non-TTY environments.
	// In TTY environments it may apply syntax highlighting, so strip ANSI codes
	// before checking for the code text.
	plain := stripEscape(out)
	assert.Contains(t, plain, "fmt.Println", "code block should contain the code text")
	// Code block lines should be indented with 2 spaces (indent is added after
	// highlighting, so it is always present).
	for _, line := range strings.Split(out, "\n") {
		assert.True(t, strings.HasPrefix(line, "  "),
			"code block line should be indented with 2 spaces: %q", line)
	}
}

// TestMarkdownRendererASTInlineBold verifies that inline bold markdown is
// rendered with bold ANSI codes through the full parse-render pipeline.
func TestMarkdownRendererASTInlineBold(t *testing.T) {
	r := NewMarkdownRenderer(nil)
	out := r.Render(context.Background(), "This is **bold** text",
		RenderOpts{Theme: DarkTheme{}, Width: 80})
	assert.Contains(t, out, "\x1b[1m", "inline bold should produce bold ANSI code")
	assert.Contains(t, out, "bold", "bold text content should be preserved")
}

// TestMarkdownRendererASTStrikethrough verifies that strikethrough markdown
// produces the strikethrough ANSI code (9).
func TestMarkdownRendererASTStrikethrough(t *testing.T) {
	r := NewMarkdownRenderer(nil)
	out := r.Render(context.Background(), "~~deleted~~",
		RenderOpts{Theme: DarkTheme{}, Width: 80})
	assert.Contains(t, out, "\x1b[9m", "strikethrough should produce ANSI code 9")
	// lipgloss applies strikethrough per-rune, fragmenting the raw output, so
	// verify the visible payload via stripEscape.
	assert.Contains(t, stripEscape(out), "deleted", "strikethrough text should be preserved")
}

// TestThemeAdapterDelegatesToTheme verifies that themeAdapter correctly
// delegates each method to the wrapped Theme, producing the same styled
// output as calling the Theme accessor and Render directly.
func TestThemeAdapterDelegatesToTheme(t *testing.T) {
	adapter := themeAdapter{theme: DarkTheme{}}
	theme := DarkTheme{}

	// Methods that delegate to Theme accessors.
	assert.Equal(t, theme.Bold().Render("x"), adapter.Bold("x"))
	assert.Equal(t, theme.Italic().Render("x"), adapter.Italic("x"))
	assert.Equal(t, theme.Faint().Render("x"), adapter.Faint("x"))
	assert.Equal(t, theme.Primary().Render("x"), adapter.Primary("x"))
	assert.Equal(t, theme.Secondary().Render("x"), adapter.Secondary("x"))
	assert.Equal(t, theme.Error().Render("x"), adapter.Error("x"))

	// Underline and Strikethrough use NewStyle() directly.
	assert.Equal(t, NewStyle().Underline(true).Render("x"), adapter.Underline("x"))
	assert.Equal(t, NewStyle().Strikethrough(true).Render("x"), adapter.Strikethrough("x"))
}

// TestThemeAdapterProducesExpectedANSICodes verifies that each themeAdapter
// method emits the expected SGR ANSI code for a DarkTheme under the forced
// truecolor profile. Bold/italic/underline/strikethrough keep their standard
// SGR codes (1/3/4/9), while colors render as 24-bit sequences.
func TestThemeAdapterProducesExpectedANSICodes(t *testing.T) {
	adapter := themeAdapter{theme: DarkTheme{}}

	assert.Contains(t, adapter.Bold("x"), "\x1b[1m", "Bold should emit code 1")
	assert.Contains(t, adapter.Italic("x"), "\x1b[3m", "Italic should emit code 3")
	assert.Contains(t, adapter.Underline("x"), "\x1b[4;4m", "Underline should emit code 4")
	assert.Contains(t, adapter.Strikethrough("x"), "\x1b[9m", "Strikethrough should emit code 9")
	// DarkTheme.Primary = #7D56F4 (truecolor; termenv quantizes 0xF4 -> 243).
	assert.Contains(t, adapter.Primary("x"), "38;2;125;86;243", "Primary should emit purple truecolor")
	// DarkTheme.Secondary = #6C7086 (truecolor).
	assert.Contains(t, adapter.Secondary("x"), "38;2;108;112;134", "Secondary should emit muted truecolor")
	// DarkTheme.Error = #FF5C5C (truecolor).
	assert.Contains(t, adapter.Error("x"), "38;2;255;92;92", "Error should emit red truecolor")
	// DarkTheme.Faint = #6C7086 + faint (2).
	assert.Contains(t, adapter.Faint("x"), "2;38;2;108;112;134", "Faint should emit muted + faint")
}

// TestThemeAdapterWithMockTheme verifies themeAdapter works with any Theme
// implementation, not just DarkTheme.
func TestThemeAdapterWithMockTheme(t *testing.T) {
	adapter := themeAdapter{theme: MockTheme{}}
	theme := MockTheme{}

	assert.Equal(t, theme.Bold().Render("x"), adapter.Bold("x"))
	assert.Equal(t, theme.Primary().Render("x"), adapter.Primary("x"))
	assert.Equal(t, theme.Secondary().Render("x"), adapter.Secondary("x"))
	assert.Equal(t, theme.Error().Render("x"), adapter.Error("x"))
	// MockTheme.Primary = #FF0000 + bold (1).
	assert.Contains(t, adapter.Primary("x"), "1;38;2;255;0;0")
}
