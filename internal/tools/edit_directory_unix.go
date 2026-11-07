//go:build darwin || linux

package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const editDirectoryBaseFlags = unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

func unsupportedEditRootOpen(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS)
}

func statEditDirectory(fd int) error {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return fmt.Errorf("edit: stat directory: %w", err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("edit: not a directory: %w", unix.ENOTDIR)
	}
	return nil
}

func openReadOnlyEditRoot(cause error) (int, int, error) {
	flags := unix.O_RDONLY | editDirectoryBaseFlags
	fd, err := unix.Open("/", flags, 0)
	if err == nil {
		err = statEditDirectory(fd)
		if err == nil {
			return fd, flags, nil
		}
		_ = unix.Close(fd)
	}
	return -1, 0, fmt.Errorf("edit: open read-only root after search descriptor unavailable: %w", errors.Join(cause, err))
}

// openEditRoot chooses search-only descriptors when the kernel recognizes the
// flag and supports fstat on the descriptor. Old kernels use the previous
// read-only walk, which cannot traverse search-only directories.
func openEditRoot() (int, int, error) {
	flags := editDirectorySearchFlag | editDirectoryBaseFlags
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		cause := fmt.Errorf("edit: open search root: %w", err)
		if unsupportedEditRootOpen(err) {
			return openReadOnlyEditRoot(cause)
		}
		return -1, 0, cause
	}
	status, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		_ = unix.Close(fd)
		return -1, 0, fmt.Errorf("edit: get root descriptor flags: %w", err)
	}
	if status&editDirectorySearchFlag == 0 {
		_ = unix.Close(fd)
		return openReadOnlyEditRoot(errors.New("edit: kernel did not retain search descriptor flag"))
	}
	if err := statEditDirectory(fd); err != nil {
		_ = unix.Close(fd)
		if unsupportedEditRootStat(err) {
			return openReadOnlyEditRoot(err)
		}
		return -1, 0, err
	}
	return fd, flags, nil
}

func openEditDirectory(path string, flags int) (*os.File, error) {
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return nil, fmt.Errorf("edit: open root: %w", err)
	}
	if err := statEditDirectory(fd); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	return walkEditDirectory(path, fd, flags)
}

// walkEditDirectory owns rootFD and walks the canonical absolute path one
// component at a time, verifying each opened descriptor is a directory.
func walkEditDirectory(path string, rootFD, flags int) (*os.File, error) {
	if !filepath.IsAbs(path) {
		_ = unix.Close(rootFD)
		return nil, fmt.Errorf("edit: directory is not absolute: %q", path)
	}
	fd := rootFD
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		if part == "" {
			continue
		}
		next, openErr := unix.Openat(fd, part, flags, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, fmt.Errorf("edit: open directory %q: %w", path, openErr)
		}
		if err := statEditDirectory(next); err != nil {
			_ = unix.Close(next)
			return nil, fmt.Errorf("edit: verify directory %q: %w", path, err)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}
