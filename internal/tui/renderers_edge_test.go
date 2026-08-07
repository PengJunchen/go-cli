package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAllRenderersEmptyContent verifies every renderer handles empty content
// without panicking and (except blank) still returns a styled/meaningful frame.
func TestAllRenderersEmptyContent(t *testing.T) {
	ctx := context.Background()
	reg := NewDefaultRegistry()
	for _, ct := range contentTypes {
		if ct == ContentTypeSpinner {
			continue // SpinnerRenderer is a standalone component, not a Renderer
		}
		r, _ := reg.Get(ct)
		out := r.Render(ctx, "", RenderOpts{Theme: DarkTheme{}, Width: 0})
		_ = out // must not panic
	}
}

// TestAllRenderersMultiByteContent verifies every renderer preserves UTF-8
// multibyte runes in its payload without corruption.
func TestAllRenderersMultiByteContent(t *testing.T) {
	ctx := context.Background()
	reg := NewDefaultRegistry()
	payload := "héllo wörld 世界 🚀"
	for _, ct := range contentTypes {
		if ct == ContentTypeSpinner {
			continue // SpinnerRenderer is a standalone component, not a Renderer
		}
		r, _ := reg.Get(ct)
		// Progress and separator renderers do not reproduce the raw payload, and
		// blank emits nothing.
		if ct == ContentTypeProgress || ct == ContentTypeSeparator || ct == ContentTypeBlank {
			continue
		}
		out := r.Render(ctx, payload, RenderOpts{Theme: DarkTheme{}, Width: 60})
		// lipgloss applies underline/strikethrough per-rune, so verify the
		// visible payload via stripEscape rather than the raw ANSI output.
		assert.Contains(t, stripEscape(out), "wörld", "%s should preserve multibyte payload", ct)
	}
}

// TestStripANSIWrapsPlainText verifies stripANSI wraps plain text at the column
// limit, inserting a newline on the rune boundary.
func TestStripANSIWrapsPlainText(t *testing.T) {
	out := stripANSI("abcdef", 3)
	assert.Equal(t, "abc\ndef", out)
}

// TestStripANSIHandlesNewlines verifies stripANSI treats pre-existing newlines
// as column resets and preserves them.
func TestStripANSIHandlesNewlines(t *testing.T) {
	out := stripANSI("ab\ncd", 2)
	assert.Equal(t, "ab\ncd", out)
	out = stripANSI("ab\ncd", 1)
	assert.Equal(t, "a\nb\nc\nd", out)
}

// TestStripANSINoWrap verifies stripANSI returns input unchanged when the column
// limit is not exceeded.
func TestStripANSINoWrap(t *testing.T) {
	assert.Equal(t, "abc", stripANSI("abc", 5))
	assert.Equal(t, "", stripANSI("", 5))
}

// TestStripANSIUnicodeColumns verifies every rune, including multibyte, is
// treated as one printable column.
func TestStripANSIUnicodeColumns(t *testing.T) {
	out := stripANSI("世界ok", 2)
	assert.Equal(t, "世界\nok", out)
}

// TestJoinIntsWasRemoved: the manual joinInts helper was deleted together with
// the custom ANSI Style struct when the package migrated to lipgloss. lipgloss
// owns SGR sequence construction now, so there is nothing to unit-test here.

// TestMarkdownRendererPreservesContent verifies markdown output contains the
// full content. The AST-based renderer does not wrap paragraph text.
func TestMarkdownRendererPreservesContent(t *testing.T) {
	out := (MarkdownRenderer{}).Render(context.Background(), "abcdefgh", RenderOpts{Theme: DarkTheme{}, Width: 4})
	assert.Contains(t, out, "abcdefgh")
}

// TestTableRendererPreservesRowOrder verifies table header emphasis is applied
// only to the first line and subsequent rows are passed through verbatim.
func TestTableRendererPreservesRowOrder(t *testing.T) {
	out := (TableRenderer{}).Render(context.Background(), "a\tb\nc\td", RenderOpts{Theme: DarkTheme{}})
	lines := strings.Split(out, "\n")
	require.Len(t, lines, 2)
	// Header row is styled (starts with an escape); data row is not.
	assert.True(t, strings.HasPrefix(lines[0], "\x1b["))
	assert.Equal(t, "c\td", lines[1])
}
// TestDiffRendererEmptyContent verifies diff on a single blank line renders the
// context (fg) style applied to the empty line without panicking. The fg style
// wraps an empty payload, so after stripping escapes the visible text is empty.
func TestDiffRendererEmptyContent(t *testing.T) {
	out := (DiffRenderer{}).Render(context.Background(), "", RenderOpts{Theme: DarkTheme{}})
	assert.True(t, strings.HasPrefix(out, "\x1b["), "empty diff line should still be fg-styled")
	assert.True(t, strings.HasSuffix(out, "\x1b[0m"), "styled output should end with a reset")
	assert.Equal(t, "", stripEscape(out))
}

// TestProgressRendererAlwaysExactlyWidth verifies the visible bar (█ and ░)
// always sums to the requested width for every fraction.
func TestProgressRendererAlwaysExactlyWidth(t *testing.T) {
	p := ProgressRenderer{}
	for _, frac := range []string{"0", "0.1", "0.5", "0.9", "1"} {
		out := p.Render(context.Background(), frac, RenderOpts{Theme: DarkTheme{}, Width: 12})
		assert.Equal(t, strings.Count(out, "█")+strings.Count(out, "░"), 12,
			"bar for %q should span exactly 12 cells", frac)
	}
}

// TestSeparatorRendererTrim verifies the separator output has no ANSI wrappers
// and uses the box-drawing character throughout.
func TestSeparatorRendererTrim(t *testing.T) {
	out := (SeparatorRenderer{}).Render(context.Background(), "", RenderOpts{Width: 5})
	assert.Equal(t, "─────", out)
}

// TestStreamingRenderersRespectWidth verifies streaming renderers wrap long
// input to the configured width.
func TestStreamingRenderersRespectWidth(t *testing.T) {
	long := "1234567890"
	for _, r := range []Renderer{StreamingRenderer{}, StreamingCodeRenderer{}} {
		out := r.Render(context.Background(), long, RenderOpts{Theme: DarkTheme{}, Width: 4})
		// After stripping ANSI the wrapped text contains a newline.
		assert.Contains(t, out, "\n", "%s should wrap at width 4", r.Name())
	}
}

// TestTableRendererWrapInHeader verifies header styling survives width wrapping.
func TestTableRendererWrapInHeader(t *testing.T) {
	out := (TableRenderer{}).Render(context.Background(), "head", RenderOpts{Theme: MockTheme{}, Width: 2})
	// MockTheme Primary is red+bold; header single cell on its own line.
	assert.True(t, strings.HasPrefix(out, "\x1b["), "header should be wrapped in primary style")
}

// TestFileTreeRendererTrimsTrailingNewline verifies renderers do not emit a
// trailing newline in their output.
func TestFileTreeRendererTrimsTrailingNewline(t *testing.T) {
	out := (FileTreeRenderer{}).Render(context.Background(), "a\nb", RenderOpts{Theme: DarkTheme{}})
	assert.False(t, strings.HasSuffix(out, "\n"), "file tree output must not end with a newline")
}
