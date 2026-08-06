package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"
)

// LineEditor reads user input with optional line editing, history, and
// tab completion.
type LineEditor interface {
	// ReadLine reads a single line (or multi-line block) of user input.
	// It blocks until input is available or ctx is canceled.
	ReadLine(ctx context.Context, prompt string) (string, error)
	// SetHistory replaces the full history.
	SetHistory(h []string)
	// History returns the current history entries.
	History() []string
	// SetCompleter sets the tab completion provider.
	SetCompleter(c Completer)
}

// Completer provides tab completion suggestions.
type Completer interface {
	// Complete returns completion candidates for the given input at the
	// given cursor position. The second return value is the start index
	// in input where the completion text should replace.
	Complete(input string, pos int) ([]Completion, int)
}

// Completion is a single tab completion suggestion.
type Completion struct {
	Text        string
	Description string
}

// DefaultLineEditor is the default LineEditor implementation. It detects
// whether stdin is a TTY and uses raw mode for interactive editing, or falls
// back to a bufio.Scanner for piped input.
type DefaultLineEditor struct {
	in        io.Reader
	out       io.Writer
	history   *HistoryStore
	completer Completer

	// Non-TTY state (lazy-initialized scanner shared across ReadLine calls).
	scanner *bufio.Scanner

	// TTY state (checked once).
	ttyChecked bool
	isTTY      bool

	// Render state for multi-line wrapping support.
	cachedTermWidth int // 0 = not queried yet
	prevVisualLines int // visual lines used by the previous render
}

// NewDefaultLineEditor creates a DefaultLineEditor reading from in and
// writing prompts/output to out.
func NewDefaultLineEditor(in io.Reader, out io.Writer) *DefaultLineEditor {
	return &DefaultLineEditor{
		in:      in,
		out:     out,
		history: NewHistoryStore(1000, ""),
	}
}

// terminalWidth returns the terminal width in columns. It queries stty once
// and caches the result. Falls back to 80 when the terminal size cannot be
// determined.
func (le *DefaultLineEditor) terminalWidth() int {
	if le.cachedTermWidth > 0 {
		return le.cachedTermWidth
	}
	le.cachedTermWidth = 80
	f, ok := le.in.(*os.File)
	if !ok {
		return le.cachedTermWidth
	}
	cmd := exec.Command("stty", "size")
	cmd.Stdin = f
	out, err := cmd.Output()
	if err != nil {
		return le.cachedTermWidth
	}
	var rows, cols int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(out)), "%d %d", &rows, &cols) //nolint:errcheck // best-effort parse
	if cols > 0 {
		le.cachedTermWidth = cols
	}
	return le.cachedTermWidth
}

// visualLineCount calculates how many terminal visual lines the prompt + buffer
// will occupy, accounting for CJK double-width characters and line wrapping.
func visualLineCount(prompt string, buf []rune, termWidth int) int {
	if termWidth <= 0 {
		termWidth = 80
	}
	total := displayWidth([]rune(prompt)) + displayWidth(buf)
	if total == 0 {
		return 1
	}
	return (total + termWidth - 1) / termWidth
}

// stripANSI removes all ANSI escape sequences from s, returning only the
// visible text. This is needed to calculate the true display width of styled
// TUI output.
func stripANSI(s string) string {
	var sb strings.Builder
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// countViewVisualLines counts the number of terminal visual lines a view
// string will occupy, accounting for newlines, ANSI escape sequences, CJK
// double-width characters, and line wrapping.
func countViewVisualLines(view string, termWidth int) int {
	if termWidth <= 0 {
		termWidth = 80
	}
	total := 0
	for _, line := range strings.Split(view, "\n") {
		visible := stripANSI(line)
		w := displayWidth([]rune(visible))
		if w == 0 {
			total++
		} else {
			total += (w + termWidth - 1) / termWidth
		}
	}
	if total == 0 {
		return 1
	}
	return total
}

// ReadLine implements LineEditor.
func (le *DefaultLineEditor) ReadLine(ctx context.Context, prompt string) (string, error) {
	if le.detectTTY() {
		return le.readLineTTY(ctx, prompt)
	}
	return le.readLineNonTTY(ctx, prompt)
}

// SetHistory implements LineEditor.
func (le *DefaultLineEditor) SetHistory(h []string) {
	le.history.entries = make([]string, len(h))
	copy(le.history.entries, h)
}

// History implements LineEditor.
func (le *DefaultLineEditor) History() []string {
	return le.history.List()
}

// SetCompleter implements LineEditor.
func (le *DefaultLineEditor) SetCompleter(c Completer) {
	le.completer = c
}

// detectTTY checks once whether the input reader is a terminal and stty is
// available for raw mode.
func (le *DefaultLineEditor) detectTTY() bool {
	if le.ttyChecked {
		return le.isTTY
	}
	le.ttyChecked = true
	f, ok := le.in.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if _, err := exec.LookPath("stty"); err != nil {
		slog.Debug("cli_line_editor_stty_not_found")
		return false
	}
	le.isTTY = true
	return true
}

// ---------------------------------------------------------------------------
// Non-TTY fallback
// ---------------------------------------------------------------------------

// readLineNonTTY reads a single line using bufio.Scanner, preserving the
// exact behavior of the original REPL input loop.
func (le *DefaultLineEditor) readLineNonTTY(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if le.scanner == nil {
		le.scanner = bufio.NewScanner(le.in)
	}
	fmt.Fprint(le.out, prompt) //nolint:errcheck
	if !le.scanner.Scan() {
		if err := le.scanner.Err(); err != nil {
			return "", err
		}
		return "", io.EOF
	}
	line := le.scanner.Text()
	le.history.Add(line)
	return line, nil
}

// ---------------------------------------------------------------------------
// TTY raw-mode implementation
// ---------------------------------------------------------------------------

// errInterrupted is returned when the user presses Ctrl+C.
var errInterrupted = errors.New("interrupted")

// byteResult is the result of reading a single byte from stdin.
type byteResult struct {
	b   byte
	err error
}

// readLineTTY reads input in raw terminal mode with line editing, history
// navigation, tab completion, and multi-line support.
func (le *DefaultLineEditor) readLineTTY(ctx context.Context, prompt string) (string, error) {
	saved, err := setRawMode(le.in)
	if err != nil {
		slog.Warn("cli_line_editor_raw_mode_failed", "err", err)
		return le.readLineNonTTY(ctx, prompt)
	}
	defer func() {
		if rErr := restoreMode(le.in, saved); rErr != nil {
			slog.Warn("cli_line_editor_restore_mode_failed", "err", rErr)
		}
	}()

	// Single reader goroutine for the duration of this ReadLine call.
	readCh := make(chan byteResult)
	done := make(chan struct{})
	go func() {
		for {
			buf := make([]byte, 1)
			n, err := le.in.Read(buf)
			if n > 0 {
				select {
				case readCh <- byteResult{b: buf[0]}:
				case <-done:
					return
				}
				continue
			}
			if err != nil {
				select {
				case readCh <- byteResult{err: err}:
				case <-done:
					return
				}
				return
			}
		}
	}()
	defer close(done)

	readByte := func() (byte, error) {
		select {
		case res := <-readCh:
			return res.b, res.err
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	// Multi-line accumulation.
	var lines []string
	currentPrompt := prompt
	for {
		line, rErr := le.readSingleLineTTY(readByte, currentPrompt)
		if rErr != nil {
			joined := strings.Join(lines, "\n")
			return joined, rErr
		}

		hasBackslash := strings.HasSuffix(line, "\\")
		if hasBackslash {
			line = strings.TrimSuffix(line, "\\")
		}
		lines = append(lines, line)
		combined := strings.Join(lines, "\n")

		inTripleQuote := strings.Count(combined, "'''")%2 == 1
		if !hasBackslash && !inTripleQuote {
			le.history.Add(combined)
			return combined, nil
		}
		currentPrompt = "... "
	}
}

// readSingleLineTTY reads one physical line with editing support.
func (le *DefaultLineEditor) readSingleLineTTY(readByte func() (byte, error), prompt string) (string, error) {
	fmt.Fprint(le.out, prompt) //nolint:errcheck
	le.prevVisualLines = 0

	termW := le.terminalWidth()

	var buf []rune
	pos := 0
	entries := le.history.entries
	histIdx := len(entries) // past end = current input
	var savedInput string

	render := func() {
		// Move cursor to the first visual line of the input so that
		// clearing affects all wrapped lines, not just the last one.
		if le.prevVisualLines > 1 {
			fmt.Fprintf(le.out, "\033[%dA", le.prevVisualLines-1) //nolint:errcheck
		}
		// Clear from cursor to end of screen (handles wrapped lines).
		fmt.Fprint(le.out, "\r\033[J")  //nolint:errcheck
		fmt.Fprint(le.out, prompt)      //nolint:errcheck
		fmt.Fprint(le.out, string(buf)) //nolint:errcheck
		// Move cursor left to correct position using display width
		// (CJK characters occupy 2 columns each).
		if pos < len(buf) {
			afterCursor := displayWidth(buf[pos:])
			if afterCursor > 0 {
				fmt.Fprintf(le.out, "\033[%dD", afterCursor) //nolint:errcheck
			}
		}
		le.prevVisualLines = visualLineCount(prompt, buf, termW)
	}

	for {
		b, err := readByte()
		if err != nil {
			return "", err
		}

		switch {
		case b == '\r' || b == '\n': // Enter
			fmt.Fprint(le.out, "\r\n") //nolint:errcheck
			return string(buf), nil

		case b == 0x01: // Ctrl+A — beginning of line
			pos = 0
			render()

		case b == 0x05: // Ctrl+E — end of line
			pos = len(buf)
			render()

		case b == 0x08 || b == 0x7F: // Backspace / Delete
			if pos > 0 {
				buf = deleteRunes(buf, pos-1, pos)
				pos--
				render()
			}

		case b == 0x0B: // Ctrl+K — kill to end of line
			buf = buf[:pos]
			render()

		case b == 0x15: // Ctrl+U — kill to beginning of line
			buf = buf[pos:]
			pos = 0
			render()

		case b == 0x17: // Ctrl+W — delete word before cursor
			if pos > 0 {
				start := pos
				for start > 0 && (buf[start-1] == ' ' || buf[start-1] == '\t') {
					start--
				}
				for start > 0 && buf[start-1] != ' ' && buf[start-1] != '\t' {
					start--
				}
				buf = deleteRunes(buf, start, pos)
				pos = start
				render()
			}

		case b == 0x09: // Tab
			if le.completer != nil {
				completions, start := le.completer.Complete(string(buf), pos)
				le.applyCompletions(&buf, &pos, completions, start, prompt)
			}

		case b == 0x03: // Ctrl+C
			fmt.Fprint(le.out, "\r\n") //nolint:errcheck
			return "", errInterrupted

		case b == 0x04: // Ctrl+D
			if len(buf) == 0 {
				fmt.Fprint(le.out, "\r\n") //nolint:errcheck
				return "", io.EOF
			}
			if pos < len(buf) {
				buf = deleteRunes(buf, pos, pos+1)
				render()
			}

		case b == 0x1B: // ESC — possible arrow key sequence
			next, err := readByte()
			if err != nil {
				return "", err
			}
			if next != '[' {
				continue
			}
			third, err := readByte()
			if err != nil {
				return "", err
			}
			switch third {
			case 'A': // Up — history previous
				if histIdx > 0 {
					if histIdx == len(entries) {
						savedInput = string(buf)
					}
					histIdx--
					buf = []rune(entries[histIdx])
					pos = len(buf)
					render()
				}
			case 'B': // Down — history next
				if histIdx < len(entries) {
					histIdx++
					if histIdx == len(entries) {
						buf = []rune(savedInput)
					} else {
						buf = []rune(entries[histIdx])
					}
					pos = len(buf)
					render()
				}
			case 'C': // Right
				if pos < len(buf) {
					w := runeWidth(buf[pos])
					pos++
					fmt.Fprintf(le.out, "\033[%dC", w) //nolint:errcheck
				}
			case 'D': // Left
				if pos > 0 {
					w := runeWidth(buf[pos-1])
					pos--
					fmt.Fprintf(le.out, "\033[%dD", w) //nolint:errcheck
				}
			}

		case b >= 0x20 && b < 0x7F: // Printable ASCII
			buf = insertRune(buf, pos, rune(b))
			pos++
			render()

		case b >= 0x80: // Multi-byte UTF-8 (CJK, emoji, etc.)
			// Determine the total length of the UTF-8 sequence from the
			// leading byte so we can read the remaining continuation bytes.
			var seqLen int
			switch {
			case b >= 0xF0:
				seqLen = 4
			case b >= 0xE0:
				seqLen = 3
			case b >= 0xC0:
				seqLen = 2
			default:
				// Stray continuation byte without a leading byte.
				continue
			}
			seq := make([]byte, seqLen)
			seq[0] = b
			valid := true
			for i := 1; i < seqLen; i++ {
				nb, rErr := readByte()
				if rErr != nil {
					return "", rErr
				}
				if nb < 0x80 || nb > 0xBF {
					valid = false
					break
				}
				seq[i] = nb
			}
			if !valid {
				continue
			}
			r, _ := utf8.DecodeRune(seq)
			if r == utf8.RuneError {
				continue
			}
			buf = insertRune(buf, pos, r)
			pos++
			render()

		default:
			// Ignore other control bytes.
		}
	}
}

// applyCompletions handles tab completion results: auto-complete a single
// match or the common prefix of multiple matches, and display the options.
func (le *DefaultLineEditor) applyCompletions(buf *[]rune, pos *int, completions []Completion, start int, prompt string) {
	if len(completions) == 0 {
		return
	}
	if len(completions) == 1 {
		text := completions[0].Text
		*buf = replaceRange(*buf, start, *pos, []rune(text))
		*pos = start + len(text)
		le.renderBuf(prompt, *buf, *pos)
		return
	}
	// Multiple completions: find common prefix.
	texts := make([]string, len(completions))
	for i, c := range completions {
		texts[i] = c.Text
	}
	prefix := longestCommonPrefix(texts)
	current := ""
	if start < *pos && start <= len(*buf) {
		current = string((*buf)[start:*pos])
	}
	if len(prefix) > len(current) {
		*buf = replaceRange(*buf, start, *pos, []rune(prefix))
		*pos = start + len(prefix)
	}
	// Display options.
	fmt.Fprint(le.out, "\r\n") //nolint:errcheck
	for _, c := range completions {
		if c.Description != "" {
			fmt.Fprintf(le.out, "  %s\t(%s)\r\n", c.Text, c.Description) //nolint:errcheck
		} else {
			fmt.Fprintf(le.out, "  %s\r\n", c.Text) //nolint:errcheck
		}
	}
	// Cursor is on a fresh line after the completion options; no need
	// to move up before rendering.
	le.prevVisualLines = 0
	le.renderBuf(prompt, *buf, *pos)
}

// renderBuf writes the prompt, buffer, and positions the cursor. It also
// handles multi-line wrapping by moving the cursor to the first visual line
// before clearing.
func (le *DefaultLineEditor) renderBuf(prompt string, buf []rune, pos int) {
	if le.prevVisualLines > 1 {
		fmt.Fprintf(le.out, "\033[%dA", le.prevVisualLines-1) //nolint:errcheck
	}
	fmt.Fprint(le.out, "\r\033[J")  //nolint:errcheck
	fmt.Fprint(le.out, prompt)      //nolint:errcheck
	fmt.Fprint(le.out, string(buf)) //nolint:errcheck
	if pos < len(buf) {
		afterCursor := displayWidth(buf[pos:])
		if afterCursor > 0 {
			fmt.Fprintf(le.out, "\033[%dD", afterCursor) //nolint:errcheck
		}
	}
	termW := le.terminalWidth()
	le.prevVisualLines = visualLineCount(prompt, buf, termW)
}

// ---------------------------------------------------------------------------
// Terminal helpers
// ---------------------------------------------------------------------------

// setRawMode saves the current terminal state and enables raw mode on the
// given file descriptor. It returns the saved state string for later
// restoration.
func setRawMode(in io.Reader) (string, error) {
	f, ok := in.(*os.File)
	if !ok {
		return "", fmt.Errorf("input is not a *os.File")
	}
	// Save current terminal state.
	out, err := exec.Command("stty", "-g").Output()
	if err != nil {
		// stty -g writes to stdout; try with stdin set explicitly.
		cmd := exec.Command("stty", "-g")
		cmd.Stdin = f
		out, err = cmd.Output()
		if err != nil {
			return "", err
		}
	}
	saved := strings.TrimSpace(string(out))

	cmd := exec.Command("stty", "raw", "-echo")
	cmd.Stdin = f
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return saved, nil
}

// restoreMode restores the terminal state from the saved string.
func restoreMode(in io.Reader, saved string) error {
	f, ok := in.(*os.File)
	if !ok {
		return fmt.Errorf("input is not a *os.File")
	}
	cmd := exec.Command("stty", saved)
	cmd.Stdin = f
	return cmd.Run()
}

// ---------------------------------------------------------------------------
// Slice helpers
// ---------------------------------------------------------------------------

// insertRune inserts r at position pos in buf, returning a new slice.
func insertRune(buf []rune, pos int, r rune) []rune {
	result := make([]rune, 0, len(buf)+1)
	result = append(result, buf[:pos]...)
	result = append(result, r)
	result = append(result, buf[pos:]...)
	return result
}

// deleteRunes removes runes from start (inclusive) to end (exclusive).
func deleteRunes(buf []rune, start, end int) []rune {
	result := make([]rune, 0, len(buf)-(end-start))
	result = append(result, buf[:start]...)
	result = append(result, buf[end:]...)
	return result
}

// replaceRange replaces buf[start:end] with runes, returning a new slice.
func replaceRange(buf []rune, start, end int, runes []rune) []rune {
	result := make([]rune, 0, len(buf)-(end-start)+len(runes))
	result = append(result, buf[:start]...)
	result = append(result, runes...)
	result = append(result, buf[end:]...)
	return result
}

// longestCommonPrefix returns the longest common prefix of the given strings.
func longestCommonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	}
	prefix := strs[0]
	for _, s := range strs[1:] {
		for !strings.HasPrefix(s, prefix) {
			prefix = prefix[:len(prefix)-1]
			if prefix == "" {
				return ""
			}
		}
	}
	return prefix
}

// runeWidth returns the display width of a rune in terminal columns.
// CJK and other East Asian wide characters occupy 2 columns; everything
// else occupies 1. This covers the common ranges used in Chinese, Japanese,
// and Korean input without pulling in an external dependency.
func runeWidth(r rune) int {
	switch {
	case r >= 0x1100 && (r <= 0x115F || // Hangul Jamo
		r >= 0x2E80 && r <= 0xA4CF && r != 0x303F || // CJK Radicals, Kangxi, Hiragana, Katakana, CJK
		r >= 0xAC00 && r <= 0xD7A3 || // Hangul Syllables
		r >= 0xF900 && r <= 0xFAFF || // CJK Compatibility Ideographs
		r >= 0xFE30 && r <= 0xFE4F || // CJK Compatibility Forms
		r >= 0xFF00 && r <= 0xFF60 || // Fullwidth Forms
		r >= 0xFFE0 && r <= 0xFFE6 || // Fullwidth Signs
		r >= 0x20000 && r <= 0x2FFFD || // CJK Extension B-F
		r >= 0x30000 && r <= 0x3FFFD): // CJK Extension G
		return 2
	default:
		return 1
	}
}

// displayWidth returns the total display width of the rune slice, accounting
// for double-width CJK characters.
func displayWidth(buf []rune) int {
	w := 0
	for _, r := range buf {
		w += runeWidth(r)
	}
	return w
}

// Compile-time assertion that DefaultLineEditor implements LineEditor.
var _ LineEditor = (*DefaultLineEditor)(nil)
