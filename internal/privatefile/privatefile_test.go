package privatefile

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteAtomicCreatesPrivateParentAndReplacesWideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "private", "secret.json")
	require.NoError(t, WriteAtomic(path, []byte("old-secret")))

	require.NoError(t, os.Chmod(path, 0o644))
	require.NoError(t, WriteAtomic(path, []byte("new-secret")))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new-secret", string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestFingerprintFileTracksSameSizeHistoricalChanges(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("file fingerprints are supported on Darwin and Linux")
	}
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	missing, err := FingerprintFile(path)
	require.NoError(t, err)
	assert.False(t, missing.Exists)

	require.NoError(t, os.WriteFile(path, []byte("first"), 0o600))
	before, err := FingerprintFile(path)
	require.NoError(t, err)
	require.True(t, before.Exists)
	assert.Equal(t, int64(len("first")), before.Size)

	// A same-length overwrite must change the ctime fingerprint so a cached
	// verified hash cursor cannot hide historical tampering.
	time.Sleep(2 * time.Millisecond)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, writeErr := f.WriteAt([]byte("other"), 0)
	require.NoError(t, writeErr)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())

	after, err := FingerprintFile(path)
	require.NoError(t, err)
	assert.Equal(t, before.Size, after.Size)
	assert.NotEqual(t, before, after)
}

func TestFingerprintFileRejectsNonRegularPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested")
	require.NoError(t, os.Mkdir(path, 0o700))
	_, err := FingerprintFile(path)
	assert.Error(t, err)
}

func TestWriteAtomicRejectsAccessibleParentWithoutChangingIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))
	path := filepath.Join(dir, "secret")
	require.NoError(t, os.WriteFile(path, []byte("old-secret"), 0o644))
	require.NoError(t, os.Chmod(path, 0o644))

	err := WriteAtomic(path, []byte("new-secret"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "new-secret")
	assert.Contains(t, err.Error(), "private")

	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "old-secret", string(data))
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	dirInfo, statErr := os.Stat(dir)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o755), dirInfo.Mode().Perm())
}

func TestWriteAtomicRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "linked")
	require.NoError(t, os.Mkdir(target, 0o700))
	require.NoError(t, os.Symlink(target, link))

	err := WriteAtomic(filepath.Join(link, "secret"), []byte("new-secret"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "new-secret")
	_, statErr := os.Lstat(filepath.Join(target, "secret"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestWriteAtomicReplacesDestinationSymlinkWithoutFollowingIt(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	target := filepath.Join(dir, "target")
	path := filepath.Join(dir, "secret")
	require.NoError(t, os.WriteFile(target, []byte("target-secret"), 0o600))
	require.NoError(t, os.Symlink(target, path))

	require.NoError(t, WriteAtomic(path, []byte("new-secret")))

	info, err := os.Lstat(path)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "atomic replace should replace the link itself")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new-secret", string(data))
	targetData, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "target-secret", string(targetData), "the symlink target must remain untouched")
}

func TestWriteAtomicWriteFailurePreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.Chmod(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("old-secret"), 0o644))
	require.NoError(t, os.Chmod(path, 0o644))
	writeErr := errors.New("write failed")
	ops := defaultWriteOps()
	ops.write = func(dst io.Writer, data []byte) error {
		_, _ = dst.Write(data[:2])
		return writeErr
	}

	err := writeAtomic(path, []byte("new-secret"), ops)
	require.ErrorIs(t, err, writeErr)
	assert.NotContains(t, err.Error(), "new-secret")
	assertExistingFile(t, path, "old-secret", 0o644)
}

func TestWriteAtomicRenameFailurePreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.Chmod(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("old-secret"), 0o644))
	require.NoError(t, os.Chmod(path, 0o644))
	renameErr := errors.New("rename failed")
	ops := defaultWriteOps()
	ops.rename = func(string, string) error { return renameErr }

	err := writeAtomic(path, []byte("new-secret"), ops)
	require.ErrorIs(t, err, renameErr)
	assert.NotContains(t, err.Error(), "new-secret")
	assertExistingFile(t, path, "old-secret", 0o644)
}

func TestWriteAtomicDirectorySyncFailureReportsCommittedReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.Chmod(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte("old-secret"), 0o600))
	syncErr := errors.New("directory sync failed")
	ops := defaultWriteOps()
	ops.syncDir = func(string) error { return syncErr }

	err := writeAtomic(path, []byte("new-secret"), ops)
	require.ErrorIs(t, err, syncErr)
	assert.Contains(t, err.Error(), "file replaced")
	assert.NotContains(t, err.Error(), "new-secret")
	assertExistingFile(t, path, "new-secret", 0o600)
}

func TestWithExclusiveLockKeepsPrivateSidecar(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "auth.json")
	called := false
	require.NoError(t, WithExclusiveLock(context.Background(), path, func() error {
		called = true
		return nil
	}))
	assert.True(t, called)

	lockInfo, err := os.Lstat(path + ".lock")
	require.NoError(t, err)
	assert.True(t, lockInfo.Mode().IsRegular())
	assert.Equal(t, os.FileMode(0o600), lockInfo.Mode().Perm())
}

func TestWithExclusiveLockRejectsSymlinkOrInsecureSidecar(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		path := filepath.Join(dir, "auth.json")
		target := filepath.Join(dir, "target")
		require.NoError(t, os.WriteFile(target, nil, 0o600))
		require.NoError(t, os.Symlink(target, path+".lock"))

		called := false
		err := WithExclusiveLock(context.Background(), path, func() error {
			called = true
			return nil
		})
		require.Error(t, err)
		assert.False(t, called)
		info, statErr := os.Stat(target)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("insecure mode", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		path := filepath.Join(dir, "auth.json")
		require.NoError(t, os.WriteFile(path+".lock", nil, 0o644))
		require.NoError(t, os.Chmod(path+".lock", 0o644))

		called := false
		err := WithExclusiveLock(context.Background(), path, func() error {
			called = true
			return nil
		})
		require.Error(t, err)
		assert.False(t, called)
		info, statErr := os.Stat(path + ".lock")
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	})
}

func TestWithExclusiveLockWaitIsCancellable(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	path := filepath.Join(dir, "auth.json")
	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- WithExclusiveLock(context.Background(), path, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("first lock holder did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	called := false
	err := WithExclusiveLock(ctx, path, func() error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, called)
	close(release)
	require.NoError(t, <-firstDone)
}

func TestWithAuditExclusiveLockSupportsOwnerSharedParent(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("advisory file locks are supported on Darwin and Linux")
	}
	dir := filepath.Join(t.TempDir(), "shared")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))
	path := filepath.Join(dir, "audit.jsonl")

	for i := 0; i < 2; i++ {
		require.NoError(t, WithAuditExclusiveLock(context.Background(), path, func() error { return nil }))
	}
	parentInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), parentInfo.Mode().Perm(), "the existing project directory is not chmodded")

	lockDir := filepath.Join(dir, ".go-cli-audit-locks")
	lockDirInfo, err := os.Lstat(lockDir)
	require.NoError(t, err)
	assert.True(t, lockDirInfo.IsDir())
	assert.Equal(t, os.FileMode(0o700), lockDirInfo.Mode().Perm())
	lockFiles, err := os.ReadDir(lockDir)
	require.NoError(t, err)
	require.Len(t, lockFiles, 1)
	lockPath := filepath.Join(lockDir, lockFiles[0].Name())
	lockInfo, err := os.Lstat(lockPath)
	require.NoError(t, err)
	assert.True(t, lockInfo.Mode().IsRegular())
	assert.Equal(t, os.FileMode(0o600), lockInfo.Mode().Perm())
	firstLockInfo := lockInfo
	require.NoError(t, WithAuditExclusiveLock(context.Background(), path, func() error { return nil }))
	lockInfo, err = os.Lstat(lockPath)
	require.NoError(t, err)
	assert.True(t, os.SameFile(firstLockInfo, lockInfo), "repeated calls use the same stable lock inode")

	calls := 0
	err = WithExclusiveLock(context.Background(), path, func() error {
		calls++
		return nil
	})
	require.Error(t, err, "the strict private-file lock contract remains unchanged")
	assert.Zero(t, calls)
}

func TestWithAuditExclusiveLockSupportsGroupReadOnlyParent(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("advisory file locks are supported on Darwin and Linux")
	}
	dir := filepath.Join(t.TempDir(), "shared")
	require.NoError(t, os.Mkdir(dir, 0o750))
	require.NoError(t, os.Chmod(dir, 0o750))
	called := false
	err := WithAuditExclusiveLock(context.Background(), filepath.Join(dir, "audit.jsonl"), func() error {
		called = true
		return nil
	})
	require.NoError(t, err)
	assert.True(t, called)
	parentInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), parentInfo.Mode().Perm())
}

func TestWithAuditExclusiveLockRejectsWideExistingTargetInSharedParent(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("advisory file locks are supported on Darwin and Linux")
	}
	dir := filepath.Join(t.TempDir(), "shared")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Chmod(dir, 0o755))
	path := filepath.Join(dir, "audit.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("existing audit data"), 0o644))
	require.NoError(t, os.Chmod(path, 0o644))
	called := false
	err := WithAuditExclusiveLock(context.Background(), path, func() error {
		called = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, called)
	assert.Contains(t, err.Error(), "0600")
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	assert.Equal(t, "existing audit data", string(data))
	info, statErr := os.Stat(path)
	require.NoError(t, statErr)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
}

func TestWithAuditExclusiveLockRejectsUnsafeExistingTargets(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("advisory file locks are supported on Darwin and Linux")
	}
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		path := filepath.Join(dir, "audit.jsonl")
		target := filepath.Join(dir, "target")
		require.NoError(t, os.WriteFile(target, []byte("unchanged"), 0o600))
		require.NoError(t, os.Symlink(target, path))
		called := false
		err := WithAuditExclusiveLock(context.Background(), path, func() error {
			called = true
			return nil
		})
		require.Error(t, err)
		assert.False(t, called)
		data, readErr := os.ReadFile(target)
		require.NoError(t, readErr)
		assert.Equal(t, "unchanged", string(data))
		info, statErr := os.Stat(target)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("non-regular target", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.Chmod(dir, 0o700))
		called := false
		err := WithAuditExclusiveLock(context.Background(), dir, func() error {
			called = true
			return nil
		})
		require.Error(t, err)
		assert.False(t, called)
	})
}

func TestWithAuditExclusiveLockRejectsUnsafeParents(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("advisory file locks are supported on Darwin and Linux")
	}
	for _, mode := range []os.FileMode{0o775, 0o777} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "shared")
			require.NoError(t, os.Mkdir(dir, mode))
			require.NoError(t, os.Chmod(dir, mode))
			called := false
			err := WithAuditExclusiveLock(context.Background(), filepath.Join(dir, "audit.jsonl"), func() error {
				called = true
				return nil
			})
			require.Error(t, err)
			assert.False(t, called)
			_, statErr := os.Lstat(filepath.Join(dir, ".go-cli-audit-locks"))
			assert.ErrorIs(t, statErr, os.ErrNotExist)
		})
	}

	t.Run("symlink parent", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		link := filepath.Join(root, "linked")
		require.NoError(t, os.Mkdir(target, 0o755))
		require.NoError(t, os.Chmod(target, 0o755))
		require.NoError(t, os.Symlink(target, link))
		called := false
		err := WithAuditExclusiveLock(context.Background(), filepath.Join(link, "audit.jsonl"), func() error {
			called = true
			return nil
		})
		require.Error(t, err)
		assert.False(t, called)
	})

	t.Run("preexisting insecure lock directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "shared")
		require.NoError(t, os.Mkdir(dir, 0o755))
		require.NoError(t, os.Chmod(dir, 0o755))
		lockDir := filepath.Join(dir, auditLockDirectoryName)
		require.NoError(t, os.Mkdir(lockDir, 0o755))
		require.NoError(t, os.Chmod(lockDir, 0o755))
		called := false
		err := WithAuditExclusiveLock(context.Background(), filepath.Join(dir, "audit.jsonl"), func() error {
			called = true
			return nil
		})
		require.Error(t, err)
		assert.False(t, called)
		lockInfo, statErr := os.Stat(lockDir)
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o755), lockInfo.Mode().Perm(), "an existing lock directory is never chmodded")
	})
}

func TestWithAuditExclusiveLockFallsBackToStrictLockWhenOwnershipIsUnknown(t *testing.T) {
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		t.Skip("ownership validation is supported on Darwin and Linux")
	}
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o700))
	called := false
	err := WithAuditExclusiveLock(context.Background(), filepath.Join(dir, "audit.jsonl"), func() error {
		called = true
		return nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported on this platform")
	assert.False(t, called, "unsupported lock platforms fail closed through the existing strict path")
}

func TestWriteFullHandlesShortWrites(t *testing.T) {
	w := &shortWriter{}
	require.NoError(t, writeFull(w, []byte("complete")))
	assert.Equal(t, "complete", w.buf.String())

	err := writeFull(zeroWriter{}, []byte("x"))
	require.ErrorIs(t, err, io.ErrShortWrite)
}

func assertExistingFile(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(data))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, mode, info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "temporary files must be removed after a failed replace")
}

type shortWriter struct {
	buf strings.Builder
}

func (w *shortWriter) Write(data []byte) (int, error) {
	if len(data) > 1 {
		data = data[:1]
	}
	return w.buf.Write(data)
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
