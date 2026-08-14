package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// GitTool.Revert
// ---------------------------------------------------------------------------

func TestGitRevert(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	// Create a commit to revert.
	writeFileInDir(t, dir, "file.txt", "content\n")
	runGitInDir(t, dir, "add", "file.txt")
	runGitInDir(t, dir, "commit", "-m", "add file.txt")
	hash := strings.TrimSpace(runGitInDir(t, dir, "rev-parse", "HEAD"))

	err := git.Revert(context.Background(), hash)
	require.NoError(t, err)

	// The file should be removed by the revert.
	_, err = os.Stat(filepath.Join(dir, "file.txt"))
	assert.True(t, os.IsNotExist(err))

	// A revert commit should exist.
	logOut := runGitInDir(t, dir, "log", "--oneline", "-1")
	assert.Contains(t, logOut, "Revert")
}

func TestGitRevertEmptyCommit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	err := git.Revert(context.Background(), "")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Revert — tool wrapper
// ---------------------------------------------------------------------------

func TestGitRevertTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "file.txt", "content\n")
	runGitInDir(t, dir, "add", "file.txt")
	runGitInDir(t, dir, "commit", "-m", "add file.txt")
	hash := strings.TrimSpace(runGitInDir(t, dir, "rev-parse", "HEAD"))

	tool := NewGitRevertTool(git)
	assert.Equal(t, "git_revert", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"commit": hash},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "reverted:")
}

func TestGitRevertToolMissingCommit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitRevertTool(git)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

func TestGitRevertToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitRevertTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"commit": "abc123"},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Reset
// ---------------------------------------------------------------------------

func TestGitResetHard(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	// Modify the tracked README.md.
	writeFileInDir(t, dir, "README.md", "modified\n")

	err := git.Reset(context.Background(), "hard")
	require.NoError(t, err)

	// The file should be reverted to the original content.
	data, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, "# Test\n", string(data))
}

func TestGitResetMixed(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	// Stage a change.
	writeFileInDir(t, dir, "README.md", "modified\n")
	runGitInDir(t, dir, "add", "README.md")

	err := git.Reset(context.Background(), "mixed")
	require.NoError(t, err)

	// The change should remain in the working tree but be unstaged.
	data, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, "modified\n", string(data))

	files, err := git.Status(context.Background())
	require.NoError(t, err)
	for _, f := range files {
		if f.File == "README.md" {
			assert.False(t, f.Staged, "README.md should not be staged after mixed reset")
		}
	}
}

func TestGitResetInvalidMode(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	err := git.Reset(context.Background(), "invalid")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid reset mode")
}

// ---------------------------------------------------------------------------
// GitTool.Reset — tool wrapper
// ---------------------------------------------------------------------------

func TestGitResetTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "README.md", "modified\n")

	tool := NewGitResetTool(git)
	assert.Equal(t, "git_reset", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"mode": "hard"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "reset --hard")

	// Verify the file was reset.
	data, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	assert.Equal(t, "# Test\n", string(data))
}

func TestGitResetToolDefaultMode(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "README.md", "modified\n")

	tool := NewGitResetTool(git)
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "reset --hard")
}

func TestGitResetToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitResetTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"mode": "hard"},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Stash
// ---------------------------------------------------------------------------

func TestGitStash(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	// Stage a new file so there is something to stash.
	writeFileInDir(t, dir, "file.txt", "content\n")
	runGitInDir(t, dir, "add", "file.txt")

	err := git.Stash(context.Background())
	require.NoError(t, err)

	// The stashed file should no longer be in the working tree.
	_, err = os.Stat(filepath.Join(dir, "file.txt"))
	assert.True(t, os.IsNotExist(err))
}

func TestGitStashNotARepo(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := NewDefaultGitTool(t.TempDir())
	err := git.Stash(context.Background())
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Stash — tool wrapper
// ---------------------------------------------------------------------------

func TestGitStashTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "file.txt", "content\n")
	runGitInDir(t, dir, "add", "file.txt")

	tool := NewGitStashTool(git)
	assert.Equal(t, "git_stash", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Equal(t, "changes stashed", res.Output)
}

func TestGitStashToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitStashTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.StashPop
// ---------------------------------------------------------------------------

func TestGitStashPop(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	// Make and stash a change.
	writeFileInDir(t, dir, "file.txt", "content\n")
	runGitInDir(t, dir, "add", "file.txt")
	require.NoError(t, git.Stash(context.Background()))

	// Pop the stash.
	err := git.StashPop(context.Background())
	require.NoError(t, err)

	// The file should be restored.
	data, err := os.ReadFile(filepath.Join(dir, "file.txt"))
	require.NoError(t, err)
	assert.Equal(t, "content\n", string(data))
}

func TestGitStashPopEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	// No stash exists — stash pop should fail.
	err := git.StashPop(context.Background())
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.StashPop — tool wrapper
// ---------------------------------------------------------------------------

func TestGitStashPopTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "file.txt", "content\n")
	runGitInDir(t, dir, "add", "file.txt")
	require.NoError(t, git.Stash(context.Background()))

	tool := NewGitStashPopTool(git)
	assert.Equal(t, "git_stash_pop", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Equal(t, "stash restored", res.Output)
}

func TestGitStashPopToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitStashPopTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Merge
// ---------------------------------------------------------------------------

func TestGitMerge(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	br := currentBranch(t, dir)

	// Create a feature branch with a new file.
	runGitInDir(t, dir, "checkout", "-b", "feature")
	writeFileInDir(t, dir, "feature.txt", "feature\n")
	runGitInDir(t, dir, "add", "feature.txt")
	runGitInDir(t, dir, "commit", "-m", "feature file")

	// Switch back and merge.
	runGitInDir(t, dir, "checkout", br)
	result, err := git.Merge(context.Background(), "feature")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.Success)

	// The merged file should exist.
	_, err = os.Stat(filepath.Join(dir, "feature.txt"))
	assert.NoError(t, err)
}

func TestGitMergeConflicts(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	br := currentBranch(t, dir)

	// Create a feature branch with a conflicting change.
	runGitInDir(t, dir, "checkout", "-b", "feature")
	writeFileInDir(t, dir, "conflict.txt", "feature change\n")
	runGitInDir(t, dir, "add", "conflict.txt")
	runGitInDir(t, dir, "commit", "-m", "feature change")

	// Switch back and make a conflicting change.
	runGitInDir(t, dir, "checkout", br)
	writeFileInDir(t, dir, "conflict.txt", "base change\n")
	runGitInDir(t, dir, "add", "conflict.txt")
	runGitInDir(t, dir, "commit", "-m", "base change")

	// Merge should report conflicts.
	result, err := git.Merge(context.Background(), "feature")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.Success)
	assert.Contains(t, result.Conflicts, "conflict.txt")
}

func TestGitMergeEmptyBranch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	_, err := git.Merge(context.Background(), "")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Merge — tool wrapper
// ---------------------------------------------------------------------------

func TestGitMergeTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	br := currentBranch(t, dir)

	runGitInDir(t, dir, "checkout", "-b", "feature")
	writeFileInDir(t, dir, "feature.txt", "feature\n")
	runGitInDir(t, dir, "add", "feature.txt")
	runGitInDir(t, dir, "commit", "-m", "feature file")
	runGitInDir(t, dir, "checkout", br)

	tool := NewGitMergeTool(git)
	assert.Equal(t, "git_merge", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"branch": "feature"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "merged: feature")
}

func TestGitMergeToolMissingBranch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitMergeTool(git)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

func TestGitMergeToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitMergeTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"branch": "feature"},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.CreateBranch
// ---------------------------------------------------------------------------

func TestGitCreateBranch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	err := git.CreateBranch(context.Background(), "feature", "")
	require.NoError(t, err)

	branches, err := git.Branch(context.Background())
	require.NoError(t, err)

	found := false
	for _, b := range branches {
		if b.Name == "feature" {
			found = true
		}
	}
	assert.True(t, found, "feature branch should exist")
}

func TestGitCreateBranchFromBase(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	// Create a commit to use as base.
	writeFileInDir(t, dir, "base.txt", "base\n")
	runGitInDir(t, dir, "add", "base.txt")
	runGitInDir(t, dir, "commit", "-m", "base commit")
	hash := strings.TrimSpace(runGitInDir(t, dir, "rev-parse", "HEAD"))

	err := git.CreateBranch(context.Background(), "feature", hash)
	require.NoError(t, err)

	// The branch should point to the specified commit.
	branchHash := strings.TrimSpace(runGitInDir(t, dir, "rev-parse", "feature"))
	assert.Equal(t, hash, branchHash)
}

func TestGitCreateBranchEmptyName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	err := git.CreateBranch(context.Background(), "", "")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.CreateBranch — tool wrapper
// ---------------------------------------------------------------------------

func TestGitCreateBranchTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	tool := NewGitCreateBranchTool(git)
	assert.Equal(t, "git_create_branch", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"name": "feature"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "created branch: feature")

	// Verify the branch was created.
	branches, err := git.Branch(context.Background())
	require.NoError(t, err)
	found := false
	for _, b := range branches {
		if b.Name == "feature" {
			found = true
		}
	}
	assert.True(t, found)
}

func TestGitCreateBranchToolWithBase(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "base.txt", "base\n")
	runGitInDir(t, dir, "add", "base.txt")
	runGitInDir(t, dir, "commit", "-m", "base commit")
	hash := strings.TrimSpace(runGitInDir(t, dir, "rev-parse", "HEAD"))

	tool := NewGitCreateBranchTool(git)
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"name": "feature", "base": hash},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "created branch: feature")
}

func TestGitCreateBranchToolMissingName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitCreateBranchTool(git)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

func TestGitCreateBranchToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitCreateBranchTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"name": "feature"},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitPRCreate — tool wrapper (error paths only; GitHub API not available)
// ---------------------------------------------------------------------------

func TestGitPRCreateToolMetadata(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitPRCreateTool(t.TempDir())
	assert.Equal(t, "git_pr_create", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())
}

func TestGitPRCreateToolMissingTitle(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitPRCreateTool(t.TempDir())

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"base": "main",
			"head": "feature",
		},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "title")
}

func TestGitPRCreateToolMissingBase(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitPRCreateTool(t.TempDir())

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"title": "Test PR",
			"head":  "feature",
		},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "base")
}

func TestGitPRCreateToolMissingHead(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitPRCreateTool(t.TempDir())

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"title": "Test PR",
			"base":  "main",
		},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "head")
}
