package tui

import (
	"strings"

	"github.com/pengjunchen/go-cli/internal/tui/highlighters"
)

// CodeHighlighter applies syntax highlighting to code blocks.
type CodeHighlighter interface {
	// Highlight applies syntax highlighting to code written in lang.
	Highlight(code, lang string) string
}

// compile-time assertion that DefaultCodeHighlighter satisfies CodeHighlighter.
var _ CodeHighlighter = (*DefaultCodeHighlighter)(nil)

// DefaultCodeHighlighter is a zero-dependency ANSI escape coloring highlighter.
// It colors keywords in blue, strings in green, comments in gray, and numbers
// in yellow. For recognized languages it uses a LanguageSpec with the proper
// keyword set, comment styles, and string delimiters. For other languages it
// falls back to a common keyword set. When stdout is not a TTY the code is
// returned unchanged (no ANSI codes).
type DefaultCodeHighlighter struct {
	languages map[string]highlighters.LanguageSpec
}

// NewDefaultCodeHighlighter returns a ready-to-use highlighter preloaded with
// specs for all supported languages.
func NewDefaultCodeHighlighter() *DefaultCodeHighlighter {
	langs := make(map[string]highlighters.LanguageSpec, len(highlighters.Specs))
	for k, v := range highlighters.Specs {
		langs[k] = v
	}
	return &DefaultCodeHighlighter{languages: langs}
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

// commonKeywords is a small set of keywords shared across C-like languages.
// Used as the fallback when the language is not recognized.
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

// fallbackSpec is used when the language is not recognized. It preserves the
// original behavior: commonKeywords, both "//" and "#" as line comments, and
// double/single/backtick string delimiters.
var fallbackSpec = highlighters.LanguageSpec{
	Keywords:     commonKeywords,
	CommentLine:  "", // empty triggers legacy dual "//" / "#" handling
	CommentBlock: [2]string{},
	StringDelims: []string{"\"", "'", "`"},
}

// Highlight applies syntax highlighting to code. When stdout is not a TTY the
// code is returned unchanged.
func (h *DefaultCodeHighlighter) Highlight(code, lang string) string {
	if !isStdoutTerminal() {
		return code
	}
	return h.highlightCode(code, lang)
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

// lookupSpec returns the LanguageSpec for the given language name (case-insensitive)
// and whether a spec was found.
func (h *DefaultCodeHighlighter) lookupSpec(lang string) (highlighters.LanguageSpec, bool) {
	if h.languages != nil {
		if spec, ok := h.languages[strings.ToLower(lang)]; ok {
			return spec, true
		}
	}
	return highlighters.LanguageSpec{}, false
}

// highlightCode applies line-by-line syntax highlighting using the LanguageSpec
// selected for lang. Unknown languages fall back to commonKeywords.
func (h *DefaultCodeHighlighter) highlightCode(code, lang string) string {
	spec, ok := h.lookupSpec(lang)
	if !ok {
		spec = fallbackSpec
	}

	var sb strings.Builder
	lines := strings.Split(code, "\n")
	inBlockComment := false
	for i, line := range lines {
		if i > 0 {
			sb.WriteString("\n")
		}
		var highlighted string
		highlighted, inBlockComment = h.highlightLine(line, spec, inBlockComment)
		sb.WriteString(highlighted)
	}
	return sb.String()
}

// highlightLine applies syntax highlighting to a single line of code. The
// inBlock parameter indicates whether the line begins inside an open block
// comment; the return value reports whether the block comment is still open at
// the end of the line.
func (h *DefaultCodeHighlighter) highlightLine(line string, spec highlighters.LanguageSpec, inBlock bool) (string, bool) {
	var sb strings.Builder
	i := 0

	blockOpen := spec.CommentBlock[0]
	blockClose := spec.CommentBlock[1]
	hasBlockComment := blockOpen != "" && blockClose != ""

	// If we are inside a block comment, look for the closing delimiter.
	if inBlock {
		idx := strings.Index(line, blockClose)
		if idx < 0 {
			// Entire line stays inside the block comment.
			if line != "" {
				return hlGray + line + hlReset, true
			}
			return "", true
		}
		end := idx + len(blockClose)
		sb.WriteString(hlGray + line[:end] + hlReset)
		i = end
		inBlock = false
	}

	for i < len(line) {
		// Full-line comment (only checked at the start of content).
		if i == 0 {
			trimmed := strings.TrimLeft(line, " \t")
			if spec.CommentLine != "" {
				if strings.HasPrefix(trimmed, spec.CommentLine) {
					return hlGray + line + hlReset, inBlock
				}
			} else {
				// Fallback: check both // and #.
				if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
					return hlGray + line + hlReset, inBlock
				}
			}
		}

		// Inline line comment.
		if spec.CommentLine != "" {
			cl := spec.CommentLine
			if i+len(cl) <= len(line) && line[i:i+len(cl)] == cl {
				sb.WriteString(hlGray + line[i:] + hlReset)
				return sb.String(), inBlock
			}
		} else {
			// Fallback: inline // only.
			if i+1 < len(line) && line[i] == '/' && line[i+1] == '/' {
				sb.WriteString(hlGray + line[i:] + hlReset)
				return sb.String(), inBlock
			}
		}

		// Block comment start.
		if hasBlockComment && i+len(blockOpen) <= len(line) {
			if line[i:i+len(blockOpen)] == blockOpen {
				rest := line[i+len(blockOpen):]
				closeIdx := strings.Index(rest, blockClose)
				if closeIdx >= 0 {
					end := i + len(blockOpen) + closeIdx + len(blockClose)
					sb.WriteString(hlGray + line[i:end] + hlReset)
					i = end
					continue
				}
				// No close on this line — rest of line is gray.
				sb.WriteString(hlGray + line[i:] + hlReset)
				return sb.String(), true
			}
		}

		ch := line[i]

		// String literal (driven by spec.StringDelims).
		if isStringDelim(ch, spec.StringDelims) {
			start := i
			i++
			if ch == '`' {
				// Raw string — no escape processing.
				for i < len(line) && line[i] != ch {
					i++
				}
			} else {
				for i < len(line) && line[i] != ch {
					if line[i] == '\\' && i+1 < len(line) {
						i += 2
						continue
					}
					i++
				}
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
			if isKeyword(word, spec) {
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
	return sb.String(), inBlock
}

// isKeyword reports whether word is a keyword in the given spec, honoring the
// CaseInsensitive flag.
func isKeyword(word string, spec highlighters.LanguageSpec) bool {
	if spec.Keywords == nil {
		return false
	}
	if spec.CaseInsensitive {
		return spec.Keywords[strings.ToUpper(word)]
	}
	return spec.Keywords[word]
}

// isStringDelim reports whether ch matches any of the string delimiter
// characters in delims.
func isStringDelim(ch byte, delims []string) bool {
	for _, d := range delims {
		if len(d) > 0 && d[0] == ch {
			return true
		}
	}
	return false
}

// isAlphaByte reports whether b is an ASCII letter.
func isAlphaByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// isAlphaNumByte reports whether b is an ASCII letter or digit.
func isAlphaNumByte(b byte) bool {
	return isAlphaByte(b) || (b >= '0' && b <= '9')
}
