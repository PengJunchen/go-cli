package tui

import (
	"context"
	"strings"
	"unicode/utf8"
)

// BoxRenderer draws a box border around content using Unicode box-drawing
// characters (┌┐└┘─│). The border is styled with the theme secondary color and
// the content is padded with one space on each side.
type BoxRenderer struct{}

var _ Renderer = (*BoxRenderer)(nil)

// ContentType returns the content type identifier for box content.
func (BoxRenderer) ContentType() string { return ContentTypeBox }

// Render wraps content in a box border. Multi-line content produces a box with
// one row per line; each row is padded to the width of the longest line.
func (BoxRenderer) Render(ctx context.Context, content string, opts RenderOpts) string {
	theme := renderTheme(opts)
	lines := strings.Split(content, "\n")

	maxLen := 0
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > maxLen {
			maxLen = n
		}
	}

	horizontal := strings.Repeat("─", maxLen+2) // +2 for 1-space padding each side

	var sb strings.Builder
	sb.WriteString(theme.Secondary().Render("┌" + horizontal + "┐"))
	sb.WriteString("\n")
	for _, line := range lines {
		pad := strings.Repeat(" ", maxLen-utf8.RuneCountInString(line))
		sb.WriteString(theme.Secondary().Render("│") + " " + line + pad + " " + theme.Secondary().Render("│"))
		sb.WriteString("\n")
	}
	sb.WriteString(theme.Secondary().Render("└" + horizontal + "┘"))

	out := sb.String()
	logRender(ctx, "box", opts.ContentType, len(out))
	return out
}

// Name identifies the renderer.
func (BoxRenderer) Name() string { return ContentTypeBox }

// Supports reports whether the renderer handles the content type.
func (BoxRenderer) Supports(ct string) bool { return ct == ContentTypeBox }
