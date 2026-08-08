//go:build linux

package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// openPTY creates a pseudo-terminal pair and returns the slave end as an
// *os.File (which reports true for term.IsTerminal).
func openPTY(t *testing.T) (*os.File, func()) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)

	// Unlock the slave PTY.
	require.NoError(t, unix.IoctlSetInt(int(master.Fd()), unix.TIOCSPTLCK, 0))

	// Get the slave device number and construct its path.
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	require.NoError(t, err)

	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)

	return slave, func() {
		slave.Close()
		master.Close()
	}
}
