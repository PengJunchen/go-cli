package tools

import (
	"errors"

	"golang.org/x/sys/unix"
)

const editDirectorySearchFlag = unix.O_PATH

func unsupportedEditRootStat(err error) bool {
	// Linux supports fstat on an O_PATH descriptor starting with 3.6. On an
	// older kernel a recognized O_PATH descriptor returns EBADF from fstat.
	return errors.Is(err, unix.EBADF)
}
