//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 20 Git Tools: GitStatusTool, GitDiffTool, and
// GitCommitTool using real git repos created with `git init`.
package e2e_20260802

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// =============================================================================
// Shared helpers
// =============================================================================

// setupPhase20GitRepo creates a real git repo in a temp dir with user config
// and GPG signing disabled so that commits succeed without external setup.
func setupPhase20GitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	phase20RunGit(t, dir, "init")
	phase20RunGit(t, dir, "config", "user.email", "e2e@test.com")
	phase20RunGit(t, dir, "config", "user.name", "E2E Test")
	phase20RunGit(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

// phase20RunGit runs a git command in dir and fails the test on error.
func phase20RunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s in %s failed: %s", strings.Join(args, " "), dir, string(out))
}

// phase20WriteFile writes a file inside dir.
func phase20WriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0644))
}

// =============================================================================
// AC-1: GitStatusTool returns untracked status for a new file
// =============================================================================

// TestET_Phase20_GitTools_AC1_UntrackedStatus verifies that in a real git repo
// with an untracked file, GitStatusTool.Execute returns the untracked status.
func TestET_Phase20_GitTools_AC1_UntrackedStatus(t *testing.T) {
	dir := setupPhase20GitRepo(t)
	phase20WriteFile(t, dir, "testfile.txt", "hello world\n")

	git := tools.NewDefaultGitTool(dir)
	statusTool := tools.NewGitStatusTool(git)

	res, err := statusTool.Execute(context.Background(), tools.ToolCall{
		ID: "tc-1", Name: "git_status", Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "untracked")
	assert.Contains(t, res.Output, "testfile.txt")
}

// =============================================================================
// AC-2: GitDiffTool returns staged diff text after git add
// =============================================================================

// TestET_Phase20_GitTools_AC2_StagedDiff verifies that after staging a file,
// GitDiffTool.Execute with staged=true returns the unified diff text.
func TestET_Phase20_GitTools_AC2_StagedDiff(t *testing.T) {
	dir := setupPhase20GitRepo(t)
	phase20WriteFile(t, dir, "testfile.txt", "hello world\n")
	phase20RunGit(t, dir, "add", "testfile.txt")

	git := tools.NewDefaultGitTool(dir)
	diffTool := tools.NewGitDiffTool(git)

	res, err := diffTool.Execute(context.Background(), tools.ToolCall{
		ID: "tc-1", Name: "git_diff", Args: map[string]any{"staged": true},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "diff --git")
	assert.Contains(t, res.Output, "testfile.txt")
	assert.Contains(t, res.Output, "+hello world")
}

// =============================================================================
// AC-3: GitCommitTool commits and returns a commit hash
// =============================================================================

// TestET_Phase20_GitTools_AC3_Commit verifies that GitCommitTool.Execute with a
// commit message succeeds and returns a commit hash in the output.
func TestET_Phase20_GitTools_AC3_Commit(t *testing.T) {
	dir := setupPhase20GitRepo(t)
	phase20WriteFile(t, dir, "testfile.txt", "hello world\n")
	phase20RunGit(t, dir, "add", "testfile.txt")

	git := tools.NewDefaultGitTool(dir)
	commitTool := tools.NewGitCommitTool(git)

	res, err := commitTool.Execute(context.Background(), tools.ToolCall{
		ID: "tc-1", Name: "git_commit", Args: map[string]any{"message": "test commit"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "committed:")

	hash := strings.TrimSpace(strings.TrimPrefix(res.Output, "committed: "))
	assert.NotEmpty(t, hash, "commit hash must not be empty")
	assert.Len(t, hash, 40, "commit hash should be a 40-char SHA-1")

	// Also verify via metadata.
	if md, ok := res.Metadata["hash"]; ok {
		assert.Equal(t, hash, md)
	}
}

// =============================================================================
// AC-4: GitStatusTool returns clean working tree after commit
// =============================================================================

// TestET_Phase20_GitTools_AC4_CleanAfterCommit verifies that after committing
// all changes, GitStatusTool.Execute reports a clean working tree.
func TestET_Phase20_GitTools_AC4_CleanAfterCommit(t *testing.T) {
	dir := setupPhase20GitRepo(t)
	phase20WriteFile(t, dir, "testfile.txt", "hello world\n")
	phase20RunGit(t, dir, "add", "testfile.txt")

	git := tools.NewDefaultGitTool(dir)
	commitTool := tools.NewGitCommitTool(git)

	_, err := commitTool.Execute(context.Background(), tools.ToolCall{
		ID: "tc-1", Name: "git_commit", Args: map[string]any{"message": "test commit"},
	})
	require.NoError(t, err)

	statusTool := tools.NewGitStatusTool(git)
	res, err := statusTool.Execute(context.Background(), tools.ToolCall{
		ID: "tc-2", Name: "git_status", Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "clean")
}

// =============================================================================
// AC-5: GitStatusTool returns error (not panic) in a non-git directory
// =============================================================================

// TestET_Phase20_GitTools_AC5_NonGitDirError verifies that calling
// GitStatusTool.Execute in a directory that is not a git repo returns an error
// containing a helpful message instead of panicking.
func TestET_Phase20_GitTools_AC5_NonGitDirError(t *testing.T) {
	dir := t.TempDir() // no git init

	git := tools.NewDefaultGitTool(dir)
	statusTool := tools.NewGitStatusTool(git)

	_, err := statusTool.Execute(context.Background(), tools.ToolCall{
		ID: "tc-1", Name: "git_status", Args: map[string]any{},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "work tree")
}

// =============================================================================
// AC-6: All 3 Git tools are registered in AssembleAgent result
// =============================================================================

// TestET_Phase20_GitTools_AC6_RegisteredInAssembleAgent verifies that
// AssembleAgent registers git_status, git_diff, and git_commit in the
// ToolRegistry.
func TestET_Phase20_GitTools_AC6_RegisteredInAssembleAgent(t *testing.T) {
	assembly := phase19wAssemble(t, phase19wTestConfig())
	ctx := context.Background()

	for _, name := range []string{"git_status", "git_diff", "git_commit"} {
		def, err := assembly.ToolRegistry.Get(ctx, name)
		require.NoError(t, err, "tool %q must be registered in AssembleAgent", name)
		assert.Equal(t, name, def.Name())
	}
}

// =============================================================================
// AC-7: Each Git tool implements the Parameterized interface
// =============================================================================

// TestET_Phase20_GitTools_AC7_ImplementsParameterized verifies that
// GitStatusTool, GitDiffTool, and GitCommitTool all implement the Parameterized
// interface (i.e., have a Parameters() method returning a non-nil schema).
func TestET_Phase20_GitTools_AC7_ImplementsParameterized(t *testing.T) {
	git := tools.NewDefaultGitTool(t.TempDir())

	toolList := []tools.ToolDefinition{
		tools.NewGitStatusTool(git),
		tools.NewGitDiffTool(git),
		tools.NewGitCommitTool(git),
	}

	for _, tool := range toolList {
		p, ok := tool.(tools.Parameterized)
		assert.True(t, ok, "%s must implement Parameterized", tool.Name())
		assert.NotNil(t, p.Parameters(), "%s Parameters() must not be nil", tool.Name())
	}
}
