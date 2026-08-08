package cli

// Terminal size fallback defaults used when the real terminal size cannot
// be determined (e.g. when stdout is not a TTY or the ioctl fails).
const (
	defaultTermWidth  = 80
	defaultTermHeight = 24
)

// GetTerminalSizeOrDefault returns the terminal size (columns, rows),
// falling back to 80x24 when the size cannot be determined (e.g. when
// stdout is not a terminal or the platform does not support the
// TIOCGWINSZ ioctl).
func GetTerminalSizeOrDefault() (int, int) {
	w, h := GetTerminalSize()
	if w <= 0 {
		w = defaultTermWidth
	}
	if h <= 0 {
		h = defaultTermHeight
	}
	return w, h
}
