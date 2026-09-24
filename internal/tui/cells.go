package tui

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
)

const terminalTabWidth = 8

// cellWidth returns the terminal-cell width of safeTerminalText(text).
func cellWidth(text string) int {
	return ansi.StringWidth(safeTerminalText(text))
}

// clipCells clips text to a terminal-cell limit without cutting a grapheme or
// an allowed ANSI sequence. A non-positive limit produces an empty string.
func clipCells(text string, limit int) string {
	if limit <= 0 {
		return ""
	}

	safe := safeTerminalText(text)
	if ansi.StringWidth(safe) <= limit {
		return safe
	}

	return safeTerminalText(ansi.Cut(safe, 0, limit))
}

// wrapCellText sanitizes terminal controls, then wraps without splitting
// Unicode grapheme clusters or allowed ANSI sequences.
func wrapCellText(text string, width int) string {
	safe := safeTerminalText(text)
	if width <= 0 {
		return safe
	}
	return ansi.GraphemeWidth.Hardwrap(safe, width, true)
}

// safeTerminalText preserves UTF-8 text, SGR styling, and safe OSC-8 links.
// Other terminal control sequences are discarded so content cannot execute
// terminal commands. Any style or link opened here is closed before return.
func safeTerminalText(text string) string {
	var out strings.Builder
	out.Grow(len(text))

	parser := ansi.NewParser()
	state := ansi.NormalState
	sgrActive := false
	linkOpen := false
	column := 0
	for len(text) > 0 {
		seq, width, n, nextState := ansi.GraphemeWidth.DecodeSequenceInString(text, state, parser)
		if n <= 0 {
			// The decoder can return no bytes for malformed input. Drop the
			// malformed byte and reset its state so the loop always progresses.
			if text[0] == '\x1b' || text[0] < 0x20 || text[0] == 0x7f {
				text = text[1:]
				state = ansi.NormalState
				parser.Reset()
				continue
			}
			_, size := utf8.DecodeRuneInString(text)
			if size <= 0 {
				size = 1
			}
			out.WriteRune(utf8.RuneError)
			text = text[size:]
			state = ansi.NormalState
			parser.Reset()
			continue
		}

		text = text[n:]
		if nextState != ansi.NormalState {
			// Do not pass incomplete escape sequences or their payload through.
			break
		}
		state = nextState

		if seq == "\n" {
			out.WriteByte('\n')
			column = 0
			continue
		}
		if seq == "\t" {
			spaces := terminalTabWidth - column%terminalTabWidth
			out.WriteString(strings.Repeat(" ", spaces))
			column += spaces
			continue
		}

		if isTerminalSequence(seq) {
			cmd := ansi.Cmd(parser.Command())
			switch {
			case isSGR(seq, cmd):
				out.WriteString(seq)
				sgrActive = sgrStyleActive(seq, sgrActive)
			case isOSC8(seq, parser.Command(), parser.Data()):
				uri, open, ok := parseOSC8(parser.Data())
				if !ok {
					continue
				}
				if open {
					if linkOpen {
						out.WriteString(ansi.ResetHyperlink())
					}
					out.WriteString(ansi.SetHyperlink(uri))
					linkOpen = true
				} else if linkOpen {
					out.WriteString(ansi.ResetHyperlink())
					linkOpen = false
				}
			}
			continue
		}

		writePrintableText(&out, seq)
		column += width
	}

	if linkOpen {
		out.WriteString(ansi.ResetHyperlink())
	}
	if sgrActive {
		out.WriteString("\x1b[0m")
	}
	return out.String()
}

func isTerminalSequence(seq string) bool {
	if len(seq) == 0 {
		return false
	}
	first := seq[0]
	return first == '\x1b' || first == ansi.CSI || first == ansi.OSC || first == 0x90 || first == 0x98 || first == 0x9e || first == 0x9f
}

func isSGR(seq string, cmd ansi.Cmd) bool {
	return ansi.HasCsiPrefix(seq) && cmd.Final() == 'm' && cmd.Prefix() == 0 && cmd.Intermediate() == 0
}

func isOSC8(seq string, command int, data []byte) bool {
	return ansi.HasOscPrefix(seq) && command == 8 && terminatedOSC(seq) && len(data) > 0
}

func terminatedOSC(seq string) bool {
	return strings.HasSuffix(seq, "\x07") || strings.HasSuffix(seq, "\x1b\\") || strings.HasSuffix(seq, "\x9c")
}

func parseOSC8(data []byte) (uri string, open bool, ok bool) {
	command, rest, found := strings.Cut(string(data), ";")
	if !found || command != "8" {
		return "", false, false
	}
	_, uri, found = strings.Cut(rest, ";")
	if !found {
		return "", false, false
	}
	if uri == "" {
		return "", false, true
	}
	return uri, true, safeHyperlinkURL(uri)
}

func safeHyperlinkURL(raw string) bool {
	for _, r := range raw {
		if unicode.IsControl(r) {
			return false
		}
	}

	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || u.Opaque != "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	default:
		return false
	}
}

func sgrStyleActive(seq string, active bool) bool {
	body := seq
	if strings.HasPrefix(body, "\x1b[") {
		body = strings.TrimPrefix(body, "\x1b[")
	} else if strings.HasPrefix(body, "\x9b") {
		body = strings.TrimPrefix(body, "\x9b")
	} else {
		return true
	}
	body = strings.TrimSuffix(body, "m")
	if body == "" {
		return false
	}
	for _, field := range strings.FieldsFunc(body, func(r rune) bool { return r == ';' || r == ':' }) {
		code, err := strconv.Atoi(field)
		if err != nil {
			active = true
			continue
		}
		active = code != 0
	}
	return active
}

func writePrintableText(out *strings.Builder, text string) {
	for len(text) > 0 {
		r, size := utf8.DecodeRuneInString(text)
		if r == utf8.RuneError && size == 1 {
			// Raw C1 bytes are terminal controls, not display text. Other
			// malformed UTF-8 is replaced to keep the output valid UTF-8.
			if text[0] < 0x80 || text[0] > 0x9f {
				out.WriteRune(utf8.RuneError)
			}
			text = text[1:]
			continue
		}
		if !unicode.IsControl(r) {
			out.WriteRune(r)
		}
		if size <= 0 {
			size = 1
		}
		text = text[size:]
	}
}
