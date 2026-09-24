//go:build darwin || linux

package session

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openSessionDirectoryNoFollow(directory string) (*os.File, error) {
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("session: open session directory without following symlinks: %w", err)
	}
	return os.NewFile(uintptr(fd), directory), nil
}

func openSessionFileNoFollow(directory, filename string, flags int, mode os.FileMode) (*os.File, error) {
	if filename == "" || filepath.Base(filename) != filename {
		return nil, fmt.Errorf("session: invalid session filename %q", filename)
	}
	directoryFile, err := openSessionDirectoryNoFollow(directory)
	if err != nil {
		return nil, err
	}
	defer directoryFile.Close()

	openFlags := unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	switch flags & (os.O_WRONLY | os.O_RDWR) {
	case os.O_WRONLY:
		openFlags |= unix.O_WRONLY
	case os.O_RDWR:
		openFlags |= unix.O_RDWR
	default:
		openFlags |= unix.O_RDONLY
	}
	if flags&os.O_CREATE != 0 {
		openFlags |= unix.O_CREAT
	}
	if flags&os.O_EXCL != 0 {
		openFlags |= unix.O_EXCL
	}
	if flags&os.O_TRUNC != 0 {
		openFlags |= unix.O_TRUNC
	}
	if flags&os.O_APPEND != 0 {
		openFlags |= unix.O_APPEND
	}
	fileFD, err := unix.Openat(int(directoryFile.Fd()), filename, openFlags, uint32(mode.Perm()))
	if err != nil {
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fileFD, &stat); err != nil {
		_ = unix.Close(fileFD)
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fileFD)
		return nil, fmt.Errorf("session: physical session entry %q is not a regular file", filename)
	}
	return os.NewFile(uintptr(fileFD), filepath.Join(directory, filename)), nil
}
