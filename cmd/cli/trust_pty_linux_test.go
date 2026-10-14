//go:build linux

package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func openTrustPTY(t *testing.T) (*os.File, *os.File, func()) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Skip("PTY tests are skipped in CI environments")
	}
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)
	require.NoError(t, unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0))
	ptyNumber, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	require.NoError(t, err)
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptyNumber), os.O_RDWR|unix.O_NOCTTY, 0)
	require.NoError(t, err)
	return master, slave, func() {
		_ = slave.Close()
		_ = master.Close()
	}
}
