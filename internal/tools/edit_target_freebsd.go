package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type freebsdEditDirectory struct{ file *os.File }

func captureEditTarget(path string, whitelist PathWhitelist) (editTarget, error) {
	return capturePortableEditTarget(path, whitelist, openFreeBSDEditDirectory)
}

func openFreeBSDEditDirectory(path string) (portableEditDirectory, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("edit: directory is not absolute: %q", path)
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", flags, 0)
	if err != nil {
		return nil, fmt.Errorf("edit: open root: %w", err)
	}
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(path), "/"), "/") {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(fd, component, flags, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			return nil, fmt.Errorf("edit: open directory component: %w", openErr)
		}
		fd = next
	}
	return &freebsdEditDirectory{file: os.NewFile(uintptr(fd), path)}, nil
}

func (d *freebsdEditDirectory) Stat() (os.FileInfo, error) { return d.file.Stat() }
func (d *freebsdEditDirectory) Close() error               { return d.file.Close() }

func (d *freebsdEditDirectory) OpenLeaf(name string) (*os.File, error) {
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func (d *freebsdEditDirectory) CreateLeaf(name string) (*os.File, error) {
	fd, err := unix.Openat(int(d.file.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func (d *freebsdEditDirectory) RenameLeaf(_ *os.File, from, to string) error {
	return unix.Renameat(int(d.file.Fd()), from, int(d.file.Fd()), to)
}

func (d *freebsdEditDirectory) RemoveLeaf(temp *os.File, name string) error {
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	current, err := portableEditOpenMatchingTemporary(d, temp, name)
	if err != nil {
		return err
	}
	if err := current.Close(); err != nil {
		return err
	}
	return unix.Unlinkat(int(d.file.Fd()), name, 0)
}
