//go:build linux

package term

import (
	"os"
	"syscall"
	"unsafe"
)

// winsize mirrors the struct winsize used by TIOCGWINSZ on Linux.
type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

// getSize queries the terminal size of stdout via the TIOCGWINSZ ioctl.
// It returns (0, 0) when the ioctl fails.
func getSize() (int, int) {
	ws := &winsize{}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		os.Stdout.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(ws)))
	if errno != 0 {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}
