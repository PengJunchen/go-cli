//go:build e2e

package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2EMarkdownRender tests a complete Markdown document through the full
// parse-render pipeline, verifying every supported block and inline element.
func TestE2EMarkdownRender(t *testing.T) {
	hl := &mockMarkdownHighlighter{}
	r := NewMarkdownRenderer(hl)

	doc := `# Heading 1

## Heading 2

### Heading 3

#### Heading 4

##### Heading 5

###### Heading 6

This paragraph has **bold**, *italic*, and ~~strikethrough~~ text.

` + "```go\nfunc main() {}\n```\n\n" +
		"```python\ndef main():\n    pass\n```\n\n" +
		"```javascript\nfunction main() {}\n```\n\n" +
		"```rust\nfn main() {}\n```\n\n" +
		"```bash\necho hello\n```\n\n" +
		`1. First item
2. Second item
3. Third item

- Bullet one
- Bullet two
- Bullet three

| Name | Value |
|------|-------|
| A | 1 |
| B | 2 |

> This is a blockquote.

---

[link text](https://example.com)

![alt text](https://example.com/image.png)`

	out := r.Render(context.Background(), doc, RenderOpts{
		Theme:       DarkTheme{},
		Width:       80,
		ContentType: ContentTypeMarkdown,
	})
	plain := stripEscape(out)

	// --- Headings 1-6: bold ANSI code ---
	assert.Contains(t, out, "\x1b[1m", "headings should produce bold ANSI code")
	for _, h := range []string{"Heading 1", "Heading 2", "Heading 3", "Heading 4", "Heading 5", "Heading 6"} {
		assert.Contains(t, plain, h, "heading %q should appear in output", h)
	}

	// --- Bold inline ---
	assert.Contains(t, plain, "bold")

	// --- Italic inline ---
	assert.Contains(t, out, "\x1b[3m", "italic should produce italic ANSI code")
	assert.Contains(t, plain, "italic")

	// --- Strikethrough inline ---
	assert.Contains(t, out, "\x1b[9m", "strikethrough should produce strikethrough ANSI code")
	assert.Contains(t, plain, "strikethrough")

	// --- Code blocks with 5 languages: highlighter invoked ---
	require.True(t, hl.called, "highlighter should have been called")
	for _, code := range []string{"func main() {}", "def main():", "function main() {}", "fn main() {}", "echo hello"} {
		assert.Contains(t, plain, code, "code from language block should appear in output: %q", code)
	}

	// --- Ordered list: numbering and indentation ---
	assert.Contains(t, plain, "1. First item")
	assert.Contains(t, plain, "2. Second item")
	assert.Contains(t, plain, "3. Third item")

	// --- Unordered list: bullet ---
	assert.Contains(t, plain, "• Bullet one")
	assert.Contains(t, plain, "• Bullet two")
	assert.Contains(t, plain, "• Bullet three")

	// --- Table: | separators ---
	assert.Contains(t, plain, "| Name |")
	assert.Contains(t, plain, "| Value |")

	// --- Blockquote: │ prefix ---
	assert.Contains(t, out, "│", "blockquote should have │ prefix")

	// --- Horizontal rule: ─ characters ---
	assert.Contains(t, plain, "─", "horizontal rule should contain ─ characters")

	// --- Link: [text](url) format with underline ---
	assert.Contains(t, out, "\x1b[4m", "link should have underline ANSI code")
	assert.Contains(t, plain, "[link text](https://example.com)")

	// --- Image: [image: alt] format ---
	assert.Contains(t, plain, "[image: alt text]")
}

// TestE2EMarkdownANSICodes verifies that a rich Markdown document produces at
// least 4 distinct ANSI style codes from the set: bold (1), italic (3),
// underline (4), strikethrough (9), color (30-107), reset (0).
func TestE2EMarkdownANSICodes(t *testing.T) {
	r := NewMarkdownRenderer(NewDefaultCodeHighlighter())

	doc := `# Title

This has **bold**, *italic*, ~~strikethrough~~, and a [link](https://x.com).

> Quote text.

` + "```go\nfunc main() {}\n```"

	out := r.Render(context.Background(), doc, RenderOpts{
		Theme:       DarkTheme{},
		Width:       80,
		ContentType: ContentTypeMarkdown,
	})

	// Target ANSI codes to detect.
	targets := map[string]string{
		"bold(1)":          "\x1b[1m",
		"italic(3)":        "\x1b[3m",
		"underline(4)":     "\x1b[4m",
		"strikethrough(9)": "\x1b[9m",
		"reset(0)":         "\x1b[0m",
		"color(104)":       "\x1b[104m", // bright cyan (DarkTheme Primary)
		"color(98)":        "\x1b[98m",  // bright black (DarkTheme Secondary)
	}

	found := 0
	for name, code := range targets {
		if strings.Contains(out, code) {
			found++
			t.Logf("found ANSI code: %s", name)
		}
	}

	require.GreaterOrEqual(t, found, 4, "output should contain at least 4 distinct ANSI style codes, got %d", found)
}

// TestE2EBoxSpinnerComponents verifies the BoxRenderer, SpinnerRenderer, and
// ProgressRenderer components.
func TestE2EBoxSpinnerComponents(t *testing.T) {
	// --- BoxRenderer: ┌┐└┘ characters ---
	boxOut := BoxRenderer{}.Render(context.Background(), "content", RenderOpts{Theme: DarkTheme{}})
	for _, ch := range []string{"┌", "┐", "└", "┘"} {
		assert.Contains(t, boxOut, ch, "box output should contain %q", ch)
	}

	// --- SpinnerRenderer: 10 unique frames ---
	spinner := NewSpinnerRenderer()
	require.Len(t, spinner.frames, 10, "spinner should have 10 frames")
	seen := make(map[string]bool, len(spinner.frames))
	for i, f := range spinner.frames {
		assert.False(t, seen[f], "duplicate frame at index %d: %q", i, f)
		seen[f] = true
	}
	// Verify RenderFrame produces output for each frame.
	for i := 0; i < 10; i++ {
		frame := spinner.RenderFrame(i)
		assert.NotEmpty(t, frame, "frame %d should produce non-empty output", i)
	}

	// --- ProgressRenderer: percentage and █/░ characters ---
	progressOut := ProgressRenderer{}.Render(context.Background(), "0.5", RenderOpts{Theme: DarkTheme{}, Width: 40})
	pplain := stripEscape(progressOut)
	assert.Contains(t, pplain, "50%", "progress should show percentage")
	assert.Contains(t, pplain, "█", "progress should use filled bar character")
	assert.Contains(t, pplain, "░", "progress should use empty bar character")
}

// TestE2EMultiLanguageHighlight verifies that code blocks in all 10 supported
// languages are rendered through the MarkdownRenderer and that the highlighter
// applies syntax coloring to language-specific keywords.
func TestE2EMultiLanguageHighlight(t *testing.T) {
	cases := []struct {
		lang    string
		code    string
		keyword string
	}{
		{"go", "func main() {}", "func"},
		{"python", "def main():\n    pass", "def"},
		{"javascript", "function main() {}", "function"},
		{"typescript", "interface Foo {}", "interface"},
		{"rust", "fn main() {}", "fn"},
		{"java", "public class Main {}", "public"},
		{"bash", "if true; then echo hi; fi", "if"},
		{"json", `{"active": true}`, "true"},
		{"yaml", "enabled: true", "true"},
		{"sql", "SELECT * FROM users", "SELECT"},
	}

	h := NewDefaultCodeHighlighter()
	r := NewMarkdownRenderer(h)

	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			md := "```" + tc.lang + "\n" + tc.code + "\n```"
			out := r.Render(context.Background(), md, RenderOpts{
				Theme:       DarkTheme{},
				Width:       80,
				ContentType: ContentTypeMarkdown,
			})
			plain := stripEscape(out)

			// Verify code content is present in the rendered output.
			assert.Contains(t, plain, tc.keyword,
				"keyword %q should appear in rendered %s code block", tc.keyword, tc.lang)

			// Verify highlighting is applied (via direct highlightCode call,
			// since Highlight() returns raw code in non-TTY environments).
			highlighted := h.highlightCode(tc.code, tc.lang)
			assert.Contains(t, highlighted, hlBlue+tc.keyword+hlReset,
				"keyword %q in %s should be colored blue", tc.keyword, tc.lang)
		})
	}
}

// TestE2EOldVsNewRendering compares the old simple rendering (uniform Primary
// color wrap) with the new AST-based rendering (varied styling per node type).
func TestE2EOldVsNewRendering(t *testing.T) {
	content := "# Title\n\nThis has **bold** and *italic* text."

	// Old rendering: simple Primary() color wrap.
	oldOut := DarkTheme{}.Primary().Render(content)

	// New rendering: AST-based with varied styling per node type.
	r := NewMarkdownRenderer(NewDefaultCodeHighlighter())
	newOut := r.Render(context.Background(), content, RenderOpts{
		Theme:       DarkTheme{},
		Width:       80,
		ContentType: ContentTypeMarkdown,
	})

	// Outputs must be different.
	assert.NotEqual(t, oldOut, newOut, "old and new rendering should produce different output")

	// New output contains heading bold codes that old doesn't.
	assert.Contains(t, newOut, "\x1b[1m", "new output should contain bold ANSI code from heading")
	assert.NotContains(t, oldOut, "\x1b[1m", "old output should not contain bold ANSI code")

	// New output contains italic codes that old doesn't.
	assert.Contains(t, newOut, "\x1b[3m", "new output should contain italic ANSI code")
	assert.NotContains(t, oldOut, "\x1b[3m", "old output should not contain italic ANSI code")

	// Count ANSI escape sequences: new should have more than old.
	oldCount := strings.Count(oldOut, "\x1b[")
	newCount := strings.Count(newOut, "\x1b[")
	assert.Greater(t, newCount, oldCount,
		"new output should contain more ANSI escape sequences than old")
}
