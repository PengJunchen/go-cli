package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestWorktreeAddListRemove exercises the full add → list → remove lifecycle
// for a branch-backed worktree.
func TestWorktreeAddListRemove(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd
	ctx := context.Background()

	wtPath := filepath.Join(dir, "wt-feature")
	branchName := "feature-wt"

	// Add a worktree on a new branch.
	require.NoError(t, git.WorktreeAdd(ctx, wtPath, branchName))

	// List should include the main worktree + the new one.
	wts, err := git.WorktreeList(ctx)
	require.NoError(t, err)
	require.Len(t, wts, 2)

	// The second entry should be the newly created worktree.
	// Resolve symlinks because macOS temp dirs live under /var which is a
	// symlink to /private/var; git reports the resolved path.
	wtPathResolved, err := filepath.EvalSymlinks(wtPath)
	require.NoError(t, err)
	var feature *WorktreeInfo
	for i := range wts {
		if wts[i].Path == wtPathResolved {
			feature = &wts[i]
			break
		}
	}
	require.NotNil(t, feature, "worktree %s (resolved %s) not found in list", wtPath, wtPathResolved)
	assert.NotEmpty(t, feature.Head)
	assert.Contains(t, feature.Branch, branchName)

	// Remove the worktree.
	require.NoError(t, git.WorktreeRemove(ctx, wtPath))

	// List should now have only the main worktree.
	wts, err = git.WorktreeList(ctx)
	require.NoError(t, err)
	assert.Len(t, wts, 1)
}

// TestWorktreeAddDetached verifies that an empty branch creates a detached
// worktree at HEAD.
func TestWorktreeAddDetached(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd
	ctx := context.Background()

	wtPath := filepath.Join(dir, "wt-detached")

	// Empty branch → detached worktree.
	require.NoError(t, git.WorktreeAdd(ctx, wtPath, ""))

	wts, err := git.WorktreeList(ctx)
	require.NoError(t, err)
	require.Len(t, wts, 2)

	wtPathResolved, err := filepath.EvalSymlinks(wtPath)
	require.NoError(t, err)
	var detached *WorktreeInfo
	for i := range wts {
		if wts[i].Path == wtPathResolved {
			detached = &wts[i]
			break
		}
	}
	require.NotNil(t, detached)
	assert.NotEmpty(t, detached.Head)
	assert.Empty(t, detached.Branch, "detached worktree should have no branch")

	require.NoError(t, git.WorktreeRemove(ctx, wtPath))
}

// TestWorktreeListOnEmptyRepo verifies that listing on a repo with only the
// main worktree returns a single entry.
func TestWorktreeListOnEmptyRepo(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	ctx := context.Background()

	wts, err := git.WorktreeList(ctx)
	require.NoError(t, err)
	assert.Len(t, wts, 1)
	assert.NotEmpty(t, wts[0].Path)
}

// TestParseWorktreePorcelain verifies the porcelain parser handles the
// standard format including detached and branch-backed worktrees.
func TestParseWorktreePorcelain(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	out := "worktree /repo/main\nHEAD abc123\nbranch refs/heads/main\n\n" +
		"worktree /repo/feature\nHEAD def456\nbranch refs/heads/feature\n\n" +
		"worktree /repo/detached\nHEAD 789abc\n\n"

	infos := parseWorktreePorcelain(out)
	require.Len(t, infos, 3)

	assert.Equal(t, "/repo/main", infos[0].Path)
	assert.Equal(t, "abc123", infos[0].Head)
	assert.Equal(t, "refs/heads/main", infos[0].Branch)

	assert.Equal(t, "/repo/feature", infos[1].Path)
	assert.Equal(t, "def456", infos[1].Head)
	assert.Equal(t, "refs/heads/feature", infos[1].Branch)

	assert.Equal(t, "/repo/detached", infos[2].Path)
	assert.Equal(t, "789abc", infos[2].Head)
	assert.Empty(t, infos[2].Branch)
}
