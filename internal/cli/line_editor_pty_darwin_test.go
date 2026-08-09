//go:build darwin

package cli

import (
	"bytes"
	"os"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// openPTY creates a pseudo-terminal pair and returns the slave end as an
// *os.File (which reports true for xterm.IsTerminal).
func openPTY(t *testing.T) (*os.File, func()) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)

	// Grant access to the slave PTY (equivalent to grantpt) and unlock it
	// (equivalent to unlockpt) before opening it.
	require.NoError(t, unix.IoctlSetInt(int(master.Fd()), unix.TIOCPTYGRANT, 0))
	require.NoError(t, unix.IoctlSetInt(int(master.Fd()), unix.TIOCPTYUNLK, 0))

	// TIOCPTYGNAME returns the full path of the slave device.
	var buf [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&buf[0])))
	require.Zero(t, errno, "TIOCPTYGNAME ioctl failed")

	idx := bytes.IndexByte(buf[:], 0)
	require.True(t, idx > 0, "TIOCPTYGNAME returned empty slave name")

	slave, err := os.OpenFile(string(buf[:idx]), os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)

	return slave, func() {
		slave.Close()
		master.Close()
	}
}
