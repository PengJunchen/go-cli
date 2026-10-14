package privatefile

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// WithExclusiveLock runs update while holding an OS-backed exclusive lock for
// path. The stable 0600 sidecar is kept after release so concurrent processes
// always lock the same inode while path itself is atomically replaced.
func WithExclusiveLock(ctx context.Context, path string, update func() error) error {
	if err := validateLockArgs(ctx, path, update); err != nil {
		return err
	}
	if err := ensurePrivateParent(filepath.Dir(path)); err != nil {
		return fmt.Errorf("privatefile: secure lock parent directory: %w", err)
	}
	return withLockFile(ctx, path+".lock", update)
}

func validateLockArgs(ctx context.Context, path string, update func() error) error {
	if ctx == nil {
		return errors.New("privatefile: lock context is required")
	}
	if path == "" {
		return errors.New("privatefile: lock path is empty")
	}
	if update == nil {
		return errors.New("privatefile: locked update is required")
	}
	return ctx.Err()
}

func withLockFile(ctx context.Context, lockPath string, update func() error) error {
	file, err := openLockFile(lockPath)
	if err != nil {
		return fmt.Errorf("privatefile: open lock file: %w", err)
	}
	unlock, err := lockOS(ctx, file)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("privatefile: acquire lock: %w", err)
	}

	var updateErr error
	func() {
		defer func() {
			if unlockErr := unlock(); unlockErr != nil {
				updateErr = errors.Join(updateErr, fmt.Errorf("privatefile: release lock: %w", unlockErr))
			}
			if closeErr := file.Close(); closeErr != nil {
				updateErr = errors.Join(updateErr, fmt.Errorf("privatefile: close lock file: %w", closeErr))
			}
		}()
		updateErr = update()
	}()
	return updateErr
}

func openLockFile(path string) (*os.File, error) {
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if err := validateLockInfo(info); err != nil {
				return nil, err
			}
			file, err := os.OpenFile(path, os.O_RDWR, 0)
			if err != nil {
				return nil, err
			}
			if err := validateOpenedLockFile(path, file); err != nil {
				_ = file.Close()
				return nil, err
			}
			return file, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}

		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := validateOpenedLockFile(path, file); err != nil {
			_ = file.Close()
			return nil, err
		}
		return file, nil
	}
}

func validateLockInfo(info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("lock path must be a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return errors.New("lock file permissions must be 0600")
	}
	return nil
}

func validateOpenedLockFile(path string, file *os.File) error {
	openedInfo, err := file.Stat()
	if err != nil {
		return err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := validateLockInfo(pathInfo); err != nil {
		return err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(pathInfo, openedInfo) {
		return errors.New("lock file changed while opening")
	}
	return nil
}
