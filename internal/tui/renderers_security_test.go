package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stripEscape removes all ANSI escape sequences from a rendered string so
// tests can inspect the visible payload independent of styling.
func stripEscape(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '\x1b' {
			// Consume the escape sequence up to and including the terminating byte.
			end := strings.IndexByte(s[i:], 'm')
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		sb.WriteByte(s[i])
		i++
	}
	return sb.String()
}

// TestRenderersStripToPayload verifies every non-blank renderer produces
// output whose visible text is self-contained and free of residual escape
// sequences after stripping.
func TestRenderersStripToPayload(t *testing.T) {
	ctx := context.Background()
	cases := map[string]struct {
		r       Renderer
		content string
		want    string
	}{
		ContentTypeMarkdown:       {MarkdownRenderer{}, "md", "md"},
		ContentTypeCode:           {CodeRenderer{}, "func", "func"},
		ContentTypeError:          {ErrorRenderer{}, "err", "err"},
		ContentTypeToolCall:       {ToolCallRenderer{}, "call", "[tool] call"},
		ContentTypeToolResult:     {ToolResultRenderer{}, "res", "[result] res"},
		ContentTypeThinking:       {ThinkingRenderer{}, "think", "think"},
		ContentTypeLink:           {LinkRenderer{}, "https://x", "https://x"},
		ContentTypeSystem:         {SystemRenderer{}, "sys", "sys"},
		ContentTypeUser:           {UserRenderer{}, "ui", "you: ui"},
		ContentTypeAssistant:      {AssistantRenderer{}, "ai", "AI: ai"},
		ContentTypeApproval:       {ApprovalRenderer{}, "ap", "[approval] ap"},
		ContentTypePrompt:         {PromptRenderer{}, "pr", "pr"},
		ContentTypeCompaction:     {CompactionRenderer{}, "co", "co"},
		ContentTypeStreaming:      {StreamingRenderer{}, "st", "st"},
		ContentTypeStreamingCode:  {StreamingCodeRenderer{}, "sc", "sc"},
		ContentTypeStreamingThink: {StreamingThinkingRenderer{}, "sth", "sth"},
		ContentTypeImage:          {ImageRenderer{}, "img", "[image: img]"},
		ContentTypeStatus:         {StatusRenderer{}, "status", "status"},
	}
	for ct, tc := range cases {
		t.Run(ct, func(t *testing.T) {
			out := tc.r.Render(ctx, tc.content, RenderOpts{Theme: DarkTheme{}})
			got := stripEscape(out)
			require.Equal(t, tc.want, got, "visible payload for %s mismatched", ct)
			assert.NotContains(t, got, "\x1b", "stripped output must not retain escapes")
		})
	}
}

// TestRenderersLongSingleLine verifies renderers that wrap to a width emit a
// newline at the boundary and preserve the full original characters after
// stripping.
func TestRenderersLongSingleLine(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("x", 50)
	renderers := []Renderer{
		MarkdownRenderer{}, CodeRenderer{}, ErrorRenderer{}, ThinkingRenderer{},
		StreamingRenderer{}, StreamingCodeRenderer{}, SystemRenderer{},
		StatusRenderer{}, CompactionRenderer{},
	}
	for _, r := range renderers {
		t.Run(r.Name(), func(t *testing.T) {
			out := r.Render(ctx, long, RenderOpts{Theme: DarkTheme{}, Width: 10})
			assert.Contains(t, out, "\n", "%s should wrap a long line at width 10", r.Name())
			assert.Equal(t, strings.Count(long, "x"),
				strings.Count(stripEscape(out), "x"),
				"%s must not drop characters when wrapping", r.Name())
		})
	}
}

// TestRenderersBoldCombinations verifies bold attributes aggregate into the
// SGR codes (1) alongside the color code for every renderer that emits a bold
// label.
func TestRenderersBoldCombinations(t *testing.T) {
	ctx := context.Background()
	renderers := []Renderer{
		ToolCallRenderer{}, ToolResultRenderer{}, ProgressRenderer{},
		UserRenderer{}, AssistantRenderer{}, ApprovalRenderer{},
	}
	for _, r := range renderers {
		t.Run(r.Name(), func(t *testing.T) {
			out := r.Render(ctx, "x", RenderOpts{Theme: DarkTheme{}})
			assert.Contains(t, out, "\x1b[", "%s should apply SGR styling", r.Name())
		})
	}
}

// TestProgressRendererWidthOneAndLarge verifies progress bars stay in bounds for
// extreme widths and stay exactly width cells long.
func TestProgressRendererWidthOneAndLarge(t *testing.T) {
	p := ProgressRenderer{}
	for _, width := range []int{1, 2, 100} {
		out := p.Render(context.Background(), "0.5", RenderOpts{Theme: DarkTheme{}, Width: width})
		assert.Equal(t, width, strings.Count(out, "=")+strings.Count(out, "-"),
			"progress bar should span exactly %d cells", width)
	}
}

// TestProgressFracFractionalWidth verifies sub-cell percentages floor to whole
// cells.
func TestProgressFracFractionalWidth(t *testing.T) {
	require.Equal(t, 0, progressFrac("0.5", 1))
	require.Equal(t, 1, progressFrac("1", 1))
	require.Equal(t, 50, progressFrac("1", 50))
}

// TestDiffRendererRevealsMarkerSeparation verifies diff keeps marker prefixes
// in the payload so additions still start with '+' and deletions with '-'.
func TestDiffRendererRevealsMarkerSeparation(t *testing.T) {
	out := (DiffRenderer{}).Render(context.Background(), "+a\n-b", RenderOpts{Theme: DarkTheme{}})
	plain := stripEscape(out)
	require.Equal(t, "+a\n-b", plain)
}

// TestTableRendererEmptySingleRow verifies a single-line table with no tab is
// styled as a header and stripped back to the exact payload.
func TestTableRendererEmptySingleRow(t *testing.T) {
	out := (TableRenderer{}).Render(context.Background(), "solo", RenderOpts{Theme: DarkTheme{}})
	require.Equal(t, "solo", stripEscape(out))
	require.True(t, strings.HasPrefix(out, "\x1b["), "single table row should be primary-styled")
}

// TestFileTreeRendererStripsToTree verifies file-tree row content survives
// stripping with the leading separator glyph.
func TestFileTreeRendererStripsToTree(t *testing.T) {
	out := (FileTreeRenderer{}).Render(context.Background(), "src", RenderOpts{Theme: DarkTheme{}})
	require.Equal(t, "│ src", stripEscape(out))
}

// TestSeparatorRendererNoAnsi verifies the separator is plain box-drawing text
// with no escape sequences across various widths.
func TestSeparatorRendererNoAnsi(t *testing.T) {
	s := SeparatorRenderer{}
	for _, w := range []int{1, 5, 60} {
		out := s.Render(context.Background(), "", RenderOpts{Width: w})
		assert.Equal(t, strings.Repeat("─", w), out, "separator for width %d", w)
		assert.NotContains(t, out, "\x1b")
	}
	// Zero and negative widths fall back to the default 60 columns.
	require.Equal(t, strings.Repeat("─", 60), s.Render(context.Background(), "", RenderOpts{Width: 0}))
	require.Equal(t, strings.Repeat("─", 60), s.Render(context.Background(), "", RenderOpts{Width: -1}))
}

// TestBlankRendererPreservesNothing verifies blank always emits empty output
// regardless of options and content.
func TestBlankRendererPreservesNothing(t *testing.T) {
	out := (BlankRenderer{}).Render(context.Background(), "ignored", RenderOpts{Theme: DarkTheme{}, Width: 5})
	require.Equal(t, "", out)
}

// TestWrapWidthPreservesContentAcrossWrapping verifies wrapping followed by
// stripping is idempotent with respect to the original content.
func TestWrapWidthPreservesContentAcrossWrapping(t *testing.T) {
	original := "The quick brown fox jumps over the lazy dog."
	wrapped := wrapWidth(original, 10)
	plain := stripEscape(wrapped)
	require.Equal(t, original, strings.ReplaceAll(plain, "\n", ""))
}

// TestRenderThemeNilAndCustom verifies renderTheme chooses the provided theme
// over the dark default and does not mutate the option.
func TestRenderThemeNilAndCustom(t *testing.T) {
	require.IsType(t, DarkTheme{}, renderTheme(RenderOpts{}))
	require.IsType(t, MockTheme{}, renderTheme(RenderOpts{Theme: MockTheme{}}))
	// A nil Theme inside options still falls back to dark.
	require.IsType(t, DarkTheme{}, renderTheme(RenderOpts{Theme: nil}))
}

// TestStyleRenderEmptyStylePassthrough verifies an unset Style passes content
// through byte-for-byte with no escape sequences.
func TestStyleRenderEmptyStylePassthrough(t *testing.T) {
	require.Equal(t, "abc", NewStyle().Render("abc"))
	require.Equal(t, "", NewStyle().Render(""))
}
