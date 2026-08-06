package markdown

import (
	"reflect"
	"testing"
)

func wantNode(t *testing.T, got *Node, wantType NodeType) {
	t.Helper()
	if got == nil {
		t.Fatalf("got nil node, want %v", wantType)
	}
	if got.Type != wantType {
		t.Fatalf("got node type %v, want %v", got.Type, wantType)
	}
}

func TestParseHeadingAllLevels(t *testing.T) {
	for level := 1; level <= 6; level++ {
		prefix := ""
		for i := 0; i < level; i++ {
			prefix += "#"
		}
		root := NewParser().Parse(prefix + " Heading " + itoa(level))
		if root.Type != NodeDocument {
			t.Fatalf("level %d: root type = %v, want NodeDocument", level, root.Type)
		}
		if len(root.Children) != 1 {
			t.Fatalf("level %d: got %d children, want 1", level, len(root.Children))
		}
		h := root.Children[0]
		if h.Type != NodeHeading {
			t.Fatalf("level %d: type = %v, want NodeHeading", level, h.Type)
		}
		if h.Level != level {
			t.Fatalf("level %d: Level = %d, want %d", level, h.Level, level)
		}
		if h.Text != "Heading "+itoa(level) {
			t.Fatalf("level %d: Text = %q, want %q", level, h.Text, "Heading "+itoa(level))
		}
	}
}

func TestParseHeadingRequiresSpace(t *testing.T) {
	// "#NoSpace" is not a heading; it becomes a paragraph.
	root := NewParser().Parse("#NoSpace")
	wantNode(t, root, NodeDocument)
	if len(root.Children) != 1 || root.Children[0].Type != NodeParagraph {
		t.Fatalf("got %+v, want a single NodeParagraph", root.Children)
	}
}

func TestParseHeadingSevenHashes(t *testing.T) {
	// Seven '#' is not a level-7 heading; only 1-6 are valid. The parser caps
	// at 6 hashes and then requires a space. "#######" has no space, so it
	// should fall through to a paragraph.
	root := NewParser().Parse("####### not a heading")
	if len(root.Children) != 1 || root.Children[0].Type != NodeParagraph {
		t.Fatalf("got %+v, want a single NodeParagraph", root.Children)
	}
}

func TestParseCodeBlockWithLanguage(t *testing.T) {
	input := "```go\nfmt.Println(\"hi\")\nsecond\n```\n"
	root := NewParser().Parse(input)
	wantNode(t, root, NodeDocument)
	if len(root.Children) != 1 {
		t.Fatalf("got %d children, want 1", len(root.Children))
	}
	cb := root.Children[0]
	if cb.Type != NodeCodeBlock {
		t.Fatalf("type = %v, want NodeCodeBlock", cb.Type)
	}
	if cb.Lang != "go" {
		t.Fatalf("Lang = %q, want \"go\"", cb.Lang)
	}
	if cb.Text != "fmt.Println(\"hi\")\nsecond" {
		t.Fatalf("Text = %q, want the verbatim code content", cb.Text)
	}
}

func TestParseCodeBlockWithoutLanguage(t *testing.T) {
	input := "```\nplain code\n```\n"
	root := NewParser().Parse(input)
	wantNode(t, root, NodeDocument)
	cb := root.Children[0]
	if cb.Type != NodeCodeBlock {
		t.Fatalf("type = %v, want NodeCodeBlock", cb.Type)
	}
	if cb.Lang != "" {
		t.Fatalf("Lang = %q, want empty", cb.Lang)
	}
	if cb.Text != "plain code" {
		t.Fatalf("Text = %q, want \"plain code\"", cb.Text)
	}
}

func TestParseCodeBlockUnterminated(t *testing.T) {
	input := "```go\nline1\nline2"
	root := NewParser().Parse(input)
	cb := root.Children[0]
	if cb.Type != NodeCodeBlock {
		t.Fatalf("type = %v, want NodeCodeBlock", cb.Type)
	}
	if cb.Lang != "go" {
		t.Fatalf("Lang = %q, want \"go\"", cb.Lang)
	}
	if cb.Text != "line1\nline2" {
		t.Fatalf("Text = %q, want \"line1\\nline2\"", cb.Text)
	}
}

func TestParseCodeBlockPreservesVerbatim(t *testing.T) {
	// Content inside a code block must not be touched.
	input := "```\n  indented\n# not a heading\n- not a list\n```\n"
	root := NewParser().Parse(input)
	cb := root.Children[0]
	want := "  indented\n# not a heading\n- not a list"
	if cb.Text != want {
		t.Fatalf("Text = %q, want %q", cb.Text, want)
	}
}

func TestParseUnorderedListAllMarkers(t *testing.T) {
	for _, marker := range []string{"-", "*", "+"} {
		input := marker + " first\n" + marker + " second\n"
		root := NewParser().Parse(input)
		if len(root.Children) != 1 {
			t.Fatalf("marker %q: got %d children, want 1", marker, len(root.Children))
		}
		list := root.Children[0]
		if list.Type != NodeList {
			t.Fatalf("marker %q: type = %v, want NodeList", marker, list.Type)
		}
		if list.Ordered {
			t.Fatalf("marker %q: Ordered = true, want false", marker)
		}
		if len(list.Children) != 2 {
			t.Fatalf("marker %q: got %d items, want 2", marker, len(list.Children))
		}
		if list.Children[0].Type != NodeListItem || list.Children[0].Text != "first" {
			t.Fatalf("marker %q: first item = %+v", marker, list.Children[0])
		}
		if list.Children[1].Type != NodeListItem || list.Children[1].Text != "second" {
			t.Fatalf("marker %q: second item = %+v", marker, list.Children[1])
		}
	}
}

func TestParseOrderedList(t *testing.T) {
	input := "1. first\n2. second\n3. third\n"
	root := NewParser().Parse(input)
	if len(root.Children) != 1 {
		t.Fatalf("got %d children, want 1", len(root.Children))
	}
	list := root.Children[0]
	if list.Type != NodeList || !list.Ordered {
		t.Fatalf("got %+v, want ordered NodeList", list)
	}
	if len(list.Children) != 3 {
		t.Fatalf("got %d items, want 3", len(list.Children))
	}
	for i, want := range []string{"first", "second", "third"} {
		if list.Children[i].Text != want {
			t.Fatalf("item %d Text = %q, want %q", i, list.Children[i].Text, want)
		}
	}
}

func TestParseOrderedListMarkerAtEOL(t *testing.T) {
	// "1." with no trailing space is still treated as an (empty) ordered item.
	root := NewParser().Parse("1.")
	list := root.Children[0]
	if list.Type != NodeList || !list.Ordered {
		t.Fatalf("got %+v, want ordered NodeList", list)
	}
	if len(list.Children) != 1 || list.Children[0].Text != "" {
		t.Fatalf("got %+v, want one empty item", list.Children)
	}
}

func TestParseListStopsAtDifferentType(t *testing.T) {
	// An ordered marker after an unordered one ends the unordered list.
	input := "- a\n- b\n1. c\n"
	root := NewParser().Parse(input)
	if len(root.Children) != 2 {
		t.Fatalf("got %d children, want 2 (two lists)", len(root.Children))
	}
	if root.Children[0].Type != NodeList || root.Children[0].Ordered {
		t.Fatalf("first child = %+v, want unordered list", root.Children[0])
	}
	if root.Children[1].Type != NodeList || !root.Children[1].Ordered {
		t.Fatalf("second child = %+v, want ordered list", root.Children[1])
	}
}

func TestParseTableWithSeparator(t *testing.T) {
	input := "| Name | Age |\n| --- | --- |\n| Bob | 30 |\n| Alice | 25 |\n"
	root := NewParser().Parse(input)
	if len(root.Children) != 1 {
		t.Fatalf("got %d children, want 1", len(root.Children))
	}
	tbl := root.Children[0]
	if tbl.Type != NodeTable {
		t.Fatalf("type = %v, want NodeTable", tbl.Type)
	}
	// Header + 2 data rows (separator is dropped).
	if len(tbl.Children) != 3 {
		t.Fatalf("got %d rows, want 3", len(tbl.Children))
	}
	wantHeader := []string{"Name", "Age"}
	if !cellsEqual(tbl.Children[0], wantHeader) {
		t.Fatalf("header cells = %v, want %v", cellTexts(tbl.Children[0]), wantHeader)
	}
	if !cellsEqual(tbl.Children[1], []string{"Bob", "30"}) {
		t.Fatalf("row1 cells = %v", cellTexts(tbl.Children[1]))
	}
	if !cellsEqual(tbl.Children[2], []string{"Alice", "25"}) {
		t.Fatalf("row2 cells = %v", cellTexts(tbl.Children[2]))
	}
}

func TestParseTableWithAlignment(t *testing.T) {
	// Separator with colons should still be recognized.
	input := "| L | C | R |\n|:--|:-:|--:|\n| a | b | c |\n"
	root := NewParser().Parse(input)
	tbl := root.Children[0]
	if tbl.Type != NodeTable {
		t.Fatalf("type = %v, want NodeTable", tbl.Type)
	}
	if len(tbl.Children) != 2 {
		t.Fatalf("got %d rows, want 2 (header + 1 data)", len(tbl.Children))
	}
}

func TestParseTableNotTableWithoutSeparator(t *testing.T) {
	// A single pipe line without a separator is not a table; it's a paragraph.
	input := "| not a table |\njust text\n"
	root := NewParser().Parse(input)
	if len(root.Children) != 1 || root.Children[0].Type != NodeParagraph {
		t.Fatalf("got %+v, want a single NodeParagraph", root.Children)
	}
}

func TestParseBlockquote(t *testing.T) {
	input := "> a quote\n> second line\n"
	root := NewParser().Parse(input)
	if len(root.Children) != 1 {
		t.Fatalf("got %d children, want 1", len(root.Children))
	}
	bq := root.Children[0]
	if bq.Type != NodeBlockquote {
		t.Fatalf("type = %v, want NodeBlockquote", bq.Type)
	}
	if len(bq.Children) != 1 || bq.Children[0].Type != NodeParagraph {
		t.Fatalf("got %+v, want one paragraph child", bq.Children)
	}
	if bq.Children[0].Text != "a quote\nsecond line" {
		t.Fatalf("Text = %q, want joined quote text", bq.Children[0].Text)
	}
}

func TestParseBlockquoteContainingList(t *testing.T) {
	input := "> - item one\n> - item two\n"
	root := NewParser().Parse(input)
	bq := root.Children[0]
	if bq.Type != NodeBlockquote {
		t.Fatalf("type = %v, want NodeBlockquote", bq.Type)
	}
	if len(bq.Children) != 1 || bq.Children[0].Type != NodeList {
		t.Fatalf("got %+v, want a nested NodeList", bq.Children)
	}
	list := bq.Children[0]
	if len(list.Children) != 2 {
		t.Fatalf("got %d list items, want 2", len(list.Children))
	}
	if list.Children[0].Text != "item one" || list.Children[1].Text != "item two" {
		t.Fatalf("items = %q, %q", list.Children[0].Text, list.Children[1].Text)
	}
}

func TestParseBlockquoteContainingHeading(t *testing.T) {
	input := "> # Heading in quote\n"
	root := NewParser().Parse(input)
	bq := root.Children[0]
	if bq.Type != NodeBlockquote {
		t.Fatalf("type = %v, want NodeBlockquote", bq.Type)
	}
	if len(bq.Children) != 1 || bq.Children[0].Type != NodeHeading {
		t.Fatalf("got %+v, want a nested NodeHeading", bq.Children)
	}
	if bq.Children[0].Level != 1 || bq.Children[0].Text != "Heading in quote" {
		t.Fatalf("heading = %+v", bq.Children[0])
	}
}

func TestParseHorizontalRules(t *testing.T) {
	for _, rule := range []string{"---", "******", "____", "   ---   "} {
		root := NewParser().Parse(rule)
		if len(root.Children) != 1 {
			t.Fatalf("rule %q: got %d children, want 1", rule, len(root.Children))
		}
		if root.Children[0].Type != NodeHR {
			t.Fatalf("rule %q: type = %v, want NodeHR", rule, root.Children[0].Type)
		}
	}
}

func TestParseHorizontalRuleTooShort(t *testing.T) {
	// Two dashes is not an HR; it is a paragraph.
	root := NewParser().Parse("--")
	if len(root.Children) != 1 || root.Children[0].Type != NodeParagraph {
		t.Fatalf("got %+v, want NodeParagraph", root.Children)
	}
}

func TestParseHorizontalRuleMixedChars(t *testing.T) {
	// Mixed characters are not an HR.
	root := NewParser().Parse("-*-")
	if len(root.Children) != 1 || root.Children[0].Type != NodeParagraph {
		t.Fatalf("got %+v, want NodeParagraph", root.Children)
	}
}

func TestParseParagraph(t *testing.T) {
	root := NewParser().Parse("just a paragraph")
	if len(root.Children) != 1 {
		t.Fatalf("got %d children, want 1", len(root.Children))
	}
	p := root.Children[0]
	if p.Type != NodeParagraph {
		t.Fatalf("type = %v, want NodeParagraph", p.Type)
	}
	if p.Text != "just a paragraph" {
		t.Fatalf("Text = %q, want \"just a paragraph\"", p.Text)
	}
}

func TestParseMultilineParagraph(t *testing.T) {
	root := NewParser().Parse("line one\nline two\nline three")
	p := root.Children[0]
	if p.Type != NodeParagraph {
		t.Fatalf("type = %v, want NodeParagraph", p.Type)
	}
	if p.Text != "line one\nline two\nline three" {
		t.Fatalf("Text = %q, want three joined lines", p.Text)
	}
}

func TestParseParagraphStopsAtBlock(t *testing.T) {
	// A paragraph is terminated when a heading begins on the next line.
	root := NewParser().Parse("some text\n# Heading")
	if len(root.Children) != 2 {
		t.Fatalf("got %d children, want 2", len(root.Children))
	}
	if root.Children[0].Type != NodeParagraph || root.Children[0].Text != "some text" {
		t.Fatalf("first child = %+v, want paragraph", root.Children[0])
	}
	if root.Children[1].Type != NodeHeading || root.Children[1].Text != "Heading" {
		t.Fatalf("second child = %+v, want heading", root.Children[1])
	}
}

func TestParseMultipleParagraphsSeparatedByBlankLines(t *testing.T) {
	input := "first paragraph\n\nsecond paragraph\n\nthird\n"
	root := NewParser().Parse(input)
	if len(root.Children) != 3 {
		t.Fatalf("got %d children, want 3", len(root.Children))
	}
	want := []string{"first paragraph", "second paragraph", "third"}
	for i, w := range want {
		if root.Children[i].Type != NodeParagraph {
			t.Fatalf("child %d type = %v, want NodeParagraph", i, root.Children[i].Type)
		}
		if root.Children[i].Text != w {
			t.Fatalf("child %d Text = %q, want %q", i, root.Children[i].Text, w)
		}
	}
}

func TestParseMixedBlockElements(t *testing.T) {
	input := "# Title\n\nSome intro text.\n\n- a\n- b\n\n> quote\n\n```\ncode\n```\n\n---\n"
	root := NewParser().Parse(input)
	wantTypes := []NodeType{
		NodeHeading,
		NodeParagraph,
		NodeList,
		NodeBlockquote,
		NodeCodeBlock,
		NodeHR,
	}
	if len(root.Children) != len(wantTypes) {
		t.Fatalf("got %d children, want %d", len(root.Children), len(wantTypes))
	}
	for i, wt := range wantTypes {
		if root.Children[i].Type != wt {
			t.Fatalf("child %d type = %v, want %v", i, root.Children[i].Type, wt)
		}
	}
}

func TestParseEmptyInput(t *testing.T) {
	root := NewParser().Parse("")
	wantNode(t, root, NodeDocument)
	if len(root.Children) != 0 {
		t.Fatalf("got %d children, want 0", len(root.Children))
	}
}

func TestParseOnlyBlankLines(t *testing.T) {
	root := NewParser().Parse("\n\n  \n\n")
	wantNode(t, root, NodeDocument)
	if len(root.Children) != 0 {
		t.Fatalf("got %d children, want 0", len(root.Children))
	}
}

func TestParseCRLFLineEndings(t *testing.T) {
	root := NewParser().Parse("# Heading\r\n\r\nParagraph text\r\n")
	if len(root.Children) != 2 {
		t.Fatalf("got %d children, want 2", len(root.Children))
	}
	if root.Children[0].Type != NodeHeading || root.Children[0].Text != "Heading" {
		t.Fatalf("first child = %+v, want heading", root.Children[0])
	}
	if root.Children[1].Type != NodeParagraph || root.Children[1].Text != "Paragraph text" {
		t.Fatalf("second child = %+v, want paragraph", root.Children[1])
	}
}

// --- helper predicates tested directly for branch coverage ---

func TestHeadingInfo(t *testing.T) {
	cases := []struct {
		line         string
		level        int
		content      string
		ok           bool
	}{
		{"# h", 1, "h", true},
		{"## h", 2, "h", true},
		{"###### h", 6, "h", true},
		{"#NoSpace", 0, "", false},
		{"no heading", 0, "", false},
		{"", 0, "", false},
		{"####### h", 0, "", false}, // 7 hashes, no space after 6th
	}
	for _, c := range cases {
		level, content, ok := headingInfo(c.line)
		if ok != c.ok || level != c.level || content != c.content {
			t.Fatalf("headingInfo(%q) = (%d, %q, %v), want (%d, %q, %v)",
				c.line, level, content, ok, c.level, c.content, c.ok)
		}
	}
}

func TestCodeBlockStart(t *testing.T) {
	cases := []struct {
		line string
		lang string
		ok   bool
	}{
		{"```go", "go", true},
		{"```", "", true},
		{"  ```js ", "js", true},
		{"not code", "", false},
		{"``", "", false},
	}
	for _, c := range cases {
		lang, ok := codeBlockStart(c.line)
		if ok != c.ok || lang != c.lang {
			t.Fatalf("codeBlockStart(%q) = (%q, %v), want (%q, %v)",
				c.line, lang, ok, c.lang, c.ok)
		}
	}
}

func TestIsHRLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"---", true},
		{"***", true},
		{"___", true},
		{"   ---   ", true},
		{"------", true},
		{"--", false},
		{"-", false},
		{"-*-", false},
		{"abc", false},
		{"", false},
		{"- a", false}, // list, not HR
	}
	for _, c := range cases {
		if got := isHRLine(c.line); got != c.want {
			t.Fatalf("isHRLine(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestIsListMarker(t *testing.T) {
	cases := []struct {
		line         string
		ordered      bool
		markerLen    int
	}{
		{"- item", false, 2},
		{"* item", false, 2},
		{"+ item", false, 2},
		{"1. item", true, 3},
		{"12. item", true, 4},
		{"1.", true, 2},
		{"1.5 text", false, 0},
		{"", false, 0},
		{"text", false, 0},
		{"-a", false, 0}, // no space after dash
	}
	for _, c := range cases {
		ordered, markerLen := isListMarker(c.line)
		if ordered != c.ordered || markerLen != c.markerLen {
			t.Fatalf("isListMarker(%q) = (%v, %d), want (%v, %d)",
				c.line, ordered, markerLen, c.ordered, c.markerLen)
		}
	}
}

func TestIsTableSeparator(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"|---|---|", true},
		{"|:--:|--:|", true},
		{"| --- | --- |", true},
		{"| abc |", false},
		{"---", false},  // no leading pipe
		{"|", false},    // empty after removing pipes
		{"|a-b|", false},
	}
	for _, c := range cases {
		if got := isTableSeparator(c.line); got != c.want {
			t.Fatalf("isTableSeparator(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestParseTableRow(t *testing.T) {
	cases := []struct {
		line string
		want []string
	}{
		{"| a | b |", []string{"a", "b"}},
		{"|a|b|", []string{"a", "b"}},
		{"|  spaced  | c |", []string{"spaced", "c"}},
		{"single", []string{"single"}}, // no pipes, single cell
	}
	for _, c := range cases {
		got := parseTableRow(c.line)
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("parseTableRow(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestStartsWithBlock(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"heading", []string{"# heading"}, true},
		{"code fence", []string{"```code"}, true},
		{"hr", []string{"---"}, true},
		{"blockquote", []string{"> quote"}, true},
		{"unordered list", []string{"- list"}, true},
		{"ordered list", []string{"1. list"}, true},
		{"table with separator", []string{"| table", "|---|"}, true},
		{"pipe without separator", []string{"| not table", "text"}, false},
		{"plain text", []string{"plain text"}, false},
		{"empty", []string{""}, false},
	}
	for _, c := range cases {
		p := &blockParser{lines: c.lines, pos: 0}
		if got := p.startsWithBlock(c.lines[0]); got != c.want {
			t.Fatalf("%s: startsWithBlock(%q) = %v, want %v", c.name, c.lines[0], got, c.want)
		}
	}
}

func TestParseTableSinglePipeLine(t *testing.T) {
	// A pipe-led line with no following line cannot be a table (no separator
	// possible). It should be parsed as a paragraph, exercising the
	// out-of-lines branch of isTableStart.
	root := NewParser().Parse("| lone pipe")
	if len(root.Children) != 1 || root.Children[0].Type != NodeParagraph {
		t.Fatalf("got %+v, want a single NodeParagraph", root.Children)
	}
	if root.Children[0].Text != "| lone pipe" {
		t.Fatalf("Text = %q, want \"| lone pipe\"", root.Children[0].Text)
	}
}

// TestInlineParserEdgeCases exercises the inline parser's fallback (unclosed
// markers) and escape branches so that block-level text content is fully
// covered. These complement the inline parser's own happy-path tests.
func TestInlineParserEdgeCases(t *testing.T) {
	cases := []struct {
		name          string
		input         string
		wantType      NodeType
		wantText      string // expected when the result is a single NodeText
		wantChildText string // expected text of a single child (formatting nodes)
		wantURL       string // expected URL for link/image nodes
	}{
		{"unclosed bold", "**x", NodeText, "**x", "", ""},
		{"unclosed italic underscore", "_unclosed", NodeText, "_unclosed", "", ""},
		{"unclosed strikethrough", "~~unclosed", NodeText, "~~unclosed", "", ""},
		{"unclosed code", "`unclosed", NodeText, "`unclosed", "", ""},
		{"unclosed link no bracket", "[unclosed", NodeText, "[unclosed", "", ""},
		{"unclosed image no bracket", "![unclosed", NodeText, "![unclosed", "", ""},
		{"link missing paren", "[a]x", NodeText, "[a]x", "", ""},
		{"image missing paren", "![a]x", NodeText, "![a]x", "", ""},
		{"link missing close paren", "[a](url", NodeText, "[a](url", "", ""},
		{"image missing close paren", "![a](url", NodeText, "![a](url", "", ""},
		{"non-escapable after backslash", "\\z", NodeText, "\\z", "", ""},
		{"italic underscore with escape", "_a\\_b_", NodeItalic, "", "a_b", ""},
		{"italic asterisk with escape", "*a\\*b*", NodeItalic, "", "a*b", ""},
		{"link escaped bracket", "[a\\]b](u)", NodeLink, "", "", "u"},
		{"link escaped paren in url", "[a](ur\\)l)", NodeLink, "", "", "ur\\)l"},
		{"italic asterisk double skip", "*a**b*", NodeItalic, "", "a**b", ""},
	}
	for _, c := range cases {
		nodes := parseInline(c.input)
		if len(nodes) != 1 {
			t.Fatalf("%s: got %d nodes, want 1", c.name, len(nodes))
		}
		got := nodes[0]
		if got.Type != c.wantType {
			t.Fatalf("%s: type = %v, want %v", c.name, got.Type, c.wantType)
		}
		if c.wantType == NodeText && got.Text != c.wantText {
			t.Fatalf("%s: text = %q, want %q", c.name, got.Text, c.wantText)
		}
		if c.wantChildText != "" {
			if len(got.Children) != 1 || got.Children[0].Text != c.wantChildText {
				t.Fatalf("%s: child text = %v, want %q", c.name, got.Children, c.wantChildText)
			}
		}
		if c.wantURL != "" && got.URL != c.wantURL {
			t.Fatalf("%s: URL = %q, want %q", c.name, got.URL, c.wantURL)
		}
	}
}

// --- helpers ---

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

func cellTexts(row *Node) []string {
	out := make([]string, 0, len(row.Children))
	for _, c := range row.Children {
		out = append(out, c.Text)
	}
	return out
}

func cellsEqual(row *Node, want []string) bool {
	return reflect.DeepEqual(cellTexts(row), want)
}
