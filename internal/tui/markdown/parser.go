package markdown

// Parser converts Markdown text into an AST
type Parser interface {
	Parse(text string) *Node
}

// NewParser creates a new default Parser
func NewParser() Parser {
	return &defaultParser{}
}

// defaultParser will be implemented in block_parser.go and inline_parser.go
type defaultParser struct{}

// Parse converts Markdown text into an AST.
//
// This is a stub implementation that returns an empty document node.
// The full implementation will be provided in tasks 26-2 (block parser)
// and 26-3 (inline parser).
func (p *defaultParser) Parse(text string) *Node {
	return &Node{Type: NodeDocument}
}
