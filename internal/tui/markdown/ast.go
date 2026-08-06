package markdown

// NodeType identifies the kind of AST node
type NodeType int

const (
	NodeDocument NodeType = iota
	NodeHeading       // # ## ###
	NodeParagraph
	NodeCodeBlock   // ```lang
	NodeCodeInline   // `code`
	NodeList         // - / 1.
	NodeListItem
	NodeTable
	NodeBlockquote    // >
	NodeHR            // ---
	NodeBold          // **text**
	NodeItalic        // *text*
	NodeStrikethrough // ~~text~~
	NodeLink          // [text](url)
	NodeImage         // ![alt](url)
	NodeText
)

// Node represents a node in the Markdown AST
type Node struct {
	Type     NodeType
	Children []*Node
	Text     string
	Level    int    // heading level 1-6
	Lang     string // code block language
	URL      string // link/image URL
	Alt      string // image alt text
	Ordered  bool   // list: ordered vs unordered
}
