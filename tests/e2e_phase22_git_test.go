//go:build e2e

// Package tests contains end-to-end integration tests for the go-cli project.
// This file verifies Phase 22 Git deep integration: branch/merge, stash/pop,
// GitConfig from YAML, GitDiffGenerator, dangerous op approval, and session
// fork with git branch linkage.
package tests

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Phase 22 Git E2E Tests (Task 22-6)
// =============================================================================

// setupPhase22GitRepo creates a real git repo in a temp dir with user config
// and an initial commit, returning the dir path.
func setupPhase22GitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	phase22RunGit(t, dir, "init")
	phase22RunGit(t, dir, "config", "user.email", "e2e@test.com")
	phase22RunGit(t, dir, "config", "user.name", "E2E Test")
	phase22RunGit(t, dir, "config", "commit.gpgsign", "false")
	// Create an initial commit so there is a HEAD to branch from.
	phase22WriteFile(t, dir, "README.md", "# test\n")
	phase22RunGit(t, dir, "add", "README.md")
	phase22RunGit(t, dir, "commit", "-m", "initial commit")
	phase22RunGit(t, dir, "branch", "-m", "main") // ensure default branch is "main"
	return dir
}

func phase22RunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s in %s failed: %s", strings.Join(args, " "), dir, string(out))
}

func phase22WriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0644))
}

// TestET_git_create_branch_and_merge verifies that creating a branch, making
// a commit on it, and merging back into the main branch works end-to-end
// using real git operations.
func TestET_git_create_branch_and_merge(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := setupPhase22GitRepo(t)
	git := tools.NewDefaultGitTool(dir)

	// Create and switch to a feature branch.
	require.NoError(t, git.CreateBranch(ctx, "feature", ""))
	require.NoError(t, git.Checkout(ctx, "feature"))

	// Make a commit on the feature branch.
	phase22WriteFile(t, dir, "feature.txt", "feature content\n")
	phase22RunGit(t, dir, "add", "feature.txt")
	phase22RunGit(t, dir, "commit", "-m", "feature commit")

	// Switch back to main and merge.
	require.NoError(t, git.Checkout(ctx, "main"))
	mergeResult, err := git.Merge(ctx, "feature")
	require.NoError(t, err)
	assert.True(t, mergeResult.Success)

	// Verify the merged file exists.
	_, err = os.Stat(filepath.Join(dir, "feature.txt"))
	require.NoError(t, err, "merged file should exist after merge")
}

// TestET_git_stash_and_pop verifies that stashing changes and popping them
// restores the working tree to the stashed state.
func TestET_git_stash_and_pop(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := setupPhase22GitRepo(t)
	git := tools.NewDefaultGitTool(dir)

	// Make changes to the working tree.
	phase22WriteFile(t, dir, "README.md", "# modified\n")

	// Verify the change exists before stashing.
	content, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	assert.Contains(t, string(content), "modified")

	// Stash the changes.
	require.NoError(t, git.Stash(ctx))

	// After stashing, the file should be back to original.
	content, err = os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	assert.Contains(t, string(content), "test")
	assert.NotContains(t, string(content), "modified")

	// Pop the stash.
	require.NoError(t, git.StashPop(ctx))

	// After popping, the modification should be restored.
	content, err = os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	assert.Contains(t, string(content), "modified")
}

// TestET_git_config_from_yaml verifies that GitConfig can be loaded from a
// YAML configuration file using the config loader.
func TestET_git_config_from_yaml(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = ctx
	yamlContent := `git:
  enabled: true
  workdir: /tmp/e2e-repo
  default_remote: origin
  branch_prefix: e2e/
  auto_commit: false
  platform: github
  api_token: secret-token
`
	// Write YAML to a temp file.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(yamlContent), 0644))

	// Load config using the config loader.
	loader := config.NewLoader().WithFile(cfgPath)
	cfg, err := loader.Load(context.Background())
	require.NoError(t, err)

	// Verify GitConfig fields.
	assert.True(t, *cfg.Git.Enabled)
	assert.Equal(t, "/tmp/e2e-repo", cfg.Git.WorkDir)
	assert.Equal(t, "origin", cfg.Git.DefaultRemote)
	assert.Equal(t, "e2e/", cfg.Git.BranchPrefix)
	assert.False(t, *cfg.Git.AutoCommit)
	assert.Equal(t, "github", cfg.Git.Platform)
	assert.Equal(t, "secret-token", cfg.Git.APIToken)
}

// TestET_git_diff_in_repo verifies that GitDiffGenerator uses `git diff` when
// inside a git repository, producing output that contains git diff markers.
func TestET_git_diff_in_repo(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_ = ctx
	dir := setupPhase22GitRepo(t)
	git := tools.NewDefaultGitTool(dir)

	// Modify a tracked file.
	phase22WriteFile(t, dir, "README.md", "# modified content\n")

	// Create a GitDiffGenerator with the real git tool and a fallback.
	fallback := tools.NewUnifiedDiffGenerator(1000, false)
	gen := tools.NewGitDiffGenerator(git, fallback)

	// Generate diff - should use git diff since we're in a repo.
	diff, err := gen.Generate(context.Background(), "# test\n", "# modified content\n", "README.md")
	require.NoError(t, err)
	assert.NotEmpty(t, diff)
	// Git diff output should contain git diff markers.
	assert.Contains(t, diff, "diff --git")
}

// TestET_git_dangerous_op_requires_approval verifies that git tools for
// dangerous operations (merge, reset, revert) include [REQUIRES APPROVAL] in
// their descriptions.
func TestET_git_dangerous_op_requires_approval(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = ctx
	git := tools.NewDefaultGitTool(t.TempDir())

	dangerousTools := []tools.ToolDefinition{
		tools.NewGitMergeTool(git),
		tools.NewGitResetTool(git),
		tools.NewGitRevertTool(git),
	}

	for _, tool := range dangerousTools {
		desc := tool.Description()
		assert.Contains(t, desc, "REQUIRES APPROVAL",
			"%s description must contain [REQUIRES APPROVAL]", tool.Name())
	}
}

// TestET_session_fork_git_branch verifies that session.Branch with
// WithGitBranch creates a linked git branch in the real git repo, and the
// branch metadata records the git branch name.
func TestET_session_fork_git_branch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := setupPhase22GitRepo(t)
	git := tools.NewDefaultGitTool(dir)

	// Create a session tree and add an initial entry.
	tree := session.NewDefaultSessionTree()
	concreteTree, ok := tree.(*session.DefaultSessionTree)
	require.True(t, ok, "tree should be *DefaultSessionTree")

	rootEntry := &session.SessionEntry{
		ID:        "root",
		Type:      session.EntryTypeUser,
		Content:   "hello",
		Timestamp: time.Now(),
	}
	require.NoError(t, tree.Append(ctx, rootEntry))

	// Wire the git tool as a branch switcher.
	concreteTree.SetGitBranchSwitcher(git)

	// Fork a branch with a linked git branch.
	err := tree.Branch(ctx, "root",
		session.WithBranchID("fork-1"),
		session.WithGitBranch("session-fork-1"),
	)
	require.NoError(t, err)

	// Verify the git branch was created.
	branches, err := git.Branch(ctx)
	require.NoError(t, err)
	found := false
	for _, b := range branches {
		if b.Name == "session-fork-1" {
			found = true
			break
		}
	}
	assert.True(t, found, "git branch 'session-fork-1' should have been created")

	// Verify the branch metadata records the git branch.
	branchMeta, ok := concreteTree.BranchMetaFor("fork-1")
	require.True(t, ok, "branch metadata for fork-1 should exist")
	assert.Equal(t, "session-fork-1", branchMeta.GitBranch)
}
