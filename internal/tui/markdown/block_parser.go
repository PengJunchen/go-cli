package markdown

import "strings"

// blockParser performs line-by-line scanning of Markdown text to build
// block-level AST nodes (headings, code blocks, lists, tables, etc.).
type blockParser struct {
	lines []string
	pos   int
}

// newBlockParser splits text into lines, normalizing CRLF to LF.
func newBlockParser(text string) *blockParser {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(text, "\n")
	return &blockParser{lines: lines}
}

// parseDocument parses the full input and returns a NodeDocument root.
func (p *blockParser) parseDocument() *Node {
	doc := &Node{Type: NodeDocument}
	doc.Children = p.parseBlocks()
	return doc
}

// parseBlocks parses consecutive block-level elements, skipping blank lines
// that separate them, and returns the resulting child nodes.
func (p *blockParser) parseBlocks() []*Node {
	var blocks []*Node
	for p.pos < len(p.lines) {
		if strings.TrimSpace(p.lines[p.pos]) == "" {
			p.pos++
			continue
		}
		if node := p.parseBlock(); node != nil {
			blocks = append(blocks, node)
		}
	}
	return blocks
}

// parseBlock inspects the current line and dispatches to the matching
// block-level parser. Unrecognized lines become paragraph content.
func (p *blockParser) parseBlock() *Node {
	line := p.lines[p.pos]

	if level, content, ok := headingInfo(line); ok {
		p.pos++
		return &Node{Type: NodeHeading, Level: level, Text: content}
	}

	if lang, ok := codeBlockStart(line); ok {
		return p.parseCodeBlock(lang)
	}

	if isHRLine(line) {
		p.pos++
		return &Node{Type: NodeHR}
	}

	if strings.HasPrefix(strings.TrimSpace(line), ">") {
		return p.parseBlockquote()
	}

	if _, ml := isListMarker(line); ml > 0 {
		ordered, _ := isListMarker(line)
		return p.parseList(ordered)
	}

	if p.isTableStart() {
		return p.parseTable()
	}

	return p.parseParagraph()
}

// parseCodeBlock collects lines until a closing fence. Content is preserved
// verbatim (no inline parsing). An unterminated fence consumes the remainder.
func (p *blockParser) parseCodeBlock(lang string) *Node {
	p.pos++ // consume opening fence
	var content []string
	for p.pos < len(p.lines) {
		l := p.lines[p.pos]
		if strings.TrimSpace(l) == "```" {
			p.pos++
			break
		}
		content = append(content, l)
		p.pos++
	}
	return &Node{Type: NodeCodeBlock, Lang: lang, Text: strings.Join(content, "\n")}
}

// parseBlockquote collects consecutive ">" lines, strips the marker, and
// recursively parses the inner content so blockquotes may nest any block.
func (p *blockParser) parseBlockquote() *Node {
	var inner []string
	for p.pos < len(p.lines) {
		t := strings.TrimSpace(p.lines[p.pos])
		if !strings.HasPrefix(t, ">") {
			break
		}
		rest := t[1:]
		if strings.HasPrefix(rest, " ") {
			rest = rest[1:]
		}
		inner = append(inner, rest)
		p.pos++
	}
	node := &Node{Type: NodeBlockquote}
	node.Children = newBlockParser(strings.Join(inner, "\n")).parseBlocks()
	return node
}

// parseList collects consecutive list-item markers of the same kind.
func (p *blockParser) parseList(ordered bool) *Node {
	node := &Node{Type: NodeList, Ordered: ordered}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if strings.TrimSpace(line) == "" {
			break
		}
		ord, ml := isListMarker(line)
		if ml == 0 || ord != ordered {
			break
		}
		node.Children = append(node.Children, &Node{Type: NodeListItem, Text: strings.TrimSpace(line[ml:])})
		p.pos++
	}
	return node
}

// isTableStart reports whether the current line begins a table, which requires
// a pipe-led header line followed by a separator line.
func (p *blockParser) isTableStart() bool {
	if p.pos >= len(p.lines) {
		return false
	}
	if !strings.HasPrefix(strings.TrimSpace(p.lines[p.pos]), "|") {
		return false
	}
	if p.pos+1 >= len(p.lines) {
		return false
	}
	return isTableSeparator(p.lines[p.pos+1])
}

// parseTable consumes consecutive pipe-led lines. The separator row is dropped.
// Each remaining row becomes a child node; its cells are grandchild nodes.
func (p *blockParser) parseTable() *Node {
	node := &Node{Type: NodeTable}
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			break
		}
		// Skip separator rows after the header has been recorded.
		if isTableSeparator(line) && len(node.Children) > 0 {
			p.pos++
			continue
		}
		rowNode := &Node{Type: NodeText}
		for _, c := range parseTableRow(line) {
			rowNode.Children = append(rowNode.Children, &Node{Type: NodeText, Text: c})
		}
		node.Children = append(node.Children, rowNode)
		p.pos++
	}
	return node
}

// parseParagraph gathers consecutive non-blank lines that do not start another
// block element. Lines are trimmed and joined with newlines. The Text field is
// populated now; inline parsing of that text is added in task 26-3.
func (p *blockParser) parseParagraph() *Node {
	var lines []string
	for p.pos < len(p.lines) {
		line := p.lines[p.pos]
		if strings.TrimSpace(line) == "" {
			break
		}
		if p.startsWithBlock(line) {
			break
		}
		lines = append(lines, strings.TrimSpace(line))
		p.pos++
	}
	return &Node{Type: NodeParagraph, Text: strings.Join(lines, "\n")}
}

// startsWithBlock reports whether a line would begin a new block element,
// terminating the current paragraph.
func (p *blockParser) startsWithBlock(line string) bool {
	if _, _, ok := headingInfo(line); ok {
		return true
	}
	if _, ok := codeBlockStart(line); ok {
		return true
	}
	if isHRLine(line) {
		return true
	}
	if strings.HasPrefix(strings.TrimSpace(line), ">") {
		return true
	}
	if _, ml := isListMarker(line); ml > 0 {
		return true
	}
	// Only break the paragraph for a real table (header + separator). A lone
	// pipe-led line without a separator stays part of the paragraph.
	if strings.HasPrefix(strings.TrimSpace(line), "|") && p.isTableStart() {
		return true
	}
	return false
}

// headingInfo matches ATX headings: one to six '#' followed by a space.
// Returns the level, trimmed content, and whether the line is a heading.
func headingInfo(line string) (level int, content string, ok bool) {
	for level < len(line) && level < 6 && line[level] == '#' {
		level++
	}
	if level == 0 {
		return 0, "", false
	}
	if level >= len(line) || line[level] != ' ' {
		return 0, "", false
	}
	return level, strings.TrimSpace(line[level+1:]), true
}

// codeBlockStart matches a fenced code block opening ("```" optionally
// followed by a language) and returns the language (if any).
func codeBlockStart(line string) (lang string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "```") {
		return "", false
	}
	return strings.TrimSpace(trimmed[3:]), true
}

// isHRLine reports whether a line is a horizontal rule: three or more
// identical '-', '*', or '_' characters (ignoring surrounding whitespace).
func isHRLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 {
		return false
	}
	c := trimmed[0]
	if c != '-' && c != '*' && c != '_' {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != c {
			return false
		}
	}
	return true
}

// isListMarker reports whether a line begins a list item and, if so, returns
// whether it is ordered and the byte length of the marker prefix consumed.
func isListMarker(line string) (ordered bool, markerLen int) {
	// Unordered markers: "- ", "* ", "+ "
	if len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && line[1] == ' ' {
		return false, 2
	}
	// Ordered markers: one or more digits followed by ". "
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i < len(line) && line[i] == '.' {
		if i+1 < len(line) && line[i+1] == ' ' {
			return true, i + 2
		}
		if i+1 == len(line) {
			return true, i + 1
		}
	}
	return false, 0
}

// isTableSeparator reports whether a line is a table separator such as
// "|---|:--:|--:|". It must start with '|' and contain only '-', ':', '|',
// and spaces after the leading pipe is considered.
func isTableSeparator(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") {
		return false
	}
	s := strings.ReplaceAll(trimmed, "|", "")
	s = strings.ReplaceAll(s, " ", "")
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '-' && s[i] != ':' {
			return false
		}
	}
	return true
}

// parseTableRow splits a pipe-led row into trimmed cell contents.
func parseTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	cells := make([]string, 0, len(parts))
	for _, part := range parts {
		cells = append(cells, strings.TrimSpace(part))
	}
	return cells
}
