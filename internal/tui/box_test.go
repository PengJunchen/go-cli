package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBoxRendererContainsBoxDrawingChars(t *testing.T) {
	b := BoxRenderer{}
	out := b.Render(context.Background(), "hello", RenderOpts{Theme: DarkTheme{}})
	for _, ch := range []string{"┌", "┐", "└", "┘", "─", "│"} {
		assert.Contains(t, out, ch, "box output should contain %q", ch)
	}
}

func TestBoxRendererContainsContent(t *testing.T) {
	b := BoxRenderer{}
	out := b.Render(context.Background(), "hello world", RenderOpts{Theme: DarkTheme{}})
	assert.Contains(t, out, "hello world")
}

func TestBoxRendererMultiline(t *testing.T) {
	b := BoxRenderer{}
	out := b.Render(context.Background(), "line1\nline2\nline3", RenderOpts{Theme: DarkTheme{}})
	plain := stripEscape(out)
	lines := strings.Split(plain, "\n")
	// top border + 3 content lines + bottom border = 5 lines
	require.Len(t, lines, 5)
	assert.Contains(t, lines[0], "┌")
	assert.Contains(t, lines[0], "┐")
	assert.Contains(t, lines[4], "└")
	assert.Contains(t, lines[4], "┘")
	for _, want := range []string{"line1", "line2", "line3"} {
		assert.Contains(t, plain, want)
	}
}

func TestBoxRendererPadding(t *testing.T) {
	b := BoxRenderer{}
	out := b.Render(context.Background(), "x", RenderOpts{Theme: DarkTheme{}})
	plain := stripEscape(out)
	// Each content line has 1-space padding on each side: "│ x │"
	lines := strings.Split(plain, "\n")
	require.Contains(t, lines[1], "│ x │")
}

func TestBoxRendererImplementsRenderer(t *testing.T) {
	var _ Renderer = BoxRenderer{}
}

func TestBoxRendererNameAndSupports(t *testing.T) {
	b := BoxRenderer{}
	assert.Equal(t, ContentTypeBox, b.Name())
	assert.True(t, b.Supports(ContentTypeBox))
	assert.False(t, b.Supports(ContentTypeMarkdown))
}

func TestBoxRendererContentType(t *testing.T) {
	b := BoxRenderer{}
	assert.Equal(t, ContentTypeBox, b.ContentType())
}

func TestBoxRendererEmptyContent(t *testing.T) {
	b := BoxRenderer{}
	out := b.Render(context.Background(), "", RenderOpts{Theme: DarkTheme{}})
	// Should not panic and should still contain border characters.
	assert.Contains(t, out, "┌")
	assert.Contains(t, out, "└")
}
