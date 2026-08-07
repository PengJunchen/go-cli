//go:build !darwin

package tui

// getTerminalSize is a stub on non-Darwin platforms; callers fall back to
// 80x24 via DefaultTerminalSizeProvider. (Raw mode and single-key reading are
// now handled by bubbletea, so the termios machinery that used to live here
// has been removed.)
func getTerminalSize() (int, int) { return 0, 0 }
