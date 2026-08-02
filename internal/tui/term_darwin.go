//go:build darwin

package tui

import (
	"syscall"
	"unsafe"
)

// termios mirrors the Darwin struct termios layout used by TIOCGETA /
// TIOCSETAW. Only the flags needed for raw mode are manipulated; the rest of
// the structure is preserved byte-for-byte so restore is lossless.
type termios struct {
	Iflag  uint32
	Oflag  uint32
	Cflag  uint32
	Lflag  uint32
	Cc     [20]byte
	Ispeed uint32
	Ospeed uint32
}

const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETAW
)

// makeRaw places the terminal attached to fd into raw mode and returns a
// restore token. Raw mode disables canonical input (ICANON), echo (ECHO),
// signal generation (ISIG) and input processing (IXON/ICRNL) so single-key
// reads from ReadRune get punctuation and control bytes as-is.
func makeRaw(fd int) (*termios, error) {
	var old termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd), uintptr(ioctlGetTermios), uintptr(unsafe.Pointer(&old))); errno != 0 {
		return nil, errno
	}
	raw := old
	raw.Iflag &^= syscall.IXON | syscall.ICRNL
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd), uintptr(ioctlSetTermios), uintptr(unsafe.Pointer(&raw))); errno != 0 {
		return nil, errno
	}
	return &old, nil
}

// restoreRaw returns the terminal identified by fd to the termios snapshot
// captured by makeRaw.
func restoreRaw(fd int, old *termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd), uintptr(ioctlSetTermios), uintptr(unsafe.Pointer(old)))
	if errno != 0 {
		return errno
	}
	return nil
}