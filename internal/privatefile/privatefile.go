// Package privatefile writes sensitive files through a private, atomic replace.
package privatefile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

var errInsecureParent = errors.New("parent directory is accessible by group or others")

type writeOps struct {
	write   func(io.Writer, []byte) error
	rename  func(string, string) error
	syncDir func(string) error
}

// WriteAtomic writes data to path using a unique 0600 sibling file, syncs it,
// renames it into place, and syncs the parent directory. Missing parent
// directories are created as 0700. Existing parents that allow group or other
// access are rejected without changing their permissions.
func WriteAtomic(path string, data []byte) error {
	return writeAtomic(path, data, defaultWriteOps())
}

func writeAtomic(path string, data []byte, ops writeOps) error {
	if path == "" {
		return errors.New("privatefile: path is empty")
	}
	dir := filepath.Dir(path)
	if err := ensurePrivateParent(dir); err != nil {
		return fmt.Errorf("privatefile: secure parent directory: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".go-cli-private-*")
	if err != nil {
		return fmt.Errorf("privatefile: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() //nolint:errcheck // cleanup after rename is a no-op.
	defer func() { _ = tmp.Close() }()        //nolint:errcheck // explicit close errors are returned below.

	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("privatefile: set temporary file permissions: %w", err)
	}
	if err := ops.write(tmp, data); err != nil {
		return fmt.Errorf("privatefile: write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("privatefile: sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("privatefile: close temporary file: %w", err)
	}
	if err := ops.rename(tmpName, path); err != nil {
		return fmt.Errorf("privatefile: replace file: %w", err)
	}
	if err := ops.syncDir(dir); err != nil {
		return fmt.Errorf("privatefile: file replaced but parent directory sync failed: %w", err)
	}
	return nil
}

func ensurePrivateParent(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect parent directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("parent path is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errInsecureParent
	}
	return nil
}

func writeFull(dst io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := dst.Write(data)
		if n < 0 || n > len(data) {
			return errors.New("writer returned an invalid byte count")
		}
		if n > 0 {
			data = data[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func defaultWriteOps() writeOps {
	return writeOps{
		write:   writeFull,
		rename:  os.Rename,
		syncDir: syncDirectory,
	}
}
