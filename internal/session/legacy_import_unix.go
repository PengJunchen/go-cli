//go:build darwin || linux

package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func publishImportedSession(ctx context.Context, directory, filename string, payload []byte) error {
	root, err := openSessionDirectoryNoFollow(directory)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	random, err := randomPhysicalSessionID()
	if err != nil {
		return err
	}
	staging := ".import-" + random
	fd, err := unix.Openat(int(root.Fd()), staging, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), staging)
	defer func() { _ = file.Close(); _ = unix.Unlinkat(int(root.Fd()), staging, 0) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	written, err := file.Write(payload)
	if err != nil {
		return err
	}
	if written != len(payload) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := unix.Linkat(int(root.Fd()), staging, int(root.Fd()), filename, 0); err != nil {
		return fmt.Errorf("session: publish import without overwrite: %w", err)
	}
	if err := root.Sync(); err != nil {
		return fmt.Errorf("session: import %s is visible but directory sync is uncertain: %w", filename, err)
	}
	if err := unix.Unlinkat(int(root.Fd()), staging, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return root.Sync()
}
