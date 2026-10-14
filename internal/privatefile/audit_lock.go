package privatefile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const auditLockDirectoryName = ".go-cli-audit-locks"

// WithAuditExclusiveLock serializes audit-file updates without requiring the
// audit file's parent directory itself to be private. A safe owner-owned
// 0700 parent keeps the historical sibling lock path; a non-writable 0750 or
// 0755 parent uses a stable 0600 lock file inside a 0700 child directory. In
// the latter case, an existing audit file must also be owner-owned and 0600;
// the readable parent reveals only the audit filename, not its contents.
// Group/other-writable, symlink, and foreign-owned parents fail closed.
func WithAuditExclusiveLock(ctx context.Context, path string, update func() error) error {
	if err := validateLockArgs(ctx, path, update); err != nil {
		return err
	}
	if !auditLockOwnershipSupported() {
		return WithExclusiveLock(ctx, path, update)
	}
	lockPath, targetPath, requirePrivateTarget, err := auditLockPath(path)
	if err != nil {
		return err
	}
	return withLockFile(ctx, lockPath, func() error {
		if err := validateAuditTarget(targetPath, requirePrivateTarget); err != nil {
			return err
		}
		return update()
	})
}

func auditLockPath(path string) (string, string, bool, error) {
	absPath, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", "", false, fmt.Errorf("privatefile: resolve audit lock target: %w", err)
	}
	parent := filepath.Dir(absPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", "", false, fmt.Errorf("privatefile: create audit target parent: %w", err)
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", "", false, fmt.Errorf("privatefile: inspect audit target parent: %w", err)
	}
	if !parentInfo.IsDir() {
		return "", "", false, errors.New("privatefile: audit target parent must be a real directory")
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", "", false, fmt.Errorf("privatefile: resolve audit target parent: %w", err)
	}
	parent = resolvedParent
	absPath = filepath.Join(parent, filepath.Base(absPath))
	if !ownedByCurrentUser(parentInfo) {
		return "", "", false, errors.New("privatefile: audit target parent must be owned by the current user")
	}
	parentMode := parentInfo.Mode().Perm()
	if parentMode&0o022 != 0 {
		return "", "", false, errors.New("privatefile: audit target parent must not be writable by group or others")
	}
	if parentMode&0o077 == 0 {
		return absPath + ".lock", absPath, false, nil
	}

	lockDir := filepath.Join(parent, auditLockDirectoryName)
	if err := os.Mkdir(lockDir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", "", false, fmt.Errorf("privatefile: create private audit lock directory: %w", err)
	} else if err == nil {
		if err := os.Chmod(lockDir, 0o700); err != nil {
			return "", "", false, fmt.Errorf("privatefile: set private audit lock directory mode: %w", err)
		}
	}
	lockDirInfo, err := os.Lstat(lockDir)
	if err != nil {
		return "", "", false, fmt.Errorf("privatefile: inspect private audit lock directory: %w", err)
	}
	if !lockDirInfo.IsDir() || lockDirInfo.Mode().Perm() != 0o700 || !ownedByCurrentUser(lockDirInfo) {
		return "", "", false, errors.New("privatefile: audit lock directory is not an owned private directory")
	}
	pathHash := sha256.Sum256([]byte(absPath))
	lockName := hex.EncodeToString(pathHash[:]) + ".lock"
	return filepath.Join(lockDir, lockName), absPath, true, nil
}

func validateAuditTarget(path string, requirePrivate bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("privatefile: inspect audit target: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("privatefile: audit target must be a regular file")
	}
	if !ownedByCurrentUser(info) {
		return errors.New("privatefile: audit target must be owned by the current user")
	}
	if requirePrivate && info.Mode().Perm() != 0o600 {
		return errors.New("privatefile: audit target permissions must be 0600 in a shared directory")
	}
	return nil
}
