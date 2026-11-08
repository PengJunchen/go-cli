//go:build darwin || linux

package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// This test-only adapter exercises the shared state machine using anchored
// syscalls on the development host; it is not used by production Unix edits.
type portableEditTestDirectory struct{ parent *os.File }

type portableEditFaultDirectory struct {
	portableEditDirectory
	failOpen, failCreate, failRename, failRemove *bool
}

func (d *portableEditFaultDirectory) OpenLeaf(name string) (*os.File, error) {
	if d.failOpen != nil && *d.failOpen {
		return nil, errors.New("injected reparse-point rejection")
	}
	return d.portableEditDirectory.OpenLeaf(name)
}

func (d *portableEditFaultDirectory) CreateLeaf(name string) (*os.File, error) {
	if d.failCreate != nil && *d.failCreate {
		return nil, errors.New("injected create failure")
	}
	return d.portableEditDirectory.CreateLeaf(name)
}

func (d *portableEditFaultDirectory) RenameLeaf(file *os.File, from, to string) error {
	if d.failRename != nil && *d.failRename {
		return errors.New("injected rename failure")
	}
	return d.portableEditDirectory.RenameLeaf(file, from, to)
}

func (d *portableEditFaultDirectory) RemoveLeaf(file *os.File, name string) error {
	if d.failRemove != nil && *d.failRemove {
		return errors.New("injected cleanup failure")
	}
	return d.portableEditDirectory.RemoveLeaf(file, name)
}

func openPortableEditTestDirectory(path string) (portableEditDirectory, error) {
	fd, flags, err := openEditRoot()
	if err != nil {
		return nil, err
	}
	parent, err := walkEditDirectory(path, fd, flags)
	if err != nil {
		return nil, err
	}
	return &portableEditTestDirectory{parent: parent}, nil
}

func (d *portableEditTestDirectory) Stat() (os.FileInfo, error) { return d.parent.Stat() }
func (d *portableEditTestDirectory) Close() error               { return d.parent.Close() }
func (d *portableEditTestDirectory) OpenLeaf(name string) (*os.File, error) {
	file, _, err := openEditLeaf(d.parent, name)
	return file, err
}
func (d *portableEditTestDirectory) CreateLeaf(name string) (*os.File, error) {
	fd, err := unix.Openat(int(d.parent.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}
func (d *portableEditTestDirectory) RenameLeaf(_ *os.File, from, to string) error {
	return unix.Renameat(int(d.parent.Fd()), from, int(d.parent.Fd()), to)
}
func (d *portableEditTestDirectory) RemoveLeaf(temp *os.File, name string) error {
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
	return unix.Unlinkat(int(d.parent.Fd()), name, 0)
}

func portableTestCapture(t *testing.T, path string, allowed ...string) editTarget {
	t.Helper()
	target, err := capturePortableEditTarget(path, NewPathWhitelist(allowed), openPortableEditTestDirectory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, target.close()) })
	return target
}

func portableTestRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func portableTestNoTemps(t *testing.T, dir string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, ".write-*"))
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestPortableEditRegularModeAndExactWhitelist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("before old after"), 0o640))
	require.NoError(t, os.Chmod(path, 0o640))
	target := portableTestCapture(t, path, path)
	content, mode := target.snapshot()
	require.Equal(t, "before old after", string(content))
	require.Equal(t, os.FileMode(0o640), mode.Perm())
	require.NoError(t, target.write(context.Background(), []byte("before new after")))
	require.Equal(t, "before new after", portableTestRead(t, path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, mode.Perm(), info.Mode().Perm())
	portableTestNoTemps(t, dir)
}

func TestPortableEditCancellationAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancelLate bool
	}{{"before_create", false}, {"before_commit", true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file")
			require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
			target := portableTestCapture(t, path, dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelLate {
				calls := 0
				ctx = context.WithValue(ctx, preparedEffectStartKey{}, preparedEffectStartGuard(func(start func() error) error {
					calls++
					if calls == 2 {
						cancel()
						return context.Canceled
					}
					return start()
				}))
			} else {
				cancel()
			}
			require.ErrorIs(t, target.write(ctx, []byte("new")), context.Canceled)
			require.Equal(t, "old", portableTestRead(t, path))
			portableTestNoTemps(t, dir)
		})
	}
}

func TestPortableEditRejectsChangedContentIdentityAndParent(t *testing.T) {
	for _, scenario := range []string{"content", "identity", "mode", "parent"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "parent")
			require.NoError(t, os.Mkdir(dir, 0o700))
			path := filepath.Join(dir, "file")
			require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
			target := portableTestCapture(t, path, root)
			switch scenario {
			case "content":
				require.NoError(t, os.WriteFile(path, []byte("changed"), 0o600))
			case "identity":
				replacement := filepath.Join(dir, "replacement")
				require.NoError(t, os.WriteFile(replacement, []byte("old"), 0o600))
				require.NoError(t, os.Rename(replacement, path))
			case "mode":
				require.NoError(t, os.Chmod(path, 0o640))
			case "parent":
				require.NoError(t, os.Rename(dir, filepath.Join(root, "moved")))
				require.NoError(t, os.Mkdir(dir, 0o700))
				require.NoError(t, os.WriteFile(path, []byte("outside"), 0o600))
			}
			require.ErrorIs(t, target.revalidate(), ErrEditConflict)
			require.ErrorIs(t, target.write(context.Background(), []byte("new")), ErrEditConflict)
			if scenario == "parent" {
				require.Equal(t, "outside", portableTestRead(t, path))
				portableTestNoTemps(t, filepath.Join(root, "moved"))
			} else if scenario == "content" {
				require.Equal(t, "changed", portableTestRead(t, path))
			} else {
				require.Equal(t, "old", portableTestRead(t, path))
			}
			portableTestNoTemps(t, dir)
		})
	}
}

func TestPortableEditRejectsSymlinkAndWhitelistEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	path := filepath.Join(root, "inside")
	other := filepath.Join(outside, "outside")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(other, []byte("outside"), 0o600))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(other, link))
	for _, candidate := range []string{other, link} {
		target, err := capturePortableEditTarget(candidate, NewPathWhitelist([]string{root}), openPortableEditTestDirectory)
		require.Error(t, err)
		if target != nil {
			_ = target.close()
		}
	}
	require.Equal(t, "outside", portableTestRead(t, other))

	// A target changed into a symlink after capture is a conflict too.
	target := portableTestCapture(t, path, root)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(other, path))
	require.Error(t, target.write(context.Background(), []byte("new")))
	require.Equal(t, "outside", portableTestRead(t, other))
	portableTestNoTemps(t, root)
}

func TestPortableEditNoParentRedirectAtFinalGuard(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	dir := filepath.Join(root, "parent")
	moved := filepath.Join(root, "moved")
	require.NoError(t, os.Mkdir(dir, 0o700))
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "file"), []byte("outside"), 0o600))
	target := portableTestCapture(t, path, root)
	calls := 0
	ctx := context.WithValue(context.Background(), preparedEffectStartKey{}, preparedEffectStartGuard(func(start func() error) error {
		calls++
		if calls == 2 {
			if err := os.Rename(dir, moved); err != nil {
				return err
			}
			if err := os.Symlink(outside, dir); err != nil {
				return err
			}
		}
		return start()
	}))
	err := target.write(ctx, []byte("new"))
	if err != nil {
		require.True(t, errors.Is(err, ErrEditConflict), "unexpected safe rejection: %v", err)
	}
	require.Equal(t, "outside", portableTestRead(t, filepath.Join(outside, "file")))
	portableTestNoTemps(t, moved)
}

func TestPortableEditRejectsSubstitutedTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	target := portableTestCapture(t, path, dir)
	var substitute string
	calls := 0
	ctx := context.WithValue(context.Background(), preparedEffectStartKey{}, preparedEffectStartGuard(func(start func() error) error {
		calls++
		if err := start(); err != nil {
			return err
		}
		if calls == 1 {
			temps, err := filepath.Glob(filepath.Join(dir, ".write-*"))
			if err != nil || len(temps) != 1 {
				return errors.New("temporary file was not created")
			}
			substitute = temps[0]
			if err := os.Rename(substitute, filepath.Join(dir, "moved-temp")); err != nil {
				return err
			}
			return os.WriteFile(substitute, []byte("substituted"), 0o600)
		}
		return nil
	}))
	err := target.write(ctx, []byte("new"))
	require.Error(t, err)
	require.Equal(t, "old", portableTestRead(t, path))
	require.Equal(t, "substituted", portableTestRead(t, substitute), "cleanup must not unlink a substituted object")
}

func TestPortableEditPlatformErrorsFailClosed(t *testing.T) {
	for _, scenario := range []string{"reparse", "create", "rename", "cleanup"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "file")
			require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
			failOpen, failCreate, failRename, failRemove := false, false, false, false
			open := func(path string) (portableEditDirectory, error) {
				parent, err := openPortableEditTestDirectory(path)
				if err != nil {
					return nil, err
				}
				return &portableEditFaultDirectory{portableEditDirectory: parent, failOpen: &failOpen, failCreate: &failCreate, failRename: &failRename, failRemove: &failRemove}, nil
			}
			target, err := capturePortableEditTarget(path, NewPathWhitelist([]string{dir}), open)
			require.NoError(t, err)
			defer func() { _ = target.close() }()
			switch scenario {
			case "reparse":
				failOpen = true
			case "create":
				failCreate = true
			case "rename":
				failRename = true
			case "cleanup":
				failRename, failRemove = true, true
			}
			err = target.write(context.Background(), []byte("new"))
			require.Error(t, err)
			if scenario == "reparse" {
				require.ErrorIs(t, err, ErrEditConflict)
			}
			if scenario == "cleanup" {
				require.ErrorContains(t, err, "injected cleanup failure")
			} else {
				portableTestNoTemps(t, dir)
			}
			require.Equal(t, "old", portableTestRead(t, path))
		})
	}
}
