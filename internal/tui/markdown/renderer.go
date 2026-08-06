package markdown

import (
	"strconv"
	"strings"
)

// ThemeAdapter is a minimal interface for theme operations,
// avoiding a direct dependency on the tui package.
type ThemeAdapter interface {
	Bold(text string) string
	Italic(text string) string
	Faint(text string) string
	Primary(text string) string
	Secondary(text string) string
	Error(text string) string
	Underline(text string) string
	Strikethrough(text string) string
}

// HighlightAdapter is a minimal interface for code highlighting.
type HighlightAdapter interface {
	Highlight(code, lang string) string
}

// MarkdownASTRenderer renders an AST to ANSI-styled text.
// It is an internal helper used by the MarkdownRenderer in renderers.go.
type MarkdownASTRenderer struct {
	theme       ThemeAdapter
	width       int
	highlighter HighlightAdapter
}

// NewMarkdownASTRenderer creates a new MarkdownASTRenderer.
func NewMarkdownASTRenderer(theme ThemeAdapter, width int, hl HighlightAdapter) *MarkdownASTRenderer {
	return &MarkdownASTRenderer{
		theme:       theme,
		width:       width,
		highlighter: hl,
	}
}

// Render walks the AST and produces ANSI-styled text.
func (r *MarkdownASTRenderer) Render(node *Node) string {
	if node == nil {
		return ""
	}
	if node.Type == NodeDocument {
		if len(node.Children) == 0 {
			return ""
		}
		var parts []string
		for _, child := range node.Children {
			parts = append(parts, r.renderBlock(child))
		}
		return strings.Join(parts, "\n\n")
	}
	return r.renderBlock(node)
}

// renderBlock dispatches to the appropriate block-level renderer.
func (r *MarkdownASTRenderer) renderBlock(node *Node) string {
	switch node.Type {
	case NodeHeading:
		return r.renderHeading(node)
	case NodeParagraph:
		return r.renderInline(node)
	case NodeCodeBlock:
		return r.renderCodeBlock(node)
	case NodeList:
		return r.renderList(node)
	case NodeTable:
		return r.renderTable(node)
	case NodeBlockquote:
		return r.renderBlockquote(node)
	case NodeHR:
		return r.renderHR()
	case NodeListItem:
		return r.renderInline(node)
	default:
		return r.renderInlineNode(node)
	}
}

// renderInline renders the inline content of a node. If the node has Children,
// they are rendered recursively. Otherwise the Text field is parsed for inline
// formatting and the result is rendered.
func (r *MarkdownASTRenderer) renderInline(node *Node) string {
	if len(node.Children) > 0 {
		var sb strings.Builder
		for _, child := range node.Children {
			sb.WriteString(r.renderInlineNode(child))
		}
		return sb.String()
	}
	if node.Text != "" {
		nodes := parseInline(node.Text)
		var sb strings.Builder
		for _, n := range nodes {
			sb.WriteString(r.renderInlineNode(n))
		}
		return sb.String()
	}
	return ""
}

// renderInlineNode renders a single inline node.
func (r *MarkdownASTRenderer) renderInlineNode(node *Node) string {
	switch node.Type {
	case NodeText:
		return node.Text
	case NodeBold:
		return r.theme.Bold(r.renderInline(node))
	case NodeItalic:
		return r.theme.Italic(r.renderInline(node))
	case NodeStrikethrough:
		return r.theme.Strikethrough(r.renderInline(node))
	case NodeCodeInline:
		return r.theme.Secondary("`" + node.Text + "`")
	case NodeLink:
		display := "[" + node.Text + "](" + node.URL + ")"
		return r.theme.Primary(r.theme.Underline(display))
	case NodeImage:
		return r.theme.Secondary("[image: " + node.Alt + "]")
	default:
		return node.Text
	}
}

// renderHeading renders a heading with bold styling and "# " prefix.
// Levels 1-2 get an underline separator.
func (r *MarkdownASTRenderer) renderHeading(node *Node) string {
	prefix := strings.Repeat("#", node.Level) + " "
	content := r.renderInline(node)
	line := prefix + content
	styled := r.theme.Bold(line)
	if node.Level <= 2 {
		sepLen := len(prefix) + rawTextLen(node)
		if sepLen < 3 {
			sepLen = 3
		}
		return styled + "\n" + strings.Repeat("─", sepLen)
	}
	return styled
}

// renderCodeBlock renders a code block with syntax highlighting and 2-space
// indentation on every line.
func (r *MarkdownASTRenderer) renderCodeBlock(node *Node) string {
	code := node.Text
	if r.highlighter != nil {
		code = r.highlighter.Highlight(code, node.Lang)
	}
	var lines []string
	for _, line := range strings.Split(code, "\n") {
		lines = append(lines, "  "+line)
	}
	return strings.Join(lines, "\n")
}

// renderList renders a list with bullet ("• ") or numbered ("1. ") prefixes
// and a 2-space indent on each item.
func (r *MarkdownASTRenderer) renderList(node *Node) string {
	var lines []string
	for i, item := range node.Children {
		var prefix string
		if node.Ordered {
			prefix = strconv.Itoa(i+1) + ". "
		} else {
			prefix = "• "
		}
		content := r.renderInline(item)
		lines = append(lines, "  "+prefix+content)
	}
	return strings.Join(lines, "\n")
}

// renderTable renders a table with column alignment and "|" separators.
// The first row is treated as the header and is followed by a separator line.
func (r *MarkdownASTRenderer) renderTable(node *Node) string {
	if len(node.Children) == 0 {
		return ""
	}
	// Collect raw cell texts for width calculation.
	rawRows := make([][]string, len(node.Children))
	for i, row := range node.Children {
		cells := make([]string, len(row.Children))
		for j, cell := range row.Children {
			cells[j] = cell.Text
		}
		rawRows[i] = cells
	}
	// Calculate the number of columns.
	numCols := 0
	for _, row := range rawRows {
		if len(row) > numCols {
			numCols = len(row)
		}
	}
	if numCols == 0 {
		return ""
	}
	// Calculate column widths.
	colWidths := make([]int, numCols)
	for _, row := range rawRows {
		for j, cell := range row {
			if j < numCols && len(cell) > colWidths[j] {
				colWidths[j] = len(cell)
			}
		}
	}
	// Render rows.
	var lines []string
	for i, row := range rawRows {
		var sb strings.Builder
		sb.WriteString("|")
		for j := 0; j < numCols; j++ {
			cell := ""
			if j < len(row) {
				cell = row[j]
			}
			sb.WriteString(" ")
			sb.WriteString(cell)
			sb.WriteString(strings.Repeat(" ", colWidths[j]-len(cell)))
			sb.WriteString(" |")
		}
		lines = append(lines, sb.String())
		// Add separator after header (first row).
		if i == 0 {
			var sep strings.Builder
			sep.WriteString("|")
			for j := 0; j < numCols; j++ {
				sep.WriteString(strings.Repeat("-", colWidths[j]+2))
				sep.WriteString("|")
			}
			lines = append(lines, sep.String())
		}
	}
	return strings.Join(lines, "\n")
}

// renderBlockquote renders a blockquote with a faint "│ " prefix on each line.
func (r *MarkdownASTRenderer) renderBlockquote(node *Node) string {
	var parts []string
	for _, child := range node.Children {
		parts = append(parts, r.renderBlock(child))
	}
	inner := strings.Join(parts, "\n\n")
	var lines []string
	for _, line := range strings.Split(inner, "\n") {
		lines = append(lines, r.theme.Faint("│ ")+line)
	}
	return strings.Join(lines, "\n")
}

// renderHR renders a horizontal rule spanning the renderer width.
func (r *MarkdownASTRenderer) renderHR() string {
	w := r.width
	if w <= 0 {
		w = 60
	}
	return strings.Repeat("─", w)
}

// rawTextLen returns the visible text length of a node (without ANSI codes).
func rawTextLen(node *Node) int {
	if len(node.Children) > 0 {
		total := 0
		for _, child := range node.Children {
			total += rawTextLen(child)
		}
		return total
	}
	return len(node.Text)
}
