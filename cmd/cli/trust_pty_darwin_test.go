//go:build darwin

package main

import (
	"bytes"
	"os"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func openTrustPTY(t *testing.T) (*os.File, *os.File, func()) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)
	require.NoError(t, unix.IoctlSetInt(int(master.Fd()), unix.TIOCPTYGRANT, 0))
	require.NoError(t, unix.IoctlSetInt(int(master.Fd()), unix.TIOCPTYUNLK, 0))

	var name [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, master.Fd(), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0])))
	require.Zero(t, errno, "TIOCPTYGNAME ioctl failed")
	index := bytes.IndexByte(name[:], 0)
	require.Positive(t, index, "TIOCPTYGNAME returned empty slave name")
	slave, err := os.OpenFile(string(name[:index]), os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)
	return master, slave, func() {
		_ = slave.Close()
		_ = master.Close()
	}
}
