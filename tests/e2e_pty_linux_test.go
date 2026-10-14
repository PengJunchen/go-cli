//go:build e2e && linux

package tests

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func openLongTaskPTY(t *testing.T) (*os.File, *os.File, func()) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("cannot allocate a PTY at /dev/ptmx: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		_ = master.Close()
		t.Skipf("cannot unlock allocated PTY slave: %v", err)
	}
	ptyNumber, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		_ = master.Close()
		t.Skipf("cannot read allocated PTY slave number: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptyNumber), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		t.Skipf("cannot open allocated PTY slave: %v", err)
	}
	if err := unix.IoctlSetWinsize(int(master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 40, Col: 120}); err != nil {
		_ = slave.Close()
		_ = master.Close()
		t.Skipf("cannot set allocated PTY size: %v", err)
	}
	return master, slave, func() {
		_ = slave.Close()
		_ = master.Close()
	}
}
