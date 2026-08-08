//go:build !darwin && !linux

package cli

// GetTerminalSize returns (0, 0) on platforms that do not support the
// TIOCGWINSZ ioctl. Callers should use GetTerminalSizeOrDefault for
// automatic fallback to 80x24.
func GetTerminalSize() (int, int) {
	return 0, 0
}
