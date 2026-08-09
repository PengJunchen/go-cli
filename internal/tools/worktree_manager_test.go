package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
