package tools

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// setupWorktreeManager creates a git repo, a base dir for worktrees, and a
// WorktreeManager pointing at them. Returns the manager, the repo dir, and the
// base dir.
func setupWorktreeManager(t *testing.T) (*WorktreeManager, string, string) {
	t.Helper()
	git := setupGitRepo(t)
	dir := git.cwd
	baseDir := filepath.Join(dir, ".worktrees")
	require.NoError(t, os.MkdirAll(baseDir, 0o755))
	mgr := NewWorktreeManager(git, baseDir)
	return mgr, dir, baseDir
}

// TestWorktreeManager_CreateGetRemove exercises the create → get → remove
// lifecycle for a single session.
func TestWorktreeManager_CreateGetRemove(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, baseDir := setupWorktreeManager(t)
	ctx := context.Background()

	path, err := mgr.CreateForSession(ctx, "sess-1", "cli-")
	require.NoError(t, err)

	// Path should be under baseDir/sessionID.
	assert.Contains(t, path, filepath.Join(baseDir, "sess-1"))

	// GetForSession returns the same path.
	assert.Equal(t, path, mgr.GetForSession("sess-1"))

	// Remove.
	require.NoError(t, mgr.RemoveForSession(ctx, "sess-1"))

	// GetForSession now returns empty.
	assert.Empty(t, mgr.GetForSession("sess-1"))
}

// TestWorktreeManager_CreateIdempotent verifies that creating a worktree for
// an existing session returns the existing path without error.
func TestWorktreeManager_CreateIdempotent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, _ := setupWorktreeManager(t)
	ctx := context.Background()

	path1, err := mgr.CreateForSession(ctx, "sess-1", "cli-")
	require.NoError(t, err)

	// Second create should return the same path, not error.
	path2, err := mgr.CreateForSession(ctx, "sess-1", "cli-")
	require.NoError(t, err)
	assert.Equal(t, path1, path2)
}

// TestWorktreeManager_List verifies that List returns all active session
// worktrees.
func TestWorktreeManager_List(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, _ := setupWorktreeManager(t)
	ctx := context.Background()

	_, err := mgr.CreateForSession(ctx, "sess-a", "cli-")
	require.NoError(t, err)
	_, err = mgr.CreateForSession(ctx, "sess-b", "cli-")
	require.NoError(t, err)

	sessions := mgr.List()
	assert.Len(t, sessions, 2)
	assert.Contains(t, sessions, "sess-a")
	assert.Contains(t, sessions, "sess-b")
}

// TestWorktreeManager_Cleanup verifies that Cleanup removes all session
// worktrees.
func TestWorktreeManager_Cleanup(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, _ := setupWorktreeManager(t)
	ctx := context.Background()

	_, err := mgr.CreateForSession(ctx, "sess-1", "cli-")
	require.NoError(t, err)
	_, err = mgr.CreateForSession(ctx, "sess-2", "cli-")
	require.NoError(t, err)

	require.Len(t, mgr.List(), 2)

	require.NoError(t, mgr.Cleanup(ctx))

	assert.Empty(t, mgr.List())
}

// TestWorktreeManager_RemoveNonexistent verifies that removing a session that
// has no worktree is a no-op.
func TestWorktreeManager_RemoveNonexistent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, _ := setupWorktreeManager(t)
	ctx := context.Background()

	// Should not error on missing session.
	require.NoError(t, mgr.RemoveForSession(ctx, "nonexistent"))
}

// TestWorktreePersistence verifies that the session→worktree mapping is
// persisted to sessions.json and recovered when a new WorktreeManager is
// created over the same baseDir (simulating a process restart).
func TestWorktreePersistence(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, baseDir := setupWorktreeManager(t)
	ctx := context.Background()

	path, err := mgr.CreateForSession(ctx, "sess-persist", "cli-")
	require.NoError(t, err)

	// The sessions.json file must exist under baseDir.
	_, err = os.Stat(filepath.Join(baseDir, "sessions.json"))
	require.NoError(t, err)

	// Simulate a restart: a new manager over the same baseDir and git tool.
	mgr2 := NewWorktreeManager(mgr.gitTool, baseDir)

	// The persisted session must be restored.
	assert.Equal(t, path, mgr2.GetForSession("sess-persist"))
	assert.Contains(t, mgr2.List(), "sess-persist")
	assert.Equal(t, path, mgr2.List()["sess-persist"])
}

// TestWorktreeOrphanScan verifies that ScanOrphans reports directories under
// baseDir that are not registered in the session mapping, while ignoring
// registered worktrees.
func TestWorktreeOrphanScan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, baseDir := setupWorktreeManager(t)
	ctx := context.Background()

	// Create a registered worktree.
	_, err := mgr.CreateForSession(ctx, "sess-real", "cli-")
	require.NoError(t, err)

	// Manually create a directory under baseDir that is NOT registered.
	orphanDir := filepath.Join(baseDir, "orphan-dir")
	require.NoError(t, os.MkdirAll(orphanDir, 0o755))

	orphans, err := mgr.ScanOrphans()
	require.NoError(t, err)

	// Only the unregistered directory should be reported as an orphan.
	require.Len(t, orphans, 1)
	abs, err := filepath.Abs(orphanDir)
	require.NoError(t, err)
	assert.Equal(t, abs, orphans[0])
}

// TestWorktreeManager_RemoveErrorPropagation verifies that a failure from the
// underlying git worktree remove is propagated by RemoveForSession (instead of
// being swallowed) and that the failing session remains registered so it can be
// retried. It also verifies that Cleanup surfaces the error (AC-6).
func TestWorktreeManager_RemoveErrorPropagation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, baseDir := setupWorktreeManager(t)
	ctx := context.Background()

	// Inject a session pointing at a path that is not a valid worktree so that
	// WorktreeRemove fails. This is the same-package test, so the unexported
	// map is accessible.
	mgr.mu.Lock()
	mgr.sessions["sess-broken"] = worktreeSession{
		SessionID: "sess-broken",
		Path:      filepath.Join(baseDir, "does-not-exist"),
	}
	mgr.mu.Unlock()

	// RemoveForSession must propagate the error rather than swallowing it.
	err := mgr.RemoveForSession(ctx, "sess-broken")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sess-broken")

	// The failed session must remain registered (retryable), since removal
	// did not succeed.
	assert.NotEmpty(t, mgr.GetForSession("sess-broken"))

	// Cleanup must also surface the error.
	cleanupErr := mgr.Cleanup(ctx)
	require.Error(t, cleanupErr)
}

// TestWorktreeSignalHandler_CleanupOnSignal verifies that delivering a signal
// to the signal channel triggers worktree cleanup (AC-5). The goroutine started
// by StartSignalCleanup calls Cleanup when it receives from sigCh.
func TestWorktreeSignalHandler_CleanupOnSignal(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, _ := setupWorktreeManager(t)
	ctx := context.Background()

	// Create a worktree so there is something to clean up.
	_, err := mgr.CreateForSession(ctx, "sess-sig", "cli-")
	require.NoError(t, err)
	require.Len(t, mgr.List(), 1)

	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	mgr.StartSignalCleanup(sigCh, done)

	// Simulate a signal delivery. The buffered channel ensures the send does
	// not block even if the goroutine has not yet reached the select.
	sigCh <- syscall.SIGINT

	// Wait for the goroutine to process the signal and clean up.
	require.Eventually(t, func() bool {
		return len(mgr.List()) == 0
	}, 2*time.Second, 10*time.Millisecond, "worktree was not cleaned up after signal")

	assert.Empty(t, mgr.List())
}

// TestWorktreeSignalHandler_NoCleanupOnDone verifies that closing the done
// channel causes the signal-handler goroutine to exit WITHOUT cleaning up,
// leaving the worktree in place for the caller to clean up via the normal
// shutdown path (AC-5).
func TestWorktreeSignalHandler_NoCleanupOnDone(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, _ := setupWorktreeManager(t)
	ctx := context.Background()

	_, err := mgr.CreateForSession(ctx, "sess-done", "cli-")
	require.NoError(t, err)
	require.Len(t, mgr.List(), 1)

	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	mgr.StartSignalCleanup(sigCh, done)

	// Normal shutdown: close done instead of sending a signal. The goroutine
	// picks the done case and exits without touching the worktree.
	close(done)

	// Give the goroutine a moment to observe done and exit.
	time.Sleep(50 * time.Millisecond)

	// The worktree must still be registered — the caller cleans it up.
	assert.Len(t, mgr.List(), 1, "worktree should not be cleaned up on done")

	// Explicit cleanup so the test does not leave a worktree behind.
	require.NoError(t, mgr.Cleanup(ctx))
	assert.Empty(t, mgr.List())
}

// TestWorktreeCleanup_Idempotent verifies that calling Cleanup multiple times
// only performs the actual removal once (M-2). The second call returns the
// saved error without re-executing removal, avoiding noisy duplicate logs when
// a signal handler and normal shutdown race.
func TestWorktreeCleanup_Idempotent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mgr, _, _ := setupWorktreeManager(t)
	ctx := context.Background()

	_, err := mgr.CreateForSession(ctx, "sess-once", "cli-")
	require.NoError(t, err)
	require.Len(t, mgr.List(), 1)

	// First cleanup removes the worktree.
	require.NoError(t, mgr.Cleanup(ctx))
	assert.Empty(t, mgr.List())

	// Second cleanup is a no-op and returns the saved (nil) error.
	require.NoError(t, mgr.Cleanup(ctx))
	assert.Empty(t, mgr.List())
}
