package markdown

import "strings"

// inlineParser parses inline Markdown elements (bold, italic, code, links,
// etc.) into AST nodes. It is intended to be called by the block parser to
// refine text content within block-level elements.
type inlineParser struct{}

// Parse scans text character by character and produces a slice of Nodes
// representing the inline formatting found.
func (p *inlineParser) Parse(text string) []*Node {
	return parseInline(text)
}

// parseInline is the core inline parsing routine.
func parseInline(text string) []*Node {
	var nodes []*Node
	var plain strings.Builder
	n := len(text)
	i := 0

	flushPlain := func() {
		if plain.Len() > 0 {
			nodes = append(nodes, &Node{Type: NodeText, Text: plain.String()})
			plain.Reset()
		}
	}

	for i < n {
		c := text[i]

		// Escape: \X where X is an escapable punctuation character
		if c == '\\' && i+1 < n && isEscapable(text[i+1]) {
			plain.WriteByte(text[i+1])
			i += 2
			continue
		}

		// Bold: **text** (must be checked before italic *)
		if c == '*' && i+1 < n && text[i+1] == '*' {
			if content, end, ok := findClosing(text, i+2, "**"); ok {
				flushPlain()
				nodes = append(nodes, &Node{
					Type:     NodeBold,
					Children: parseInline(content),
				})
				i = end
				continue
			}
			plain.WriteByte(c)
			i++
			continue
		}

		// Italic: *text*
		if c == '*' {
			if content, end, ok := findClosingSingleAsterisk(text, i+1); ok {
				flushPlain()
				nodes = append(nodes, &Node{
					Type:     NodeItalic,
					Children: parseInline(content),
				})
				i = end
				continue
			}
			plain.WriteByte(c)
			i++
			continue
		}

		// Italic: _text_
		if c == '_' {
			if content, end, ok := findClosing(text, i+1, "_"); ok {
				flushPlain()
				nodes = append(nodes, &Node{
					Type:     NodeItalic,
					Children: parseInline(content),
				})
				i = end
				continue
			}
			plain.WriteByte(c)
			i++
			continue
		}

		// Strikethrough: ~~text~~
		if c == '~' && i+1 < n && text[i+1] == '~' {
			if content, end, ok := findClosing(text, i+2, "~~"); ok {
				flushPlain()
				nodes = append(nodes, &Node{
					Type:     NodeStrikethrough,
					Children: parseInline(content),
				})
				i = end
				continue
			}
			plain.WriteByte(c)
			i++
			continue
		}

		// Inline code: `code`
		if c == '`' {
			if j := findBacktick(text, i+1); j < n {
				flushPlain()
				nodes = append(nodes, &Node{
					Type: NodeCodeInline,
					Text: text[i+1 : j],
				})
				i = j + 1
				continue
			}
			plain.WriteByte(c)
			i++
			continue
		}

		// Image: ![alt](url) — must be checked before links
		if c == '!' && i+1 < n && text[i+1] == '[' {
			if node, end, ok := parseImage(text, i); ok {
				flushPlain()
				nodes = append(nodes, node)
				i = end
				continue
			}
			plain.WriteByte(c)
			i++
			continue
		}

		// Link: [text](url)
		if c == '[' {
			if node, end, ok := parseLink(text, i); ok {
				flushPlain()
				nodes = append(nodes, node)
				i = end
				continue
			}
			plain.WriteByte(c)
			i++
			continue
		}

		// Regular character
		plain.WriteByte(c)
		i++
	}

	flushPlain()
	return nodes
}

// isEscapable reports whether c is an ASCII punctuation character that can be
// escaped with a backslash in Markdown.
func isEscapable(c byte) bool {
	switch c {
	case '\\', '`', '*', '_', '{', '}', '[', ']', '(', ')', '#', '+', '-', '.', '!', '~', '>', '<':
		return true
	default:
		return false
	}
}

// findClosing searches for the closing marker starting from index start,
// skipping backslash-escaped characters. Returns the content between start and
// the marker, the index after the closing marker, and whether it was found.
func findClosing(text string, start int, marker string) (content string, end int, found bool) {
	i := start
	n := len(text)
	mlen := len(marker)
	for i < n {
		if text[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if i+mlen <= n && text[i:i+mlen] == marker {
			return text[start:i], i + mlen, true
		}
		i++
	}
	return "", 0, false
}

// findClosingSingleAsterisk searches for a closing single '*' that is not part
// of a '**' pair, skipping backslash-escaped characters.
func findClosingSingleAsterisk(text string, start int) (content string, end int, found bool) {
	i := start
	n := len(text)
	for i < n {
		if text[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if text[i] == '*' {
			if i+1 < n && text[i+1] == '*' {
				i += 2
				continue
			}
			return text[start:i], i + 1, true
		}
		i++
	}
	return "", 0, false
}

// findBacktick returns the index of the first '`' at or after start, or n if
// none is found. No escape processing is done — code spans are literal.
func findBacktick(text string, start int) int {
	n := len(text)
	for i := start; i < n; i++ {
		if text[i] == '`' {
			return i
		}
	}
	return n
}

// parseLink attempts to parse [text](url) starting at the '[' at index start.
func parseLink(text string, start int) (node *Node, end int, found bool) {
	n := len(text)
	i := start + 1 // skip '['
	for i < n {
		if text[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if text[i] == ']' {
			break
		}
		i++
	}
	if i >= n {
		return nil, 0, false
	}
	linkText := text[start+1 : i]
	i++ // skip ']'
	if i >= n || text[i] != '(' {
		return nil, 0, false
	}
	i++ // skip '('
	urlStart := i
	for i < n {
		if text[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if text[i] == ')' {
			break
		}
		i++
	}
	if i >= n {
		return nil, 0, false
	}
	url := text[urlStart:i]
	i++ // skip ')'
	return &Node{Type: NodeLink, Text: linkText, URL: url}, i, true
}

// parseImage attempts to parse ![alt](url) starting at the '!' at index start.
func parseImage(text string, start int) (node *Node, end int, found bool) {
	n := len(text)
	i := start + 2 // skip '!['
	for i < n {
		if text[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if text[i] == ']' {
			break
		}
		i++
	}
	if i >= n {
		return nil, 0, false
	}
	altText := text[start+2 : i]
	i++ // skip ']'
	if i >= n || text[i] != '(' {
		return nil, 0, false
	}
	i++ // skip '('
	urlStart := i
	for i < n {
		if text[i] == '\\' && i+1 < n {
			i += 2
			continue
		}
		if text[i] == ')' {
			break
		}
		i++
	}
	if i >= n {
		return nil, 0, false
	}
	url := text[urlStart:i]
	i++ // skip ')'
	return &Node{Type: NodeImage, Alt: altText, URL: url}, i, true
}
