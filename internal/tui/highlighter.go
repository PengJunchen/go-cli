package tui

import (
	"log/slog"
	"strings"
)

// CodeHighlighter applies syntax highlighting to code blocks.
type CodeHighlighter interface {
	// Highlight applies syntax highlighting to code written in lang.
	Highlight(code, lang string) string
}

// compile-time assertion that DefaultCodeHighlighter satisfies CodeHighlighter.
var _ CodeHighlighter = (*DefaultCodeHighlighter)(nil)

// DefaultCodeHighlighter is a zero-dependency ANSI escape coloring highlighter.
// It colors Go keywords in blue, strings in green, and comments in gray. For
// other languages it applies basic keyword coloring. When stdout is not a TTY
// the code is returned unchanged (no ANSI codes).
type DefaultCodeHighlighter struct{}

// NewDefaultCodeHighlighter returns a ready-to-use highlighter.
func NewDefaultCodeHighlighter() *DefaultCodeHighlighter {
	return &DefaultCodeHighlighter{}
}

// ANSI color escape sequences used by the highlighter.
const (
	hlRed    = "\033[31m"
	hlGreen  = "\033[32m"
	hlYellow = "\033[33m"
	hlBlue   = "\033[34m"
	hlGray   = "\033[90m"
	hlReset  = "\033[0m"
)

// goKeywords is the set of Go language keywords.
var goKeywords = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
}

// commonKeywords is a small set of keywords shared across C-like languages.
var commonKeywords = map[string]bool{
	"if": true, "else": true, "for": true, "while": true, "return": true,
	"func": true, "function": true, "def": true, "class": true, "import": true,
	"from": true, "export": true, "const": true, "let": true, "var": true,
	"true": true, "false": true, "nil": true, "null": true, "none": true,
	"break": true, "continue": true, "switch": true, "case": true, "default": true,
	"try": true, "catch": true, "finally": true, "throw": true, "new": true,
	"async": true, "await": true, "yield": true, "type": true, "struct": true,
	"interface": true, "package": true, "go": true, "select": true, "chan": true,
	"map": true, "range": true, "defer": true,
}

// Highlight applies syntax highlighting to code. When stdout is not a TTY the
// code is returned unchanged.
func (h *DefaultCodeHighlighter) Highlight(code, lang string) string {
	if !isStdoutTerminal() {
		return code
	}
	out := h.highlightCode(code, lang)
	slog.Debug("tui.highlighter.highlight", "lang", lang, "input_bytes", len(code), "output_bytes", len(out))
	return out
}

// HighlightMarkdown processes markdown text and applies syntax highlighting to
// fenced code blocks (```lang ... ```). Non-code text is left unchanged.
func (h *DefaultCodeHighlighter) HighlightMarkdown(text string) string {
	if !isStdoutTerminal() {
		return text
	}
	var sb strings.Builder
	lines := strings.Split(text, "\n")
	inCodeBlock := false
	var codeLines []string
	var lang string

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				// End of code block — highlight accumulated code.
				code := strings.Join(codeLines, "\n")
				sb.WriteString(h.highlightCode(code, lang))
				sb.WriteString("\n")
				sb.WriteString(line)
				inCodeBlock = false
				codeLines = nil
				lang = ""
			} else {
				// Start of code block.
				sb.WriteString(line)
				lang = strings.TrimPrefix(trimmed, "```")
				lang = strings.TrimSpace(lang)
				inCodeBlock = true
			}
		} else if inCodeBlock {
			codeLines = append(codeLines, line)
		} else {
			sb.WriteString(line)
		}
		if i < len(lines)-1 {
			sb.WriteString("\n")
		}
	}

	// Handle unclosed code block at end of text.
	if inCodeBlock && len(codeLines) > 0 {
		code := strings.Join(codeLines, "\n")
		sb.WriteString(h.highlightCode(code, lang))
	}

	return sb.String()
}

// highlightCode applies line-by-line syntax highlighting.
func (h *DefaultCodeHighlighter) highlightCode(code, lang string) string {
	keywords := commonKeywords
	if strings.EqualFold(lang, "go") || strings.EqualFold(lang, "golang") {
		keywords = goKeywords
	}

	var sb strings.Builder
	lines := strings.Split(code, "\n")
	for i, line := range lines {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(h.highlightLine(line, keywords))
	}
	return sb.String()
}

// highlightLine applies syntax highlighting to a single line of code.
func (h *DefaultCodeHighlighter) highlightLine(line string, keywords map[string]bool) string {
	var sb strings.Builder
	i := 0
	for i < len(line) {
		// Full-line comment (// or #).
		if i == 0 {
			trimmed := strings.TrimLeft(line, " \t")
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
				return hlGray + line + hlReset
			}
		}

		// Inline // comment.
		if i+1 < len(line) && line[i] == '/' && line[i+1] == '/' {
			sb.WriteString(hlGray + line[i:] + hlReset)
			return sb.String()
		}

		ch := line[i]

		// Double-quoted string.
		if ch == '"' {
			start := i
			i++
			for i < len(line) && line[i] != '"' {
				if line[i] == '\\' && i+1 < len(line) {
					i += 2
					continue
				}
				i++
			}
			if i < len(line) {
				i++ // include closing quote
			}
			sb.WriteString(hlGreen + line[start:i] + hlReset)
			continue
		}

		// Backtick string (Go raw string).
		if ch == '`' {
			start := i
			i++
			for i < len(line) && line[i] != '`' {
				i++
			}
			if i < len(line) {
				i++ // include closing backtick
			}
			sb.WriteString(hlGreen + line[start:i] + hlReset)
			continue
		}

		// Single-quoted string/char.
		if ch == '\'' {
			start := i
			i++
			for i < len(line) && line[i] != '\'' {
				if line[i] == '\\' && i+1 < len(line) {
					i += 2
					continue
				}
				i++
			}
			if i < len(line) {
				i++ // include closing quote
			}
			sb.WriteString(hlGreen + line[start:i] + hlReset)
			continue
		}

		// Identifier or keyword.
		if isAlphaByte(ch) || ch == '_' {
			start := i
			for i < len(line) && (isAlphaNumByte(line[i]) || line[i] == '_') {
				i++
			}
			word := line[start:i]
			if keywords[word] {
				sb.WriteString(hlBlue + word + hlReset)
			} else {
				sb.WriteString(word)
			}
			continue
		}

		// Numeric literal.
		if ch >= '0' && ch <= '9' {
			start := i
			for i < len(line) && (isAlphaNumByte(line[i]) || line[i] == '.' || line[i] == 'x' || line[i] == 'X') {
				i++
			}
			sb.WriteString(hlYellow + line[start:i] + hlReset)
			continue
		}

		sb.WriteByte(ch)
		i++
	}
	return sb.String()
}

// isAlphaByte reports whether b is an ASCII letter.
func isAlphaByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// isAlphaNumByte reports whether b is an ASCII letter or digit.
func isAlphaNumByte(b byte) bool {
	return isAlphaByte(b) || (b >= '0' && b <= '9')
}
