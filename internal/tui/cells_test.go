package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/require"
)

func TestWrapWidthUsesCellAndGraphemeBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		input string
		width int
		want  string
	}{
		{name: "CJK fullwidth", input: "世界abc", width: 4, want: "世界\nabc"},
		{name: "combining mark", input: "e\u0301xy", width: 1, want: "e\u0301\nx\ny"},
		{name: "variation selector", input: "✈️x", width: 2, want: "✈️\nx"},
		{name: "skin tone modifier", input: "👍🏽x", width: 2, want: "👍🏽\nx"},
		{name: "ZWJ emoji", input: "👨‍👩‍👧‍👦x", width: 2, want: "👨‍👩‍👧‍👦\nx"},
		{name: "flag", input: "🇺🇸x", width: 2, want: "🇺🇸\nx"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, wrapWidth(tc.input, tc.width))
		})
	}
}

func TestCellWidthAndClipUnicodeAndANSI(t *testing.T) {
	linkOpen := ansi.SetHyperlink("https://example.test")
	linkClose := ansi.ResetHyperlink()

	cases := []struct {
		name     string
		input    string
		width    int
		want     string
		wantCell int
	}{
		{name: "empty", input: "", width: 5, want: "", wantCell: 0},
		{name: "zero columns", input: "abc", width: 0, want: "", wantCell: 0},
		{name: "ASCII", input: "hello", width: 3, want: "hel", wantCell: 3},
		{name: "CJK fullwidth", input: "世界abc", width: 4, want: "世界", wantCell: 4},
		{name: "combining mark stays with base", input: "e\u0301x", width: 1, want: "e\u0301", wantCell: 1},
		{name: "variation selector stays with base", input: "✈️x", width: 2, want: "✈️", wantCell: 2},
		{name: "skin tone modifier stays with emoji", input: "👍🏽x", width: 2, want: "👍🏽", wantCell: 2},
		{name: "ZWJ emoji stays together", input: "👨‍👩‍👧‍👦x", width: 2, want: "👨‍👩‍👧‍👦", wantCell: 2},
		{name: "flag stays together", input: "🇺🇸x", width: 2, want: "🇺🇸", wantCell: 2},
		{name: "wide cluster never half fits", input: "界x", width: 1, want: "", wantCell: 0},
		{name: "SGR occupies no cells and closes after clip", input: "\x1b[31mab界\x1b[0m", width: 2, want: "\x1b[31mab\x1b[0m", wantCell: 2},
		{name: "OSC 8 occupies no cells and closes after clip", input: linkOpen + "go界" + linkClose, width: 2, want: linkOpen + "go" + linkClose, wantCell: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := clipCells(tc.input, tc.width)
			require.Equal(t, tc.want, got)
			require.Equal(t, tc.wantCell, cellWidth(got))
		})
	}

	require.Equal(t, 4, cellWidth("\x1b[31m世界\x1b[0m"))
	require.Equal(t, 2, cellWidth(linkOpen+"go"+linkClose))
}

func TestTabsExpandAtEightCellStops(t *testing.T) {
	linkOpen := ansi.SetHyperlink("https://example.test")
	linkClose := ansi.ResetHyperlink()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "ASCII column", input: "A\tB", want: "A       B"},
		{name: "CJK column", input: "界\tX", want: "界      X"},
		{name: "ANSI style is zero width", input: "\x1b[31mA\tB\x1b[0m", want: "\x1b[31mA       B\x1b[0m"},
		{name: "ANSI style around CJK is zero width", input: "\x1b[31m界\tX\x1b[0m", want: "\x1b[31m界      X\x1b[0m"},
		{name: "ANSI hyperlink is zero width", input: linkOpen + "界\tX" + linkClose, want: linkOpen + "界      X" + linkClose},
		{name: "newline resets tab stop", input: "界\n\tX", want: "界\n        X"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeTerminalText(tc.input)
			require.Equal(t, tc.want, got)
			require.NotContains(t, got, "\t")
		})
	}

	wrapped := wrapWidth("界\tX", 8)
	require.Equal(t, "界      \nX", wrapped)
	for _, line := range strings.Split(wrapped, "\n") {
		require.LessOrEqual(t, cellWidth(line), 8)
	}
}

func TestWrapWidthPreservesAndClosesAllowedANSI(t *testing.T) {
	linkOpen := ansi.SetHyperlink("https://example.test")
	linkClose := ansi.ResetHyperlink()
	input := "\x1b[31m世界abc\x1b[0m " + linkOpen + "go界" + linkClose
	got := wrapWidth(input, 4)

	require.NotContains(t, got, "\x1b[2J")
	require.Equal(t, ansi.StringWidth(safeTerminalText(input)), ansi.StringWidth(got))
	require.Equal(t, 1, strings.Count(got, linkOpen))
	require.Equal(t, 1, strings.Count(got, linkClose))
	for _, line := range strings.Split(got, "\n") {
		require.LessOrEqual(t, cellWidth(line), 4)
	}

	stripped := stripANSI("\x1b[31mhello\x1b[0m"+linkOpen+"world"+linkClose, 0)
	require.Equal(t, "helloworld", stripped)
}

func TestTerminalTextDropsUnallowedControlsAndClosesLinks(t *testing.T) {
	input := "a\x1b[2Jb\x1b]52;c;clipboard\x07c\x07d\x00e" +
		"\x1b]8;;javascript:alert(1)\x1b\\unsafe\x1b]8;;\x1b\\"
	require.Equal(t, "abcdeunsafe", safeTerminalText(input))
	for _, uri := range []string{
		"javascript:alert(1)",
		"file:///etc/passwd",
		"https://user@example.test/path",
		"https://example.test/control\nbyte",
	} {
		require.False(t, safeHyperlinkURL(uri), "URI %q must not be emitted as OSC-8", uri)
	}

	linkOpen := ansi.SetHyperlink("https://example.test")
	linkClose := ansi.ResetHyperlink()
	require.Equal(t, linkOpen+"label"+linkClose, safeTerminalText(linkOpen+"label"))

	// An unterminated OSC sequence is discarded in full, including its body.
	require.Equal(t, "before", safeTerminalText("before\x1b]8;;https://example.testinside"))
}

func TestBoxRendererUsesTerminalCellWidths(t *testing.T) {
	out := BoxRenderer{}.Render(context.Background(), "短\nlong", RenderOpts{})
	lines := strings.Split(stripEscape(out), "\n")
	require.Len(t, lines, 4)
	for _, line := range lines {
		require.Equal(t, 8, ansi.StringWidth(line), "box rows must share a terminal-cell width")
	}
	require.Contains(t, lines[1], "│ 短   │")
	unsafe := BoxRenderer{}.Render(context.Background(), "a\x1b[2Jb", RenderOpts{})
	require.NotContains(t, unsafe, "\x1b[2J")
	require.Contains(t, stripEscape(unsafe), "│ ab │")
}

func TestCodeAndDiffRenderersWrapAtTerminalCellBoundaries(t *testing.T) {
	code := CodeRenderer{}.Render(context.Background(), "界ab", RenderOpts{Width: 3})
	require.Equal(t, "界a\nb", strings.TrimRight(stripEscape(code), " "))
	for _, line := range strings.Split(code, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(line), 3)
	}
	require.NotContains(t, code, "\x1b[2J")
	unsafe := CodeRenderer{}.Render(context.Background(), "a\x1b[2Jb", RenderOpts{})
	require.Equal(t, "ab", stripEscape(unsafe))
	require.NotContains(t, unsafe, "\x1b[2J")

	diff := DiffRenderer{}.Render(context.Background(), "+界ab\n-中文x", RenderOpts{Width: 3})
	for _, line := range strings.Split(diff, "\n") {
		require.LessOrEqual(t, ansi.StringWidth(line), 3)
	}
	require.Equal(t, "+界\nab\n-中\n文x", strings.TrimRight(stripEscape(diff), " "))
}
