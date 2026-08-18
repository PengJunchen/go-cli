package tui

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarkdownRenderer_NoHighlighterArg verifies that NewMarkdownRenderer
// takes no arguments (glamour/chroma owns syntax highlighting now) and returns
// a usable renderer.
func TestMarkdownRenderer_NoHighlighterArg(t *testing.T) {
	r := NewMarkdownRenderer()
	require.NotNil(t, r)
	assert.Equal(t, "markdown", r.Name())
	assert.True(t, r.Supports(ContentTypeMarkdown))
	assert.False(t, r.Supports(ContentTypeCode))
}

// TestMarkdownRenderer_Headings verifies a heading renders to non-empty output
// containing the heading text. Glamour renders headings without the leading
// "#", so we assert the text survives rather than the marker.
func TestMarkdownRenderer_Headings(t *testing.T) {
	r := NewMarkdownRenderer()
	out := r.Render(context.Background(), "# Title", RenderOpts{Theme: DarkTheme{}, Width: 80})
	assert.NotEmpty(t, out)
	assert.Contains(t, stripEscape(out), "Title")
}

// TestMarkdownRenderer_CodeBlock verifies a fenced Go code block renders to
// non-empty output containing the code text. Chroma (via glamour) applies the
// coloring; we only assert content presence since exact ANSI codes are
// profile/chroma-version dependent.
func TestMarkdownRenderer_CodeBlock(t *testing.T) {
	r := NewMarkdownRenderer()
	out := r.Render(context.Background(), "```go\nfmt.Println(\"hi\")\n```",
		RenderOpts{Theme: DarkTheme{}, Width: 80})
	assert.NotEmpty(t, out)
	assert.Contains(t, stripEscape(out), "Println")
}

// TestMarkdownRenderer_Table verifies a GFM table renders to non-empty output
// containing the table contents and a box-drawing separator.
func TestMarkdownRenderer_Table(t *testing.T) {
	r := NewMarkdownRenderer()
	out := r.Render(context.Background(),
		"| Name | Value |\n|------|-------|\n| A | 1 |",
		RenderOpts{Theme: DarkTheme{}, Width: 80})
	assert.NotEmpty(t, out)
	plain := stripEscape(out)
	assert.Contains(t, plain, "Name")
	assert.Contains(t, plain, "Value")
	// Glamour renders GFM tables with a column separator.
	assert.Contains(t, plain, "│")
}

// TestMarkdownRenderer_List verifies an unordered list renders both items.
func TestMarkdownRenderer_List(t *testing.T) {
	r := NewMarkdownRenderer()
	out := r.Render(context.Background(), "- a\n- b", RenderOpts{Theme: DarkTheme{}, Width: 80})
	plain := stripEscape(out)
	assert.Contains(t, plain, "a")
	assert.Contains(t, plain, "b")
}

// TestMarkdownRenderer_FallbackOnError verifies edge-case content (empty,
// whitespace-only) does not panic. Glamour returns empty for empty input; the
// renderer falls back to the raw content on a render error, so callers never
// receive a partial/crashed result.
func TestMarkdownRenderer_FallbackOnError(t *testing.T) {
	r := NewMarkdownRenderer()
	for _, in := range []string{"", "   ", "\n\n", "\t"} {
		assert.NotPanics(t, func() {
			_ = r.Render(context.Background(), in, RenderOpts{Theme: DarkTheme{}, Width: 80})
		}, "rendering %q must not panic", in)
	}
	// Empty input yields empty (glamour) or the raw content (fallback); both are
	// acceptable as long as no panic occurs.
	out := r.Render(context.Background(), "", RenderOpts{Theme: DarkTheme{}, Width: 80})
	_ = out
}

// TestMarkdownRenderer_WidthCached verifies the cached glamour renderer is
// reused when style and width are unchanged (no error/panic on repeat calls),
// and that a width change still produces valid output.
func TestMarkdownRenderer_WidthCached(t *testing.T) {
	r := NewMarkdownRenderer()
	ctx := context.Background()
	out1 := r.Render(ctx, "# Hello", RenderOpts{Theme: DarkTheme{}, Width: 80})
	out2 := r.Render(ctx, "# Hello", RenderOpts{Theme: DarkTheme{}, Width: 80})
	assert.Equal(t, out1, out2, "stable opts should reuse the cached renderer")
	out3 := r.Render(ctx, "# Hello", RenderOpts{Theme: DarkTheme{}, Width: 40})
	assert.NotEmpty(t, out3)
}

// TestMarkdownRenderer_LightThemeStyle verifies a LightTheme maps to the glamour
// "light" style and still renders content without panicking.
func TestMarkdownRenderer_LightThemeStyle(t *testing.T) {
	r := NewMarkdownRenderer()
	out := r.Render(context.Background(), "# Hello", RenderOpts{Theme: LightTheme{}, Width: 80})
	assert.NotEmpty(t, out)
	assert.Contains(t, stripEscape(out), "Hello")
}

// TestRegisterDefaultRenderers_NoHighlighterArg verifies the default registry
// registers a markdown renderer without needing a highlighter argument.
func TestRegisterDefaultRenderers_NoHighlighterArg(t *testing.T) {
	reg := NewDefaultRegistry()
	r, ok := reg.Get(ContentTypeMarkdown)
	require.True(t, ok)
	require.NotNil(t, r)
	assert.Equal(t, "markdown", r.Name())
}

// TestMarkdownDeleted_FilesRemoved verifies the self-implemented markdown and
// highlighter packages/files were deleted as part of the glamour migration.
func TestMarkdownDeleted_FilesRemoved(t *testing.T) {
	for _, p := range []string{
		"internal/tui/markdown/parser.go",
		"internal/tui/highlighter.go",
		"internal/tui/highlighters/specs.go",
	} {
		_, err := os.Stat(p)
		assert.True(t, os.IsNotExist(err), "deleted file should not exist: %s", p)
	}
}
