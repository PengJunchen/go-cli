package markdown

import "testing"

func TestNewParserReturnsNonNil(t *testing.T) {
	p := NewParser()
	if p == nil {
		t.Fatal("NewParser returned nil, want non-nil Parser")
	}
}

func TestParseReturnsNodeDocumentRoot(t *testing.T) {
	p := NewParser()
	root := p.Parse("# Hello")
	if root == nil {
		t.Fatal("Parse returned nil, want non-nil root Node")
	}
	if root.Type != NodeDocument {
		t.Fatalf("Parse root Type = %v, want NodeDocument", root.Type)
	}
}

func TestParseEmptyStringReturnsDocumentWithNoChildren(t *testing.T) {
	p := NewParser()
	root := p.Parse("")
	if root == nil {
		t.Fatal("Parse returned nil, want non-nil root Node")
	}
	if root.Type != NodeDocument {
		t.Fatalf("Parse root Type = %v, want NodeDocument", root.Type)
	}
	if len(root.Children) != 0 {
		t.Fatalf("Parse root Children len = %d, want 0 for empty input", len(root.Children))
	}
}
