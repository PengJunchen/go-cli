package cli

import "github.com/pengjunchen/go-cli/internal/term"

// Terminal size fallback defaults used when the real terminal size cannot
// be determined (e.g. when stdout is not a TTY or the ioctl fails).
const (
	defaultTermWidth  = term.DefaultWidth
	defaultTermHeight = term.DefaultHeight
)

// GetTerminalSize returns the terminal size (columns, rows) of stdout.
// It returns (0, 0) when stdout is not a terminal or the ioctl fails.
func GetTerminalSize() (int, int) {
	return term.GetSize()
}

// GetTerminalSizeOrDefault returns the terminal size (columns, rows),
// falling back to 80x24 when the size cannot be determined.
func GetTerminalSizeOrDefault() (int, int) {
	return term.GetSizeOrDefault()
}
