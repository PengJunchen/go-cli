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
	"runtime"
	"strings"
)

// portableEditDirectory exposes only single-component operations relative to
// one already-open directory. Platform adapters must not fall back to paths.
type portableEditDirectory interface {
	Stat() (os.FileInfo, error)
	OpenLeaf(string) (*os.File, error)
	CreateLeaf(string) (*os.File, error)
	RenameLeaf(*os.File, string, string) error
	RemoveLeaf(*os.File, string) error
	Close() error
}

type portableEditDirectoryOpener func(string) (portableEditDirectory, error)

type portableEditTarget struct {
	path       string
	parentPath string
	name       string
	root       string
	whitelist  PathWhitelist
	open       portableEditDirectoryOpener
	parent     portableEditDirectory
	parentInfo os.FileInfo
	original   *os.File
	info       os.FileInfo
	content    []byte
}

var _ editTarget = (*portableEditTarget)(nil)

func portableEditSamePath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func portableEditWhitelistRoot(wl PathWhitelist, path string) (string, error) {
	if wl.invalid {
		return "", errors.New("edit: invalid path whitelist")
	}
	if wl.unrestricted() {
		return "", nil
	}
	for _, root := range wl.paths {
		rel, err := filepath.Rel(root, path)
		if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
			continue
		}
		if portableEditSamePath(root, path) {
			return root, nil
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", fmt.Errorf("edit: resolve whitelist root: %w", err)
		}
		if !portableEditSamePath(root, resolved) {
			return "", errors.New("edit: whitelist root changed")
		}
		return root, nil
	}
	return "", errors.New("edit: path escapes the path whitelist")
}

func capturePortableEditTarget(path string, whitelist PathWhitelist, open portableEditDirectoryOpener) (editTarget, error) {
	if open == nil {
		return nil, errors.New("edit: anchored directory opener is unavailable")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("edit: resolve target: %w", err)
	}
	name := filepath.Base(absolute)
	if name == "." || name == ".." || name == string(filepath.Separator) || filepath.VolumeName(name) != "" {
		return nil, fmt.Errorf("edit: invalid target name %q", name)
	}
	parentPath, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, fmt.Errorf("edit: resolve parent: %w", err)
	}
	root, err := portableEditWhitelistRoot(whitelist, filepath.Join(parentPath, name))
	if err != nil {
		return nil, err
	}
	parent, err := open(parentPath)
	if err != nil {
		return nil, fmt.Errorf("edit: open parent: %w", err)
	}
	target := &portableEditTarget{path: absolute, parentPath: parentPath, name: name, root: root, whitelist: whitelist, open: open, parent: parent}
	accepted := false
	defer func() {
		if !accepted {
			_ = target.close()
		}
	}()
	target.parentInfo, err = parent.Stat()
	if err != nil {
		return nil, fmt.Errorf("edit: stat parent directory: %w", err)
	}
	if !target.parentInfo.IsDir() {
		return nil, errors.New("edit: parent is not a directory")
	}
	target.original, err = parent.OpenLeaf(name)
	if err != nil {
		return nil, fmt.Errorf("edit: open target: %w", err)
	}
	target.info, err = target.original.Stat()
	if err != nil {
		return nil, fmt.Errorf("edit: stat target: %w", err)
	}
	if !target.info.Mode().IsRegular() {
		return nil, errors.New("edit: target is not a regular file")
	}
	target.content, err = io.ReadAll(target.original)
	if err != nil {
		return nil, fmt.Errorf("edit: read target: %w", err)
	}
	if err = target.revalidate(); err != nil {
		return nil, err
	}
	accepted = true
	return target, nil
}

func (t *portableEditTarget) snapshot() ([]byte, os.FileMode) { return t.content, t.info.Mode() }

func (t *portableEditTarget) conflict(reason string, cause error) error {
	return &EditConflictError{Path: t.path, Reason: reason, Cause: cause}
}

func (t *portableEditTarget) revalidate() error {
	parentPath, err := filepath.EvalSymlinks(filepath.Dir(t.path))
	if err != nil || !portableEditSamePath(parentPath, t.parentPath) {
		return t.conflict("path", err)
	}
	root, err := portableEditWhitelistRoot(t.whitelist, filepath.Join(parentPath, t.name))
	if err != nil || !portableEditSamePath(root, t.root) {
		return t.conflict("whitelist", err)
	}
	parent, err := t.open(parentPath)
	if err != nil {
		return t.conflict("parent_identity", err)
	}
	parentInfo, statErr := parent.Stat()
	closeErr := parent.Close()
	if statErr != nil || closeErr != nil {
		return t.conflict("parent_identity", errors.Join(statErr, closeErr))
	}
	if !os.SameFile(t.parentInfo, parentInfo) {
		return t.conflict("parent_identity", nil)
	}
	file, err := t.parent.OpenLeaf(t.name)
	if err != nil {
		return t.conflict("identity", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(t.info, info) {
		return t.conflict("identity", err)
	}
	if info.Mode() != t.info.Mode() {
		return t.conflict("mode", nil)
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("edit: re-read target: %w", err)
	}
	after, err := file.Stat()
	if err != nil || after.Mode() != t.info.Mode() {
		return t.conflict("mode", err)
	}
	if !bytes.Equal(t.content, content) {
		return t.conflict("content", nil)
	}
	return nil
}

// portableEditOpenMatchingTemporary never follows a replacement symlink and
// refuses to operate on a different file occupying the temporary name.
func portableEditOpenMatchingTemporary(parent portableEditDirectory, temp *os.File, name string) (*os.File, error) {
	expected, err := temp.Stat()
	if err != nil {
		return nil, err
	}
	current, err := parent.OpenLeaf(name)
	if err != nil {
		return nil, err
	}
	info, err := current.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		_ = current.Close()
		return nil, errors.Join(errors.New("temporary file identity changed"), err)
	}
	return current, nil
}

func (t *portableEditTarget) write(ctx context.Context, content []byte) (resultErr error) {
	if ctx == nil {
		return errors.New("edit: context is required")
	}
	var tmp *os.File
	var name string
	err := startPreparedEffect(ctx, func() error {
		for i := 0; i < 32; i++ {
			var random [16]byte
			if _, err := rand.Read(random[:]); err != nil {
				return fmt.Errorf("edit: generate temporary name: %w", err)
			}
			name = ".write-" + hex.EncodeToString(random[:])
			var err error
			tmp, err = t.parent.CreateLeaf(name)
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return err
		}
		return errors.New("edit: cannot allocate temporary name")
	})
	if err != nil {
		return fmt.Errorf("edit: create temporary file: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if cleanupErr := t.parent.RemoveLeaf(tmp, name); cleanupErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("edit: remove temporary file: %w", cleanupErr))
			}
			if closeErr := tmp.Close(); closeErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("edit: close temporary file: %w", closeErr))
			}
			return
		}
		_ = tmp.Close()
	}()
	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("edit: write temporary file: %w", err)
	}
	if err := tmp.Chmod(t.info.Mode()); err != nil {
		return fmt.Errorf("edit: chmod temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("edit: sync temporary file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.revalidate(); err != nil {
		return err
	}
	current, err := portableEditOpenMatchingTemporary(t.parent, tmp, name)
	if err != nil {
		return t.conflict("temporary_identity", err)
	}
	currentInfo, statErr := current.Stat()
	currentBytes, readErr := io.ReadAll(current)
	closeErr := current.Close()
	if statErr != nil || readErr != nil || closeErr != nil || currentInfo.Mode() != t.info.Mode() || !bytes.Equal(currentBytes, content) {
		return t.conflict("temporary_content", errors.Join(statErr, readErr, closeErr))
	}
	if err := startPreparedEffect(ctx, func() error { return t.parent.RenameLeaf(tmp, name, t.name) }); err != nil {
		return fmt.Errorf("edit: rename temporary file: %w", err)
	}
	committed = true
	return nil
}

func (t *portableEditTarget) close() error {
	var err error
	if t.original != nil {
		err = t.original.Close()
	}
	if t.parent != nil {
		err = errors.Join(err, t.parent.Close())
	}
	return err
}
