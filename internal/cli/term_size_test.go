package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGetTerminalSizeNonTerminalReturnsZero verifies that GetTerminalSize
// returns (0, 0) when stdout is not a terminal. In the test environment
// stdout is a pipe, so the TIOCGWINSZ ioctl fails and the function returns
// zeros.
func TestGetTerminalSizeNonTerminalReturnsZero(t *testing.T) {
	w, h := GetTerminalSize()
	assert.Equal(t, 0, w, "width should be 0 for non-terminal stdout")
	assert.Equal(t, 0, h, "height should be 0 for non-terminal stdout")
}

// TestGetTerminalSizeFallbackDefaults verifies that GetTerminalSizeOrDefault
// returns the fallback defaults (80x24) when the terminal size cannot be
// determined.
func TestGetTerminalSizeFallbackDefaults(t *testing.T) {
	w, h := GetTerminalSizeOrDefault()
	assert.Equal(t, defaultTermWidth, w)
	assert.Equal(t, defaultTermHeight, h)
}
