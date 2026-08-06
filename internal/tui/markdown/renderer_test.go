package markdown

import (
	"strings"
	"testing"
)

// --- mock implementations ---

// mockTheme implements ThemeAdapter for testing.
type mockTheme struct{}

func (mockTheme) Bold(text string) string          { return "\x1b[1m" + text + "\x1b[0m" }
func (mockTheme) Italic(text string) string        { return "\x1b[3m" + text + "\x1b[0m" }
func (mockTheme) Faint(text string) string         { return "\x1b[2m" + text + "\x1b[0m" }
func (mockTheme) Primary(text string) string       { return "\x1b[36m" + text + "\x1b[0m" }
func (mockTheme) Secondary(text string) string     { return "\x1b[90m" + text + "\x1b[0m" }
func (mockTheme) Error(text string) string         { return "\x1b[31m" + text + "\x1b[0m" }
func (mockTheme) Underline(text string) string     { return "\x1b[4m" + text + "\x1b[0m" }
func (mockTheme) Strikethrough(text string) string { return "\x1b[9m" + text + "\x1b[0m" }

// mockHighlighter implements HighlightAdapter for testing.
type mockHighlighter struct {
	called bool
	code   string
	lang   string
}

func (m *mockHighlighter) Highlight(code, lang string) string {
	m.called = true
	m.code = code
	m.lang = lang
	return code
}

// --- tests ---

func TestRenderHeadingContainsBold(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeHeading, Level: 1, Text: "Title"}
	out := r.Render(node)
	if !strings.Contains(out, "\x1b[1m") {
		t.Fatalf("heading output does not contain bold ANSI code \x1b[1m, got: %q", out)
	}
	if !strings.Contains(out, "# Title") {
		t.Fatalf("heading output does not contain '# Title', got: %q", out)
	}
}

func TestRenderHeadingLevel1HasUnderlineSeparator(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeHeading, Level: 1, Text: "Title"}
	out := r.Render(node)
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("level-1 heading should have an underline separator, got %d lines: %q", len(lines), out)
	}
	if strings.Trim(lines[1], "─") != "" {
		t.Fatalf("underline separator should be all ─, got: %q", lines[1])
	}
}

func TestRenderHeadingLevel2HasUnderlineSeparator(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeHeading, Level: 2, Text: "Sub"}
	out := r.Render(node)
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("level-2 heading should have an underline separator, got %d lines: %q", len(lines), out)
	}
}

func TestRenderHeadingLevel3NoSeparator(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeHeading, Level: 3, Text: "Sub3"}
	out := r.Render(node)
	if strings.Contains(out, "\n") {
		t.Fatalf("level-3 heading should not have a separator, got: %q", out)
	}
}

func TestRenderCodeBlockCallsHighlighter(t *testing.T) {
	hl := &mockHighlighter{}
	r := NewMarkdownASTRenderer(mockTheme{}, 80, hl)
	node := &Node{Type: NodeCodeBlock, Lang: "go", Text: "fmt.Println(\"hi\")"}
	out := r.Render(node)
	if !hl.called {
		t.Fatal("highlighter.Highlight was not called")
	}
	if hl.lang != "go" {
		t.Fatalf("highlighter lang = %q, want %q", hl.lang, "go")
	}
	if hl.code != "fmt.Println(\"hi\")" {
		t.Fatalf("highlighter code = %q, want %q", hl.code, "fmt.Println(\"hi\")")
	}
	// Code block lines should be indented with 2 spaces.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  ") {
			t.Fatalf("code block line not indented with 2 spaces: %q", line)
		}
	}
}

func TestRenderCodeBlockNilHighlighter(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeCodeBlock, Lang: "go", Text: "code line"}
	out := r.Render(node)
	if !strings.Contains(out, "code line") {
		t.Fatalf("code block output should contain raw code, got: %q", out)
	}
}

func TestRenderListHasIndentation(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{
		Type:    NodeList,
		Ordered: false,
		Children: []*Node{
			{Type: NodeListItem, Text: "first"},
			{Type: NodeListItem, Text: "second"},
		},
	}
	out := r.Render(node)
	if !strings.Contains(out, "•") {
		t.Fatalf("list output should contain bullet •, got: %q", out)
	}
	for i, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  ") {
			t.Fatalf("line %d does not have indentation: %q", i, line)
		}
	}
}

func TestRenderOrderedList(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{
		Type:    NodeList,
		Ordered: true,
		Children: []*Node{
			{Type: NodeListItem, Text: "first"},
			{Type: NodeListItem, Text: "second"},
			{Type: NodeListItem, Text: "third"},
		},
	}
	out := r.Render(node)
	if !strings.Contains(out, "1. ") {
		t.Fatalf("ordered list output should contain '1. ', got: %q", out)
	}
	if !strings.Contains(out, "2. ") {
		t.Fatalf("ordered list output should contain '2. ', got: %q", out)
	}
	if !strings.Contains(out, "3. ") {
		t.Fatalf("ordered list output should contain '3. ', got: %q", out)
	}
}

func TestRenderTableHasSeparators(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{
		Type: NodeTable,
		Children: []*Node{
			{Type: NodeText, Children: []*Node{
				{Type: NodeText, Text: "Name"},
				{Type: NodeText, Text: "Age"},
			}},
			{Type: NodeText, Children: []*Node{
				{Type: NodeText, Text: "Bob"},
				{Type: NodeText, Text: "30"},
			}},
		},
	}
	out := r.Render(node)
	if !strings.Contains(out, "|") {
		t.Fatalf("table output should contain | separators, got: %q", out)
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 3 {
		t.Fatalf("table should have at least 3 lines (header, separator, data), got %d: %q", len(lines), out)
	}
	// Second line should be the separator.
	if !strings.Contains(lines[1], "-") {
		t.Fatalf("table separator line should contain -, got: %q", lines[1])
	}
}

func TestRenderBlockquoteHasPrefix(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{
		Type: NodeBlockquote,
		Children: []*Node{
			{Type: NodeParagraph, Text: "a quote"},
		},
	}
	out := r.Render(node)
	if !strings.Contains(out, "│") {
		t.Fatalf("blockquote output should contain │ prefix, got: %q", out)
	}
	if !strings.Contains(out, "a quote") {
		t.Fatalf("blockquote output should contain the quote text, got: %q", out)
	}
}

func TestRenderHR(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeHR}
	out := r.Render(node)
	if out == "" {
		t.Fatal("HR output should not be empty")
	}
	if strings.Trim(out, "─") != "" {
		t.Fatalf("HR output should be all ─ characters, got: %q", out)
	}
	if len([]rune(out)) != 80 {
		t.Fatalf("HR output should be 80 ─ characters (matching width), got %d", len([]rune(out)))
	}
}

func TestRenderHRDefaultWidth(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 0, nil)
	node := &Node{Type: NodeHR}
	out := r.Render(node)
	if len([]rune(out)) != 60 {
		t.Fatalf("HR with width 0 should default to 60 characters, got %d", len([]rune(out)))
	}
}

func TestRenderBold(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeBold, Children: []*Node{{Type: NodeText, Text: "bold"}}}
	out := r.Render(node)
	if !strings.Contains(out, "\x1b[1m") {
		t.Fatalf("bold output should contain bold ANSI code \\x1b[1m, got: %q", out)
	}
	if !strings.Contains(out, "bold") {
		t.Fatalf("bold output should contain 'bold', got: %q", out)
	}
}

func TestRenderItalic(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeItalic, Children: []*Node{{Type: NodeText, Text: "italic"}}}
	out := r.Render(node)
	if !strings.Contains(out, "\x1b[3m") {
		t.Fatalf("italic output should contain italic ANSI code \\x1b[3m, got: %q", out)
	}
}

func TestRenderStrikethrough(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeStrikethrough, Children: []*Node{{Type: NodeText, Text: "strike"}}}
	out := r.Render(node)
	if !strings.Contains(out, "\x1b[9m") {
		t.Fatalf("strikethrough output should contain strikethrough ANSI code \\x1b[9m, got: %q", out)
	}
}

func TestRenderLink(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeLink, Text: "click here", URL: "https://example.com"}
	out := r.Render(node)
	if !strings.Contains(out, "click here") {
		t.Fatalf("link output should contain link text, got: %q", out)
	}
	if !strings.Contains(out, "https://example.com") {
		t.Fatalf("link output should contain URL, got: %q", out)
	}
	if !strings.Contains(out, "\x1b[36m") {
		t.Fatalf("link output should contain primary color ANSI code, got: %q", out)
	}
	if !strings.Contains(out, "\x1b[4m") {
		t.Fatalf("link output should contain underline ANSI code, got: %q", out)
	}
}

func TestRenderImage(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeImage, Alt: "logo", URL: "https://example.com/logo.png"}
	out := r.Render(node)
	if !strings.Contains(out, "[image: logo]") {
		t.Fatalf("image output should contain '[image: logo]', got: %q", out)
	}
	if !strings.Contains(out, "\x1b[90m") {
		t.Fatalf("image output should contain secondary color ANSI code, got: %q", out)
	}
}

func TestRenderCodeInline(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeCodeInline, Text: "code"}
	out := r.Render(node)
	if !strings.Contains(out, "`code`") {
		t.Fatalf("inline code output should contain backtick-wrapped code, got: %q", out)
	}
	if !strings.Contains(out, "\x1b[90m") {
		t.Fatalf("inline code output should contain secondary color ANSI code, got: %q", out)
	}
}

func TestRenderEmptyDocument(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{Type: NodeDocument}
	out := r.Render(node)
	if out != "" {
		t.Fatalf("empty document should render as empty string, got: %q", out)
	}
}

func TestRenderNilNode(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	out := r.Render(nil)
	if out != "" {
		t.Fatalf("nil node should render as empty string, got: %q", out)
	}
}

func TestRenderDocumentWithMultipleBlocks(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{
		Type: NodeDocument,
		Children: []*Node{
			{Type: NodeHeading, Level: 1, Text: "Title"},
			{Type: NodeParagraph, Text: "Some text"},
			{Type: NodeHR},
		},
	}
	out := r.Render(node)
	if !strings.Contains(out, "\x1b[1m") {
		t.Fatalf("document output should contain bold heading, got: %q", out)
	}
	if !strings.Contains(out, "Some text") {
		t.Fatalf("document output should contain paragraph text, got: %q", out)
	}
	if !strings.Contains(out, "─") {
		t.Fatalf("document output should contain HR, got: %q", out)
	}
}

func TestRenderParagraphWithInlineFormatting(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	// Paragraph with raw text that contains inline markdown.
	node := &Node{Type: NodeParagraph, Text: "This is **bold** text"}
	out := r.Render(node)
	if !strings.Contains(out, "\x1b[1m") {
		t.Fatalf("paragraph with bold markdown should contain bold ANSI code, got: %q", out)
	}
	if !strings.Contains(out, "bold") {
		t.Fatalf("paragraph output should contain 'bold', got: %q", out)
	}
}

func TestRenderBlockquoteWithMultipleLines(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{
		Type: NodeBlockquote,
		Children: []*Node{
			{Type: NodeParagraph, Text: "line one\nline two"},
		},
	}
	out := r.Render(node)
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "│") {
			t.Fatalf("line %d of blockquote should contain │ prefix: %q", i, line)
		}
	}
}

func TestRenderTableColumnAlignment(t *testing.T) {
	r := NewMarkdownASTRenderer(mockTheme{}, 80, nil)
	node := &Node{
		Type: NodeTable,
		Children: []*Node{
			{Type: NodeText, Children: []*Node{
				{Type: NodeText, Text: "Name"},
				{Type: NodeText, Text: "Age"},
			}},
			{Type: NodeText, Children: []*Node{
				{Type: NodeText, Text: "Bob"},
				{Type: NodeText, Text: "30"},
			}},
		},
	}
	out := r.Render(node)
	lines := strings.Split(out, "\n")
	// Header and data rows should have aligned columns.
	// "Name" (4) is wider than "Bob" (3), so "Bob" should be padded.
	if !strings.Contains(lines[0], "Name") {
		t.Fatalf("header row should contain 'Name': %q", lines[0])
	}
	if !strings.Contains(lines[2], "Bob ") {
		t.Fatalf("data row should pad 'Bob' to align with 'Name': %q", lines[2])
	}
}
