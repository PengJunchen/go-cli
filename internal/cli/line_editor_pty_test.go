//go:build darwin || linux

package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	xterm "golang.org/x/term"
)

// TestRawMode_PTY_RoundTrip verifies that setRawMode/restoreMode correctly
// toggle raw mode on a real PTY using golang.org/x/term, and that the terminal
// remains usable after the round-trip.
func TestRawMode_PTY_RoundTrip(t *testing.T) {
	slave, cleanup := openPTY(t)
	defer cleanup()

	fd := int(slave.Fd())
	require.True(t, xterm.IsTerminal(fd), "PTY slave must be a terminal")

	// Enter raw mode via the line editor's helper.
	saved, err := setRawMode(slave)
	require.NoError(t, err)
	require.NotNil(t, saved)

	// The terminal must still report as a terminal while in raw mode.
	assert.True(t, xterm.IsTerminal(fd))

	// Restore the original state.
	require.NoError(t, restoreMode(slave, saved))

	// The terminal must remain usable after restore.
	assert.True(t, xterm.IsTerminal(fd))

	// A second round-trip should also succeed, proving the terminal was not
	// left in a broken state by the first cycle.
	saved2, err := setRawMode(slave)
	require.NoError(t, err)
	require.NotNil(t, saved2)
	require.NoError(t, restoreMode(slave, saved2))
}
