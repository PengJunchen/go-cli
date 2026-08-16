package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	xterm "golang.org/x/term"
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
	scanner  *bufio.Scanner
	scanDone chan struct{} // closed when the in-flight scan goroutine exits; nil when none is running

	// TTY state (checked once).
	ttyChecked bool
	isTTY      bool

	// Render state for multi-line wrapping support.
	termWidth       atomic.Int64 // 0 = not queried yet (needs re-query)
	prevVisualLines int          // visual lines used by the previous render

	// SIGWINCH monitoring for dynamic terminal resize.
	winchStop chan struct{} // closed to signal the monitor goroutine to exit
	winchDone chan struct{} // closed when the monitor goroutine has exited
	winchOnce sync.Once     // guards lazy startup of monitorResize
	stopOnce  sync.Once     // guards idempotent closing of winchStop

	// IME (input method editor) detection and cooked mode fallback.
	// When IME activity is detected in raw TTY mode, a one-time warning
	// is printed and the user can press Ctrl+\ to switch to cooked mode
	// for better CJK input support.
	imeDetected    bool
	imePromptShown bool
	cookedMode     bool
}

// LineEditorOption configures a DefaultLineEditor at construction time.
type LineEditorOption func(*DefaultLineEditor)

// WithHistoryPath sets the file path used for history persistence. If the
// underlying HistoryStore is nil the option is a no-op.
func WithHistoryPath(path string) LineEditorOption {
	return func(le *DefaultLineEditor) {
		if le.history != nil {
			le.history.filePath = path
		}
	}
}

// WithHistoryMaxLen sets the maximum number of history entries. Values <= 0 are
// ignored so the default (1000) is preserved.
func WithHistoryMaxLen(n int) LineEditorOption {
	return func(le *DefaultLineEditor) {
		if n > 0 && le.history != nil {
			le.history.maxLen = n
		}
	}
}

// NewDefaultLineEditor creates a DefaultLineEditor reading from in and
// writing prompts/output to out. Optional LineEditorOption values can be used
// to configure history persistence.
func NewDefaultLineEditor(in io.Reader, out io.Writer, opts ...LineEditorOption) *DefaultLineEditor {
	le := &DefaultLineEditor{
		in:      in,
		out:     out,
		history: NewHistoryStore(1000, ""),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(le)
		}
	}
	return le
}

// HistoryStore returns the underlying HistoryStore used by the line editor, or
// nil if none is configured. Callers can use it to Load/Save history.
func (le *DefaultLineEditor) HistoryStore() *HistoryStore {
	return le.history
}

// terminalWidth returns the terminal width in columns. It returns the cached
// value if available (non-zero); otherwise it queries the terminal and caches
// the result. Falls back to 80 when the terminal size cannot be determined.
func (le *DefaultLineEditor) terminalWidth() int {
	if w := le.termWidth.Load(); w > 0 {
		return int(w)
	}
	return le.queryTermWidth()
}

// invalidateTermWidth clears the cached terminal width so that the next call
// to terminalWidth re-queries the actual size. It is called on SIGWINCH and
// at the start of each readLineTTY to ensure fresh measurements.
func (le *DefaultLineEditor) invalidateTermWidth() {
	le.termWidth.Store(0)
}

// queryTermWidth queries the terminal size via xterm.GetSize and stores the
// result in the atomic cache. If the query fails or the input is not a TTY,
// 80 is stored as a valid fallback. Returns the stored value.
func (le *DefaultLineEditor) queryTermWidth() int {
	width := int64(80)
	f, ok := le.termFile()
	if ok {
		if cols, _, err := xterm.GetSize(int(f.Fd())); err == nil && cols > 0 {
			width = int64(cols)
		}
	}
	le.termWidth.Store(width)
	return int(width)
}

// startResizeMonitor lazily starts the SIGWINCH monitoring goroutine on the
// first readLineTTY call. It is safe to call multiple times; only the first
// invocation starts the goroutine.
func (le *DefaultLineEditor) startResizeMonitor() {
	le.winchOnce.Do(func() {
		le.winchStop = make(chan struct{})
		le.winchDone = make(chan struct{})
		go le.monitorResize()
	})
}

// Stop shuts down the SIGWINCH monitoring goroutine. It is safe to call
// multiple times and safe to call even if startResizeMonitor was never
// invoked.
func (le *DefaultLineEditor) Stop() {
	le.winchOnce.Do(func() {
		// monitorResize was never started; initialise channels so the
		// wait below succeeds. winchDone is closed immediately since no
		// goroutine is running.
		le.winchStop = make(chan struct{})
		le.winchDone = make(chan struct{})
		close(le.winchDone)
	})
	le.stopOnce.Do(func() {
		close(le.winchStop)
	})
	<-le.winchDone
}

// visualLineCount calculates how many terminal visual lines the prompt + buffer
// will occupy, accounting for CJK double-width characters, line wrapping, and
// explicit newlines (e.g. from a multi-line paste).
func visualLineCount(prompt string, buf []rune, termWidth int) int {
	if termWidth <= 0 {
		termWidth = 80
	}
	// Split buf by newlines to handle explicit line breaks. Each
	// segment's wrapped line count is summed; the prompt only contributes
	// to the first segment's width.
	var totalLines int
	segStart := 0
	for i := 0; i <= len(buf); i++ {
		if i < len(buf) && buf[i] != '\n' {
			continue
		}
		seg := buf[segStart:i]
		var w int
		if totalLines == 0 {
			w = displayWidth([]rune(prompt)) + displayWidth(seg)
		} else {
			w = displayWidth(seg)
		}
		if w == 0 {
			totalLines++
		} else {
			totalLines += (w + termWidth - 1) / termWidth
		}
		segStart = i + 1
	}
	if totalLines == 0 {
		return 1
	}
	return totalLines
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

// ReadLine implements LineEditor.
func (le *DefaultLineEditor) ReadLine(ctx context.Context, prompt string) (string, error) {
	if le.detectTTY() {
		return le.readLineTTY(ctx, prompt)
	}
	return le.readLineNonTTY(ctx, prompt)
}

// SetHistory implements LineEditor.
func (le *DefaultLineEditor) SetHistory(h []string) {
	le.history.Set(h)
}

// History implements LineEditor.
func (le *DefaultLineEditor) History() []string {
	return le.history.List()
}

// SetCompleter implements LineEditor.
func (le *DefaultLineEditor) SetCompleter(c Completer) {
	le.completer = c
}

// detectTTY checks once whether the input reader is an interactive terminal.
func (le *DefaultLineEditor) detectTTY() bool {
	if le.ttyChecked {
		return le.isTTY
	}
	le.ttyChecked = true
	f, ok := le.termFile()
	if !ok {
		return false
	}
	if !xterm.IsTerminal(int(f.Fd())) {
		return false
	}
	le.isTTY = true
	return true
}

// ---------------------------------------------------------------------------
// Non-TTY fallback
// ---------------------------------------------------------------------------

// readLineNonTTY reads a single line using bufio.Scanner. The blocking
// scanner.Scan() call runs in a goroutine so that ReadLine returns promptly
// when ctx is canceled. The result channel is buffered (size 1) so the
// scanner goroutine can always send its result and exit even after the caller
// has returned due to context cancellation.
//
// Note: if the underlying reader never returns (e.g. an open pipe with no
// data), the scanner goroutine will remain blocked on Scan() and cannot be
// interrupted. The caller is responsible for closing the reader to unblock it.
//
// To prevent concurrent access to le.scanner (which is not goroutine-safe),
// a scanDone channel tracks the in-flight goroutine. If ReadLine is called
// again after a cancellation (while the previous goroutine is still running),
// the new call waits for the previous goroutine to exit before proceeding.
func (le *DefaultLineEditor) readLineNonTTY(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// If a previous scan goroutine is still in-flight (e.g. after a context
	// cancellation), wait for it to exit before touching the scanner.
	if le.scanDone != nil {
		select {
		case <-le.scanDone:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		le.scanDone = nil
	}

	if le.scanner == nil {
		le.scanner = bufio.NewScanner(le.in)
	}
	fmt.Fprint(le.out, prompt) //nolint:errcheck

	type scanResult struct {
		line string
		err  error
	}
	resultCh := make(chan scanResult, 1)
	done := make(chan struct{})
	le.scanDone = done

	go func() {
		defer close(done)
		if !le.scanner.Scan() {
			err := le.scanner.Err()
			if err == nil {
				err = io.EOF
			}
			resultCh <- scanResult{err: err}
			return
		}
		resultCh <- scanResult{line: le.scanner.Text()}
	}()

	select {
	case res := <-resultCh:
		le.scanDone = nil
		if res.err != nil {
			return "", res.err
		}
		le.history.Add(res.line)
		return res.line, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// TTY raw-mode implementation
// ---------------------------------------------------------------------------

// markIMEDetected flags that IME (input method editor) activity has been
// detected in raw TTY mode. On the first detection, a one-time warning is
// printed to the editor's output stream informing the user that CJK input
// may not work correctly and that Ctrl+\ switches to cooked mode.
func (le *DefaultLineEditor) markIMEDetected() {
	if le.imeDetected {
		return
	}
	le.imeDetected = true
	if !le.imePromptShown {
		le.imePromptShown = true
		fmt.Fprint(le.out, "\n⚠ IME (input method editor) detected. CJK input may not work correctly in raw mode.\n") //nolint:errcheck
		fmt.Fprint(le.out, "Press Ctrl+\\ to switch to cooked mode for better IME support.\n")                          //nolint:errcheck
	}
}

// readLineCooked reads a single line using bufio.Scanner with the terminal
// in cooked mode. The caller is responsible for restoring the terminal to
// cooked mode before calling this method. A goroutine runs the blocking
// Scanner.Scan() call so that the method returns promptly when ctx is
// canceled.
func (le *DefaultLineEditor) readLineCooked(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	fmt.Fprint(le.out, prompt) //nolint:errcheck

	scanner := bufio.NewScanner(le.in)
	type scanResult struct {
		line string
		err  error
	}
	resultCh := make(chan scanResult, 1)

	go func() {
		if !scanner.Scan() {
			err := scanner.Err()
			if err == nil {
				err = io.EOF
			}
			resultCh <- scanResult{err: err}
			return
		}
		resultCh <- scanResult{line: scanner.Text()}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			return "", res.err
		}
		return res.line, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// readLineCookedMulti reads input in cooked mode with multi-line
// accumulation support (backslash continuation and triple-quote detection).
// lines contains any previously accumulated lines from a raw-mode session;
// nil should be passed when starting fresh.
//
// A single bufio.Scanner is created once and reused across all line reads.
// This is critical because Scanner internally buffers data from the underlying
// reader; creating a new Scanner per line would lose buffered bytes and cause
// premature EOF on the second read.
func (le *DefaultLineEditor) readLineCookedMulti(ctx context.Context, prompt string, lines []string) (string, error) {
	currentPrompt := prompt
	scanner := bufio.NewScanner(le.in)

	type scanResult struct {
		line string
		err  error
	}

	for {
		if err := ctx.Err(); err != nil {
			return strings.Join(lines, "\n"), err
		}
		fmt.Fprint(le.out, currentPrompt) //nolint:errcheck

		resultCh := make(chan scanResult, 1)
		go func() {
			if !scanner.Scan() {
				err := scanner.Err()
				if err == nil {
					err = io.EOF
				}
				resultCh <- scanResult{err: err}
				return
			}
			resultCh <- scanResult{line: scanner.Text()}
		}()

		var line string
		select {
		case res := <-resultCh:
			if res.err != nil {
				joined := strings.Join(lines, "\n")
				if joined != "" {
					var ie *interruptedError
					if errors.As(res.err, &ie) {
						ie.hadContent = true
					}
				}
				return joined, res.err
			}
			line = res.line
		case <-ctx.Done():
			return strings.Join(lines, "\n"), ctx.Err()
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

// errInterrupted is a sentinel error for Ctrl+C interrupts. The concrete
// type returned by the line editor is *interruptedError, which carries
// information about whether the input buffer had content when the interrupt
// occurred, enabling graded Ctrl+C semantics (clear line vs. exit prompt).
var errInterrupted = errors.New("interrupted")

// errToggleCooked is a sentinel error returned by readSingleLineTTY when
// the user presses Ctrl+\ to request a switch from raw mode to cooked mode.
// readLineTTY catches this error, restores the terminal to cooked mode, and
// reads subsequent input via bufio.Scanner.
var errToggleCooked = errors.New("toggle cooked mode")

// interruptedError is the concrete error returned when the user presses
// Ctrl+C. HadContent reports whether the input buffer (including any
// accumulated multi-line content) had text when the interrupt occurred.
type interruptedError struct {
	hadContent bool
}

func (e *interruptedError) Error() string { return "interrupted" }

// Is implements errors.Is so that errors.Is(err, errInterrupted) returns
// true for any *interruptedError.
func (e *interruptedError) Is(target error) bool { return target == errInterrupted }

// byteResult is the result of reading a single byte from stdin.
type byteResult struct {
	b   byte
	err error
}

// readLineTTY reads input in raw terminal mode with line editing, history
// navigation, tab completion, and multi-line support.
func (le *DefaultLineEditor) readLineTTY(ctx context.Context, prompt string) (string, error) {
	// Ensure the SIGWINCH monitor is running (lazy, started once) and
	// invalidate the cached width so each ReadLine re-queries the actual
	// terminal size.
	le.startResizeMonitor()
	le.invalidateTermWidth()

	// If cooked mode is active (set by a previous Ctrl+\ toggle), read
	// directly via bufio.Scanner without entering raw mode. The terminal
	// is already in cooked mode from the previous ReadLine call.
	if le.cookedMode {
		return le.readLineCookedMulti(ctx, prompt, nil)
	}

	saved, err := setRawMode(le.in)
	if err != nil {
		slog.Warn("cli_line_editor_raw_mode_failed", "err", err)
		return le.readLineNonTTY(ctx, prompt)
	}
	defer func() {
		// Disable bracketed paste mode before restoring terminal state.
		fmt.Fprint(le.out, "\x1b[?2004l") //nolint:errcheck
		if rErr := restoreMode(le.in, saved); rErr != nil {
			slog.Warn("cli_line_editor_restore_mode_failed", "err", rErr)
		}
	}()

	// Enable bracketed paste mode so pasted content is wrapped in
	// ESC[200~...ESC[201~ and can be detected as a single unit.
	fmt.Fprint(le.out, "\x1b[?2004h") //nolint:errcheck

	// Single reader goroutine for the duration of this ReadLine call.
	readCh := make(chan byteResult)
	done := make(chan struct{})
	var doneOnce sync.Once
	closeDone := func() { doneOnce.Do(func() { close(done) }) }
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
	defer closeDone()

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
			if errors.Is(rErr, errToggleCooked) {
				// User pressed Ctrl+\ to switch to cooked mode. Stop the
				// reader goroutine, restore the terminal to cooked mode,
				// and continue reading via bufio.Scanner.
				le.cookedMode = true
				closeDone()
				fmt.Fprint(le.out, "\x1b[?2004l") //nolint:errcheck
				if rErr := restoreMode(le.in, saved); rErr != nil {
					slog.Warn("cli_line_editor_restore_mode_failed", "err", rErr)
				}
				return le.readLineCookedMulti(ctx, currentPrompt, lines)
			}
			joined := strings.Join(lines, "\n")
			// If interrupting with accumulated multi-line content, mark
			// the error so the caller treats it as "had content" (clear
			// line rather than exit prompt).
			if joined != "" {
				var ie *interruptedError
				if errors.As(rErr, &ie) {
					ie.hadContent = true
				}
			}
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
	entries := le.history.List()
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

	// Reverse-i-search state.
	var searchMode bool
	var searchQuery []rune
	var searchOrigBuf []rune
	var searchOrigPos int
	var searchMatchIdx int = -1

	renderSearch := func() {
		if le.prevVisualLines > 1 {
			fmt.Fprintf(le.out, "\033[%dA", le.prevVisualLines-1) //nolint:errcheck
		}
		fmt.Fprint(le.out, "\r\033[J") //nolint:errcheck
		match := ""
		if searchMatchIdx >= 0 && searchMatchIdx < len(entries) {
			match = entries[searchMatchIdx]
		}
		fmt.Fprintf(le.out, "(reverse-i-search)`%s': %s", string(searchQuery), match) //nolint:errcheck
		le.prevVisualLines = 1
	}

	for {
		b, err := readByte()
		if err != nil {
			return "", err
		}

		// In search mode, intercept all key handling.
		if searchMode {
			switch {
			case b == '\r' || b == '\n': // Enter — confirm and submit
				searchMode = false
				if searchMatchIdx >= 0 {
					buf = []rune(entries[searchMatchIdx])
				} else {
					buf = searchOrigBuf
				}
				fmt.Fprint(le.out, "\r\n") //nolint:errcheck
				return string(buf), nil
			case b == 0x1B: // Esc — cancel
				searchMode = false
				buf = searchOrigBuf
				pos = searchOrigPos
				render()
			case b == 0x08 || b == 0x7F: // Backspace
				if len(searchQuery) > 0 {
					searchQuery = searchQuery[:len(searchQuery)-1]
					searchMatchIdx = findReverseMatch(entries, string(searchQuery), -1)
				}
				renderSearch()
			case b == 0x12: // Ctrl+R — next older match
				searchMatchIdx = findReverseMatch(entries, string(searchQuery), searchMatchIdx)
				renderSearch()
			case b >= 0x20 && b < 0x7F: // Printable
				searchQuery = append(searchQuery, rune(b))
				searchMatchIdx = findReverseMatch(entries, string(searchQuery), -1)
				renderSearch()
			default:
				// Ignore other control keys in search mode.
			}
			continue
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

		case b == 0x12: // Ctrl+R — reverse incremental search
			searchMode = true
			searchQuery = nil
			searchOrigBuf = buf
			searchOrigPos = pos
			searchMatchIdx = -1
			renderSearch()

		case b == 0x09: // Tab
			if le.completer != nil {
				completions, start := le.completer.Complete(string(buf), pos)
				le.applyCompletions(&buf, &pos, completions, start, prompt)
			}

		case b == 0x03: // Ctrl+C
			fmt.Fprint(le.out, "\r\n") //nolint:errcheck
			return "", &interruptedError{hadContent: len(buf) > 0}

		case b == 0x04: // Ctrl+D
			if len(buf) == 0 {
				fmt.Fprint(le.out, "\r\n") //nolint:errcheck
				return "", io.EOF
			}
			if pos < len(buf) {
				buf = deleteRunes(buf, pos, pos+1)
				render()
			}

		case b == 0x1C: // Ctrl+\ — toggle cooked mode for IME support
			le.cookedMode = true
			return "", errToggleCooked

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
			case 'H': // Home — beginning of line
				pos = 0
				render()
			case 'F': // End — end of line
				pos = len(buf)
				render()
			case '2':
				// Possible bracketed paste sequence: ESC[200~ (start) or
				// ESC[201~ (end), or Insert key: ESC[2~.
				fourth, err := readByte()
				if err != nil {
					return "", err
				}
				if fourth == '~' {
					// Insert key — ignore.
					continue
				}
				if fourth != '0' {
					// Unknown CSI sequence — ignore.
					continue
				}
				fifth, err := readByte()
				if err != nil {
					return "", err
				}
				sixth, err := readByte()
				if err != nil {
					return "", err
				}
				if sixth != '~' {
					// Unknown sequence — ignore.
					continue
				}
				if fifth == '0' {
					// Paste start: ESC[200~
					content, pErr := readPasteContent(readByte)
					if pErr != nil {
						return "", pErr
					}
					// Normalize CRLF and CR to LF.
					content = strings.ReplaceAll(content, "\r\n", "\n")
					content = strings.ReplaceAll(content, "\r", "\n")
					if !strings.Contains(content, "\n") {
						// Single-line paste: insert into buffer and
						// continue editing.
						for _, r := range content {
							buf = insertRune(buf, pos, r)
							pos++
						}
						render()
					} else {
						// Multi-line paste: insert into buffer and
						// return immediately as a single multi-line
						// input so the content is not submitted or
						// added to history line by line.
						for _, r := range content {
							buf = insertRune(buf, pos, r)
							pos++
						}
						if le.prevVisualLines > 1 {
							fmt.Fprintf(le.out, "\033[%dA", le.prevVisualLines-1) //nolint:errcheck
						}
						fmt.Fprint(le.out, "\r\033[J")                                    //nolint:errcheck
						fmt.Fprint(le.out, prompt)                                        //nolint:errcheck
						fmt.Fprint(le.out, strings.ReplaceAll(string(buf), "\n", "\r\n")) //nolint:errcheck
						le.prevVisualLines = visualLineCount(prompt, buf, termW)
						fmt.Fprint(le.out, "\r\n") //nolint:errcheck
						return string(buf), nil
					}
				}
				// fifth == '1' → paste end (ESC[201~), ignore outside paste.
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
			le.markIMEDetected()
			buf = insertRune(buf, pos, r)
			pos++
			render()

		default:
			// Ignore other control bytes.
		}
	}
}

// readPasteContent reads bytes from readByte until the bracketed paste end
// sequence ESC[201~ is encountered. It returns the collected content
// (without the end sequence). If readByte returns an error before the end
// sequence is seen, the partial content and the error are returned.
func readPasteContent(readByte func() (byte, error)) (string, error) {
	var buf []byte
	pasteEnd := []byte{0x1B, '[', '2', '0', '1', '~'}
	for {
		b, err := readByte()
		if err != nil {
			return string(buf), err
		}
		if b != 0x1B {
			buf = append(buf, b)
			continue
		}
		// ESC: check if this is the start of the paste end sequence.
		seq := []byte{b}
		matched := true
		for i := 1; i < len(pasteEnd); i++ {
			nb, err := readByte()
			if err != nil {
				buf = append(buf, seq...)
				return string(buf), err
			}
			seq = append(seq, nb)
			if nb != pasteEnd[i] {
				matched = false
				break
			}
		}
		if matched {
			return string(buf), nil
		}
		// Not the paste end sequence; add consumed bytes to content.
		buf = append(buf, seq...)
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
	// Display options with aligned descriptions.
	fmt.Fprint(le.out, "\r\n") //nolint:errcheck
	maxTextWidth := 0
	for _, c := range completions {
		if w := displayWidth([]rune(c.Text)); w > maxTextWidth {
			maxTextWidth = w
		}
	}
	for _, c := range completions {
		if c.Description != "" {
			textWidth := displayWidth([]rune(c.Text))
			padding := strings.Repeat(" ", maxTextWidth-textWidth)
			fmt.Fprintf(le.out, "  %s%s  %s\r\n", c.Text, padding, c.Description) //nolint:errcheck
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

// termFile returns the underlying *os.File of the editor's input reader and
// whether the assertion succeeded. It is a convenience helper used by the
// terminal control methods.
func (le *DefaultLineEditor) termFile() (*os.File, bool) {
	f, ok := le.in.(*os.File)
	return f, ok
}

// setRawMode saves the current terminal state and enables raw mode on the
// given file descriptor. It returns the saved *xterm.State for later
// restoration.
func setRawMode(in io.Reader) (*xterm.State, error) {
	f, ok := in.(*os.File)
	if !ok {
		return nil, fmt.Errorf("input is not a *os.File")
	}
	fd := int(f.Fd())
	saved, err := xterm.GetState(fd)
	if err != nil {
		return nil, err
	}
	if _, err := xterm.MakeRaw(fd); err != nil {
		return nil, err
	}
	return saved, nil
}

// restoreMode restores the terminal state from the saved *xterm.State.
func restoreMode(in io.Reader, saved *xterm.State) error {
	f, ok := in.(*os.File)
	if !ok {
		return fmt.Errorf("input is not a *os.File")
	}
	return xterm.Restore(int(f.Fd()), saved)
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

// findReverseMatch searches entries in reverse order (newest first) for one
// containing query (case-insensitive). startIdx is the index of the last match;
// search begins from startIdx-1. If startIdx is -1, search starts from the last
// entry. Returns the index of the match, or -1 if no match is found.
func findReverseMatch(entries []string, query string, startIdx int) int {
	if len(query) == 0 || startIdx == 0 {
		return -1
	}
	q := strings.ToLower(query)
	start := startIdx - 1
	if start < 0 {
		start = len(entries) - 1
	}
	for i := start; i >= 0; i-- {
		if strings.Contains(strings.ToLower(entries[i]), q) {
			return i
		}
	}
	return -1
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
