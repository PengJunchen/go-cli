//go:build darwin

package cli

import (
	"os"

	"golang.org/x/sys/unix"
)

func cancelReaderSupportsTTYFile(file *os.File) bool {
	return file.Name() != "/dev/tty" || file.Fd() < uintptr(unix.FD_SETSIZE)
}
