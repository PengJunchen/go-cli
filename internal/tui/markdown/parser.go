package markdown

// Parser converts Markdown text into an AST
type Parser interface {
	Parse(text string) *Node
}

// NewParser creates a new default Parser
func NewParser() Parser {
	return &defaultParser{}
}

// defaultParser is implemented via block_parser.go (block-level parsing) and
// will be extended by inline_parser.go (inline parsing, task 26-3).
type defaultParser struct{}

// Parse converts Markdown text into an AST. It splits the input into lines,
// parses all block-level elements, and returns a NodeDocument root. Inline
// parsing (populating Children of text-bearing nodes) is added in task 26-3;
// for now text content is stored in each node's Text field.
func (p *defaultParser) Parse(text string) *Node {
	return newBlockParser(text).parseDocument()
}
