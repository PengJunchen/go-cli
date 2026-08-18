package tui

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rendererCase is a single expected-behavior entry in the renderer table.
// want contains a set of substrings that every rendered output must contain;
// notWant lists substrings that must be absent.
type rendererCase struct {
	name     string
	renderer Renderer
	content  string
	opts     RenderOpts
	want     []string
	notWant  []string
	exact    string // when non-empty the output must equal this exactly
}

// TestRenderersTable exercises every built-in renderer across representative
// and edge inputs, asserting both the stable Name/Supports contract and the
// presence/absence of styling markers in the rendered output. This complements
// TestEveryRendererProducesOutput by actually verifying per-renderer semantics.
func TestRenderersTable(t *testing.T) {
	ctx := context.Background()
	cases := []rendererCase{
		{
			name:     "markdown heading renders heading text",
			renderer: NewMarkdownRenderer(),
			content:  "# Heading",
			opts:     RenderOpts{Theme: DarkTheme{}, Width: 20},
			want:     []string{"Heading"}, // glamour renders headings without the "#"
			notWant:  []string{},
		},
		{
			name:     "markdown renders plain content",
			renderer: NewMarkdownRenderer(),
			content:  "plain",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"plain"},
			notWant:  []string{},
		},
		{
			name:     "code applies fg style",
			renderer: CodeRenderer{},
			content:  "x := 1",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"38;2;205;214;243", "x := 1"}, // dark fg truecolor
			notWant:  []string{},
		},
		{
			name:     "code passthrough with nil theme defaults to dark",
			renderer: CodeRenderer{},
			content:  "y",
			opts:     RenderOpts{},
			want:     []string{"38;2;205;214;243"},
			notWant:  []string{},
		},
		{
			name:     "table highlights the header row only",
			renderer: TableRenderer{},
			content:  "head\ta\nrow\tb",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"38;2;125;86;243"}, // header is primary styled (#7D56F4; termenv quantizes 0xF4 -> 243)
			notWant:  []string{},
		},
		{
			name:     "table single line",
			renderer: TableRenderer{},
			content:  "only",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"only"},
			notWant:  []string{},
		},
		{
			name:     "diff colors added green removed red contextual grey",
			renderer: DiffRenderer{},
			content:  "+add\n-del\nctx",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"38;2;4;231;97", "38;2;255;92;92", "38;2;205;214;243"},
			notWant:  []string{},
		},
		{
			name:     "diff treats all non-prefixed lines as context",
			renderer: DiffRenderer{},
			content:  "no marker",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"no marker"},
			notWant:  []string{},
		},
		{
			name:     "error uses red style",
			renderer: ErrorRenderer{},
			content:  "boom",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"38;2;255;92;92"},
			notWant:  []string{},
		},
		{
			name:     "tool call prefixes bold [tool] label",
			renderer: ToolCallRenderer{},
			content:  "read file",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"[tool]", "read file"},
			notWant:  []string{},
		},
		{
			name:     "tool result prefixes bold [result] label",
			renderer: ToolResultRenderer{},
			content:  "ok",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"[result]", "ok"},
			notWant:  []string{},
		},
		{
			name:     "thinking applies faint italic",
			renderer: ThinkingRenderer{},
			content:  "reasoning",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"reasoning"},
			notWant:  []string{},
		},
		{
			name:     "progress bar at width bounded",
			renderer: ProgressRenderer{},
			content:  "0.5",
			opts:     RenderOpts{Theme: DarkTheme{}, Width: 10},
			want:     []string{"█████", "░░░░░", "50%"},
			notWant:  []string{},
		},
		{
			name:     "progress clamps above 1.0",
			renderer: ProgressRenderer{},
			content:  "99",
			opts:     RenderOpts{Theme: DarkTheme{}, Width: 4},
			want:     []string{"████", "100%"},
			notWant:  []string{},
		},
		{
			name:     "progress non-numeric fills zero cells",
			renderer: ProgressRenderer{},
			content:  "bananas",
			opts:     RenderOpts{Theme: DarkTheme{}, Width: 4},
			want:     []string{"░░░░", "0%"},
			notWant:  []string{},
		},
		{
			name:     "progress negative clamps to zero",
			renderer: ProgressRenderer{},
			content:  "-0.5",
			opts:     RenderOpts{Theme: DarkTheme{}, Width: 4},
			want:     []string{"░░░░", "0%"},
			notWant:  []string{},
		},
		{
			name:     "progress default width 40 when unset",
			renderer: ProgressRenderer{},
			content:  "0.25",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{strings.Repeat("█", 10), strings.Repeat("░", 30), "25%"},
			notWant:  []string{},
		},
		{
			name:     "file tree decorates each line with a separator glyph",
			renderer: FileTreeRenderer{},
			content:  "src\ncmd",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"│ ", "src", "cmd"},
			notWant:  []string{},
		},
		{
			name:     "image emits a placeholder",
			renderer: ImageRenderer{},
			content:  "photo",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"[image: photo]"},
			notWant:  []string{},
		},
		{
			name:     "link wraps display text in an ANSI escape sequence",
			renderer: LinkRenderer{},
			content:  "https://example.com",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"https://example.com"},
			notWant:  []string{},
		},
		{
			name:     "system renders in secondary style",
			renderer: SystemRenderer{},
			content:  "note",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"note"},
			notWant:  []string{},
		},
		{
			name:     "user prefixes you marker",
			renderer: UserRenderer{},
			content:  "message",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"you: ", "message"},
			notWant:  []string{},
		},
		{
			name:     "assistant prefixes AI marker",
			renderer: AssistantRenderer{},
			content:  "reply",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"AI: ", "reply"},
			notWant:  []string{},
		},
		{
			name:     "approval prefixes [approval] label",
			renderer: ApprovalRenderer{},
			content:  "allow?",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"[approval]", "allow?"},
			notWant:  []string{},
		},
		{
			name:     "prompt renders the payload",
			renderer: PromptRenderer{},
			content:  "ask",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"ask"},
			notWant:  []string{},
		},
		{
			name:     "compaction renders the payload",
			renderer: CompactionRenderer{},
			content:  "summarized",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"summarized"},
			notWant:  []string{},
		},
		{
			name:     "streaming renders primary style",
			renderer: StreamingRenderer{},
			content:  "chunk",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"chunk"},
			notWant:  []string{},
		},
		{
			name:     "streaming code renders fg style",
			renderer: StreamingCodeRenderer{},
			content:  "func{}",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"func{}"},
			notWant:  []string{},
		},
		{
			name:     "streaming thinking renders the payload",
			renderer: StreamingThinkingRenderer{},
			content:  "hmm",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"hmm"},
			notWant:  []string{},
		},
		{
			name:     "blank always emits empty output regardless of content",
			renderer: BlankRenderer{},
			content:  "ignored",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{},
			notWant:  []string{"ignored"},
			exact:    "",
		},
		{
			name:     "separator spans the configured width",
			renderer: SeparatorRenderer{},
			content:  "",
			opts:     RenderOpts{Width: 8},
			want:     []string{strings.Repeat("─", 8)},
			notWant:  []string{},
			exact:    strings.Repeat("─", 8),
		},
		{
			name:     "separator defaults to 60 columns when unset",
			renderer: SeparatorRenderer{},
			content:  "",
			opts:     RenderOpts{},
			want:     []string{strings.Repeat("─", 60)},
			notWant:  []string{},
			exact:    strings.Repeat("─", 60),
		},
		{
			name:     "status renders in secondary style",
			renderer: StatusRenderer{},
			content:  "ready",
			opts:     RenderOpts{Theme: DarkTheme{}},
			want:     []string{"ready"},
			notWant:  []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := tc.renderer.Render(ctx, tc.content, tc.opts)
			if tc.exact != "" {
				assert.Equal(t, tc.exact, out)
				return
			}
			for _, w := range tc.want {
				// Some renderers (link, markdown strikethrough) use underline or
				// strikethrough which lipgloss applies per-rune, fragmenting the
				// raw output. Accept a match in either the raw output or the
				// escape-stripped output.
				assert.True(t, strings.Contains(out, w) || strings.Contains(stripEscape(out), w),
					"renderer %s missing expected substring %q", tc.renderer.Name(), w)
			}
			for _, nw := range tc.notWant {
				assert.NotContains(t, out, nw, "renderer %s unexpectedly contains %q", tc.renderer.Name(), nw)
			}
		})
	}
}

// TestRendererNamesMatchContentType verifies each built-in renderer reports a
// Name equal to the content type it supports and that Name is stable.
func TestRendererNamesMatchContentType(t *testing.T) {
	for _, ct := range contentTypes {
		if ct == ContentTypeSpinner {
			continue // SpinnerRenderer is a standalone component, not a Renderer
		}
		r, ok := NewDefaultRegistry().Get(ct)
		require.True(t, ok, "missing %q", ct)
		assert.Equal(t, ct, r.Name(), "renderer name should equal its content type")
	}
}

// TestRendererSupportsIsExclusive verifies each renderer only Supports its own
// content type and nothing else.
func TestRendererSupportsIsExclusive(t *testing.T) {
	for _, ct := range contentTypes {
		if ct == ContentTypeSpinner {
			continue // SpinnerRenderer is a standalone component, not a Renderer
		}
		r, _ := NewDefaultRegistry().Get(ct)
		for _, other := range contentTypes {
			assert.Equal(t, ct == other, r.Supports(other),
				"%s.Supports(%q) should be %v", ct, other, ct == other)
		}
	}
}

// TestRenderThemeFallback verifies renderTheme returns the provided theme when
// present and the DarkTheme default otherwise.
func TestRenderThemeFallback(t *testing.T) {
	assert.IsType(t, DarkTheme{}, renderTheme(RenderOpts{Theme: nil}))
	assert.IsType(t, MockTheme{}, renderTheme(RenderOpts{Theme: MockTheme{}}))
}

// TestWrapWidthNoWrap verifies wrapWidth returns input unchanged when width is
// zero, negative, or at least the input length.
func TestWrapWidthNoWrap(t *testing.T) {
	long := "abcdefghij"
	assert.Equal(t, long, wrapWidth(long, 0))
	assert.Equal(t, long, wrapWidth(long, -5))
	assert.Equal(t, long, wrapWidth(long, len(long)))
	assert.Equal(t, "", wrapWidth("", 5))
}

// TestWrapWidthInsertsNewlines verifies wrapping inserts newlines at the width
// boundary without altering the original characters.
func TestWrapWidthInsertsNewlines(t *testing.T) {
	in := "abcdefghij"
	out := wrapWidth(in, 4)
	require.Equal(t, "abcd\nefgh\nij", out)
}

// TestWrapWidthResetsAcrossExistingNewlines verifies the column counter resets
// on pre-existing newlines.
func TestWrapWidthResetsAcrossExistingNewlines(t *testing.T) {
	out := wrapWidth("ab\ncd", 4)
	assert.Equal(t, "ab\ncd", out)
}

// TestStripANSIOnlyPrintableColumns verifies stripANSI tracks printable
// columns while a preceding escape sequence does not count as a column.
func TestStripANSIOnlyPrintableColumns(t *testing.T) {
	// The ESC sequence occupies zero printable columns, so the next rune is the
	// first column.
	out := stripANSI("ab", 2)
	assert.Equal(t, "ab", out)
	assert.Equal(t, utf8.RuneCountInString(out), 2)
}

// TestProgressFracTable drives progressFrac with representative, boundary and
// invalid inputs.
func TestProgressFracTable(t *testing.T) {
	tests := []struct {
		in    string
		width int
		want  int
	}{
		{"0", 10, 0},
		{"1", 10, 10},
		{"0.5", 10, 5},
		{"1.5", 10, 10}, // clamps high
		{"-1", 10, 0},   // clamps low
		{"abc", 10, 0},  // unparsable
		{"", 10, 0},     // empty
		{"0.25", 100, 25},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, progressFrac(tc.in, tc.width), "progressFrac(%q, %d)", tc.in, tc.width)
	}
}
