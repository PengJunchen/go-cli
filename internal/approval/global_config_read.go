package approval

import (
	"errors"
	"fmt"
	"io"
	"os"
)

var errGlobalConfigFileChanged = errors.New("approval: global config changed during read")

type globalConfigReadOps struct {
	lstat func(string) (os.FileInfo, error)
	open  func(string) (*os.File, error)
}

// ReadGlobalConfigFile securely opens and reads a private user-global config
// file. The file must be a regular, non-symlink 0600 file owned by the current
// user, and the path must still name the opened inode after its bytes are read.
func ReadGlobalConfigFile(path string) ([]byte, error) {
	return readGlobalConfigFileWithOps(path, globalConfigReadOps{lstat: os.Lstat, open: os.Open})
}

func readGlobalConfigFileWithOps(path string, ops globalConfigReadOps) ([]byte, error) {
	if path == "" {
		return nil, errors.New("approval: global config path is empty")
	}
	before, err := ops.lstat(path)
	if err != nil {
		return nil, fmt.Errorf("approval: inspect global config: %w", err)
	}
	if err := validateGlobalConfigInfo(before, path); err != nil {
		return nil, err
	}

	file, err := ops.open(path)
	if err != nil {
		return nil, fmt.Errorf("approval: open global config: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	opened, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("approval: inspect opened global config: %w", err)
	}
	if !os.SameFile(before, opened) {
		return nil, errGlobalConfigFileChanged
	}
	if err := validateGlobalConfigInfo(opened, path); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("approval: read global config: %w", err)
	}
	readInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("approval: inspect read global config: %w", err)
	}
	if !os.SameFile(opened, readInfo) {
		return nil, errGlobalConfigFileChanged
	}
	if err := validateGlobalConfigInfo(readInfo, path); err != nil {
		return nil, err
	}
	after, err := ops.lstat(path)
	if err != nil {
		return nil, fmt.Errorf("approval: recheck global config path: %w", err)
	}
	if !os.SameFile(readInfo, after) {
		return nil, errGlobalConfigFileChanged
	}
	if err := validateGlobalConfigInfo(after, path); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("approval: close global config: %w", err)
	}
	closed = true
	return data, nil
}

func validateGlobalConfigInfo(info os.FileInfo, path string) error {
	if info == nil || !info.Mode().IsRegular() {
		return errors.New("approval: global config must be a regular, non-symlink file")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("global config %s has permissions %o, expected 0600", path, info.Mode().Perm())
	}
	return validateFileOwner(info, path)
}
