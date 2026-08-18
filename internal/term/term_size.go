// Package term provides terminal size detection across platforms.
package term

// DefaultWidth and DefaultHeight are the fallback terminal dimensions used
// when the real size cannot be determined (e.g. when stdout is not a TTY or
// the ioctl fails).
const (
	DefaultWidth  = 80
	DefaultHeight = 24
)

// GetSize returns the terminal size (columns, rows) of stdout. It returns
// (0, 0) when stdout is not a terminal or the platform does not support the
// TIOCGWINSZ ioctl.
func GetSize() (int, int) {
	return getSize()
}

// GetSizeOrDefault returns the terminal size (columns, rows), falling back to
// 80x24 when the size cannot be determined.
func GetSizeOrDefault() (int, int) {
	w, h := GetSize()
	if w <= 0 {
		w = DefaultWidth
	}
	if h <= 0 {
		h = DefaultHeight
	}
	return w, h
}
