//go:build darwin || linux

package tools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type anchoredEditTarget struct {
	path       string
	parentPath string
	root       string
	name       string
	parent     *os.File
	parentInfo os.FileInfo
	dirFlags   int
	original   *os.File
	info       os.FileInfo
	content    []byte
	whitelist  PathWhitelist
}

var _ editTarget = (*anchoredEditTarget)(nil)

var errEditWhitelistChanged = errors.New("edit: path whitelist changed or rejected target")
var errEditNonRegular = errors.New("edit: unsupported non-regular target")

func editWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

func editWhitelistRoot(wl PathWhitelist, target string) (string, error) {
	if wl.invalid {
		return "", errors.New("edit: invalid path whitelist")
	}
	if wl.unrestricted() {
		return "", nil
	}
	for _, root := range wl.paths {
		if editWithinRoot(root, target) {
			// Exact-file scopes are checked through the captured parent and
			// no-follow leaf descriptor, without resolving the leaf again here.
			if root == target {
				return root, nil
			}
			resolved, err := filepath.EvalSymlinks(root)
			if err != nil {
				return "", fmt.Errorf("edit: resolve whitelist root: %w", err)
			}
			if resolved != root {
				return "", fmt.Errorf("%w: whitelist root changed", errEditWhitelistChanged)
			}
			return root, nil
		}
	}
	return "", fmt.Errorf("%w: path escapes the path whitelist", errEditWhitelistChanged)
}

func openEditLeaf(parent *os.File, name string) (*os.File, os.FileInfo, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("edit: open target: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("edit: stat target: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%w %q", errEditNonRegular, name)
	}
	return file, info, nil
}

func captureEditTarget(path string, whitelist PathWhitelist) (editTarget, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("edit: resolve target: %w", err)
	}
	name := filepath.Base(absolute)
	if name == "." || name == ".." || name == string(filepath.Separator) {
		return nil, fmt.Errorf("edit: invalid target name %q", name)
	}
	parentPath, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("edit: resolve parent: %w", err)
	}
	root, err := editWhitelistRoot(whitelist, filepath.Join(parentPath, name))
	if err != nil {
		return nil, err
	}
	rootFD, dirFlags, err := openEditRoot()
	if err != nil {
		return nil, err
	}
	parent, err := walkEditDirectory(parentPath, rootFD, dirFlags)
	if err != nil {
		return nil, err
	}
	target := &anchoredEditTarget{path: absolute, parentPath: parentPath, root: root, name: name, parent: parent, dirFlags: dirFlags, whitelist: whitelist}
	defer func() {
		if err != nil {
			_ = target.close()
		}
	}()
	target.parentInfo, err = parent.Stat()
	if err != nil {
		return nil, fmt.Errorf("edit: stat parent: %w", err)
	}
	target.original, target.info, err = openEditLeaf(parent, name)
	if err != nil {
		return nil, err
	}
	target.content, err = io.ReadAll(target.original)
	if err != nil {
		return nil, fmt.Errorf("edit: read target: %w", err)
	}
	// Recheck both namespace and bytes after the initial read.
	if err = target.revalidate(); err != nil {
		return nil, err
	}
	return target, nil
}

func (t *anchoredEditTarget) snapshot() ([]byte, os.FileMode) { return t.content, t.info.Mode() }

func (t *anchoredEditTarget) conflict(reason string, cause error) error {
	return &EditConflictError{Path: t.path, Reason: reason, Cause: cause}
}

func (t *anchoredEditTarget) revalidate() error {
	// Resolve the user path again, while all data access remains anchored to
	// the parent descriptor. These checks cannot make rename a CAS operation.
	parentPath, err := filepath.EvalSymlinks(filepath.Dir(t.path))
	if err != nil {
		if os.IsNotExist(err) {
			return t.conflict("path", err)
		}
		return fmt.Errorf("edit: resolve parent: %w", err)
	}
	if parentPath != t.parentPath {
		return t.conflict("path", nil)
	}
	root, err := editWhitelistRoot(t.whitelist, filepath.Join(parentPath, t.name))
	if err != nil {
		if errors.Is(err, errEditWhitelistChanged) || os.IsNotExist(err) {
			return t.conflict("path", err)
		}
		return fmt.Errorf("edit: recheck whitelist: %w", err)
	}
	if root != t.root {
		return t.conflict("path", nil)
	}
	parent, err := openEditDirectory(parentPath, t.dirFlags)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return t.conflict("path", err)
		}
		return fmt.Errorf("edit: re-open parent: %w", err)
	}
	parentInfo, statErr := parent.Stat()
	closeErr := parent.Close()
	if statErr != nil {
		return fmt.Errorf("edit: stat parent: %w", statErr)
	}
	if closeErr != nil {
		return fmt.Errorf("edit: close parent: %w", closeErr)
	}
	if !os.SameFile(t.parentInfo, parentInfo) {
		return t.conflict("parent_identity", nil)
	}
	file, info, err := openEditLeaf(t.parent, t.name)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) || errors.Is(err, errEditNonRegular) {
			return t.conflict("identity", err)
		}
		return fmt.Errorf("edit: re-open target: %w", err)
	}
	defer file.Close() //nolint:errcheck // read-only descriptor
	if !os.SameFile(t.info, info) {
		return t.conflict("identity", nil)
	}
	if t.info.Mode() != info.Mode() {
		return t.conflict("mode", nil)
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("edit: re-read target: %w", err)
	}
	after, err := file.Stat()
	if err != nil {
		return fmt.Errorf("edit: restat target: %w", err)
	}
	if after.Mode() != t.info.Mode() {
		return t.conflict("mode", nil)
	}
	if !bytes.Equal(t.content, content) {
		return t.conflict("content", nil)
	}
	return nil
}

func (t *anchoredEditTarget) write(ctx context.Context, content []byte) error {
	var tmp *os.File
	var name string
	err := startPreparedEffect(ctx, func() error {
		for i := 0; i < 32; i++ {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return fmt.Errorf("edit: generate temporary name: %w", err)
			}
			name = ".write-" + hex.EncodeToString(random[:])
			fd, err := unix.Openat(int(t.parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
			if errors.Is(err, unix.EEXIST) {
				continue
			}
			if err != nil {
				return fmt.Errorf("edit: create temporary file: %w", err)
			}
			tmp = os.NewFile(uintptr(fd), name)
			return nil
		}
		return errors.New("edit: cannot allocate temporary name")
	})
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(t.parent.Fd()), name, 0) }()
	defer func() { _ = tmp.Close() }()
	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("edit: write temporary file: %w", err)
	}
	if err := tmp.Chmod(t.info.Mode()); err != nil {
		return fmt.Errorf("edit: chmod temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("edit: sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("edit: close temporary file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.revalidate(); err != nil {
		return err
	}
	return startPreparedEffect(ctx, func() error {
		if err := unix.Renameat(int(t.parent.Fd()), name, int(t.parent.Fd()), t.name); err != nil {
			return fmt.Errorf("edit: rename temporary file: %w", err)
		}
		return nil
	})
}

func (t *anchoredEditTarget) close() error {
	var err error
	if t.original != nil {
		err = t.original.Close()
	}
	if t.parent != nil {
		err = errors.Join(err, t.parent.Close())
	}
	return err
}
