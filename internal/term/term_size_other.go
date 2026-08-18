//go:build !darwin && !linux

package term

// getSize returns (0, 0) on platforms that do not support the TIOCGWINSZ
// ioctl. Callers should use GetSizeOrDefault for automatic fallback to 80x24.
func getSize() (int, int) {
	return 0, 0
}
