package markdown

import "testing"

func TestInlineBold(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("**bold**")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeBold {
		t.Errorf("Type = %v, want NodeBold", nodes[0].Type)
	}
	if len(nodes[0].Children) != 1 {
		t.Fatalf("Children len = %d, want 1", len(nodes[0].Children))
	}
	if nodes[0].Children[0].Type != NodeText {
		t.Errorf("Child Type = %v, want NodeText", nodes[0].Children[0].Type)
	}
	if nodes[0].Children[0].Text != "bold" {
		t.Errorf("Child Text = %q, want %q", nodes[0].Children[0].Text, "bold")
	}
}

func TestInlineItalic(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("*italic*")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeItalic {
		t.Errorf("Type = %v, want NodeItalic", nodes[0].Type)
	}
	if len(nodes[0].Children) != 1 {
		t.Fatalf("Children len = %d, want 1", len(nodes[0].Children))
	}
	if nodes[0].Children[0].Type != NodeText {
		t.Errorf("Child Type = %v, want NodeText", nodes[0].Children[0].Type)
	}
	if nodes[0].Children[0].Text != "italic" {
		t.Errorf("Child Text = %q, want %q", nodes[0].Children[0].Text, "italic")
	}
}

func TestInlineItalicUnderscore(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("_italic_")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeItalic {
		t.Errorf("Type = %v, want NodeItalic", nodes[0].Type)
	}
	if len(nodes[0].Children) != 1 {
		t.Fatalf("Children len = %d, want 1", len(nodes[0].Children))
	}
	if nodes[0].Children[0].Text != "italic" {
		t.Errorf("Child Text = %q, want %q", nodes[0].Children[0].Text, "italic")
	}
}

func TestInlineStrikethrough(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("~~strike~~")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeStrikethrough {
		t.Errorf("Type = %v, want NodeStrikethrough", nodes[0].Type)
	}
	if len(nodes[0].Children) != 1 {
		t.Fatalf("Children len = %d, want 1", len(nodes[0].Children))
	}
	if nodes[0].Children[0].Text != "strike" {
		t.Errorf("Child Text = %q, want %q", nodes[0].Children[0].Text, "strike")
	}
}

func TestInlineCode(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("`code`")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeCodeInline {
		t.Errorf("Type = %v, want NodeCodeInline", nodes[0].Type)
	}
	if nodes[0].Text != "code" {
		t.Errorf("Text = %q, want %q", nodes[0].Text, "code")
	}
}

func TestInlineLink(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("[text](url)")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeLink {
		t.Errorf("Type = %v, want NodeLink", nodes[0].Type)
	}
	if nodes[0].Text != "text" {
		t.Errorf("Text = %q, want %q", nodes[0].Text, "text")
	}
	if nodes[0].URL != "url" {
		t.Errorf("URL = %q, want %q", nodes[0].URL, "url")
	}
}

func TestInlineImage(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("![alt](url)")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeImage {
		t.Errorf("Type = %v, want NodeImage", nodes[0].Type)
	}
	if nodes[0].Alt != "alt" {
		t.Errorf("Alt = %q, want %q", nodes[0].Alt, "alt")
	}
	if nodes[0].URL != "url" {
		t.Errorf("URL = %q, want %q", nodes[0].URL, "url")
	}
}

func TestInlineNested(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("**bold *italic* bold**")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeBold {
		t.Errorf("Type = %v, want NodeBold", nodes[0].Type)
	}
	if len(nodes[0].Children) != 3 {
		t.Fatalf("Children len = %d, want 3", len(nodes[0].Children))
	}
	if nodes[0].Children[0].Type != NodeText {
		t.Errorf("Child 0 Type = %v, want NodeText", nodes[0].Children[0].Type)
	}
	if nodes[0].Children[0].Text != "bold " {
		t.Errorf("Child 0 Text = %q, want %q", nodes[0].Children[0].Text, "bold ")
	}
	if nodes[0].Children[1].Type != NodeItalic {
		t.Errorf("Child 1 Type = %v, want NodeItalic", nodes[0].Children[1].Type)
	}
	if nodes[0].Children[2].Type != NodeText {
		t.Errorf("Child 2 Type = %v, want NodeText", nodes[0].Children[2].Type)
	}
	if nodes[0].Children[2].Text != " bold" {
		t.Errorf("Child 2 Text = %q, want %q", nodes[0].Children[2].Text, " bold")
	}
}

func TestInlineEscape(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("\\*not italic\\*")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeText {
		t.Errorf("Type = %v, want NodeText", nodes[0].Type)
	}
	if nodes[0].Text != "*not italic*" {
		t.Errorf("Text = %q, want %q", nodes[0].Text, "*not italic*")
	}
}

func TestInlineUnclosed(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("*unclosed")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeText {
		t.Errorf("Type = %v, want NodeText", nodes[0].Type)
	}
	if nodes[0].Text != "*unclosed" {
		t.Errorf("Text = %q, want %q", nodes[0].Text, "*unclosed")
	}
}

func TestInlineMixed(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("Hello **world** and [link](http://x)")
	if len(nodes) != 4 {
		t.Fatalf("len(nodes) = %d, want 4", len(nodes))
	}
	if nodes[0].Type != NodeText {
		t.Errorf("Node 0 Type = %v, want NodeText", nodes[0].Type)
	}
	if nodes[0].Text != "Hello " {
		t.Errorf("Node 0 Text = %q, want %q", nodes[0].Text, "Hello ")
	}
	if nodes[1].Type != NodeBold {
		t.Errorf("Node 1 Type = %v, want NodeBold", nodes[1].Type)
	}
	if nodes[2].Type != NodeText {
		t.Errorf("Node 2 Type = %v, want NodeText", nodes[2].Type)
	}
	if nodes[2].Text != " and " {
		t.Errorf("Node 2 Text = %q, want %q", nodes[2].Text, " and ")
	}
	if nodes[3].Type != NodeLink {
		t.Errorf("Node 3 Type = %v, want NodeLink", nodes[3].Type)
	}
	if nodes[3].Text != "link" {
		t.Errorf("Node 3 Text = %q, want %q", nodes[3].Text, "link")
	}
	if nodes[3].URL != "http://x" {
		t.Errorf("Node 3 URL = %q, want %q", nodes[3].URL, "http://x")
	}
}

func TestInlinePlainText(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("just text")
	if len(nodes) != 1 {
		t.Fatalf("len(nodes) = %d, want 1", len(nodes))
	}
	if nodes[0].Type != NodeText {
		t.Errorf("Type = %v, want NodeText", nodes[0].Type)
	}
	if nodes[0].Text != "just text" {
		t.Errorf("Text = %q, want %q", nodes[0].Text, "just text")
	}
}

func TestInlineEmpty(t *testing.T) {
	p := &inlineParser{}
	nodes := p.Parse("")
	if len(nodes) != 0 {
		t.Fatalf("len(nodes) = %d, want 0", len(nodes))
	}
}
