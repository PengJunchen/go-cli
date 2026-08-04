package tui

import (
	"log/slog"
	"os"
)

// TerminalSizeProvider provides terminal dimensions.
type TerminalSizeProvider interface {
	// Width returns the terminal width in columns.
	Width() int
	// Height returns the terminal height in rows.
	Height() int
}

// DefaultTerminalSizeProvider uses ioctl TIOCGWINSZ on Unix systems to
// determine the terminal size. When stdout is not a TTY, the ioctl fails, or
// the reported width is zero, it falls back to 80x24.
type DefaultTerminalSizeProvider struct{}

// compile-time assertion that DefaultTerminalSizeProvider satisfies
// TerminalSizeProvider.
var _ TerminalSizeProvider = (*DefaultTerminalSizeProvider)(nil)

// NewDefaultTerminalSizeProvider returns a ready-to-use provider.
func NewDefaultTerminalSizeProvider() *DefaultTerminalSizeProvider {
	return &DefaultTerminalSizeProvider{}
}

// defaultTerminalWidth is the fallback column count when the real terminal
// width cannot be determined.
const defaultTerminalWidth = 80

// defaultTerminalHeight is the fallback row count when the real terminal
// height cannot be determined.
const defaultTerminalHeight = 24

// Width returns the terminal width in columns, falling back to
// defaultTerminalWidth when the terminal size cannot be determined.
func (d *DefaultTerminalSizeProvider) Width() int {
	if !isStdoutTerminal() {
		return defaultTerminalWidth
	}
	w, _ := getTerminalSize()
	if w <= 0 {
		return defaultTerminalWidth
	}
	slog.Debug("tui.terminal.size", "width", w)
	return w
}

// Height returns the terminal height in rows, falling back to
// defaultTerminalHeight when the terminal size cannot be determined.
func (d *DefaultTerminalSizeProvider) Height() int {
	if !isStdoutTerminal() {
		return defaultTerminalHeight
	}
	_, h := getTerminalSize()
	if h <= 0 {
		return defaultTerminalHeight
	}
	return h
}

// isStdoutTerminal reports whether stdout is a terminal (TTY). The terminal
// size ioctl only works on character devices, so non-TTY output (pipes, files)
// causes the provider to use the fallback dimensions.
func isStdoutTerminal() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
