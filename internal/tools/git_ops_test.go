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

// setupRemoteAndClone creates a bare remote from the repo at dir, pushes the
// current branch, clones it, and returns the clone directory and branch name.
func setupRemoteAndClone(t *testing.T, dir string) (cloneDir, branch string) {
	t.Helper()
	remoteDir := t.TempDir()
	runGitInDir(t, remoteDir, "init", "--bare")
	runGitInDir(t, dir, "remote", "add", "origin", remoteDir)
	branch = currentBranch(t, dir)
	runGitInDir(t, dir, "push", "origin", branch)
	cloneDir = filepath.Join(t.TempDir(), "clone")
	runGitInDir(t, dir, "clone", remoteDir, cloneDir)
	return cloneDir, branch
}

// ---------------------------------------------------------------------------
// GitTool.Status
// ---------------------------------------------------------------------------

func TestGitStatus(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	// Modify a tracked file and add an untracked file.
	writeFileInDir(t, dir, "README.md", "modified\n")
	writeFileInDir(t, dir, "new.txt", "new\n")

	files, err := git.Status(context.Background())
	require.NoError(t, err)

	byFile := map[string]GitFileStatus{}
	for _, f := range files {
		byFile[f.File] = f
	}

	_, ok := byFile["README.md"]
	assert.True(t, ok, "README.md should appear in status")
	assert.Equal(t, "modified", byFile["README.md"].Status)
	assert.False(t, byFile["README.md"].Staged)

	_, ok = byFile["new.txt"]
	assert.True(t, ok, "new.txt should appear in status")
	assert.Equal(t, "untracked", byFile["new.txt"].Status)
}

func TestGitStatusClean(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	files, err := git.Status(context.Background())
	require.NoError(t, err)
	assert.Empty(t, files)
}

func TestGitStatusNotARepo(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := NewDefaultGitTool(t.TempDir())
	_, err := git.Status(context.Background())
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Status — tool wrapper
// ---------------------------------------------------------------------------

func TestGitStatusTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "new.txt", "new\n")

	tool := NewGitStatusTool(git)
	assert.Equal(t, "git_status", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	assert.Contains(t, res.Output, "new.txt")
}

func TestGitStatusToolClean(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitStatusTool(git)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Equal(t, "(clean working tree)", res.Output)
}

func TestGitStatusToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitStatusTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Diff
// ---------------------------------------------------------------------------

func TestGitDiff(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "README.md", "modified\n")

	out, err := git.Diff(context.Background(), GitDiffOptions{})
	require.NoError(t, err)
	assert.Contains(t, out, "-# Test")
	assert.Contains(t, out, "+modified")
}

func TestGitDiffStaged(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "README.md", "modified\n")
	runGitInDir(t, dir, "add", "README.md")

	out, err := git.Diff(context.Background(), GitDiffOptions{Staged: true})
	require.NoError(t, err)
	assert.Contains(t, out, "-# Test")
	assert.Contains(t, out, "+modified")
}

func TestGitDiffPath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "README.md", "modified\n")

	out, err := git.Diff(context.Background(), GitDiffOptions{Path: "README.md"})
	require.NoError(t, err)
	assert.Contains(t, out, "+modified")
}

func TestGitDiffNotARepo(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := NewDefaultGitTool(t.TempDir())
	_, err := git.Diff(context.Background(), GitDiffOptions{})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Diff — tool wrapper
// ---------------------------------------------------------------------------

func TestGitDiffTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "README.md", "modified\n")

	tool := NewGitDiffTool(git)
	assert.Equal(t, "git_diff", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "modified")
}

func TestGitDiffToolStaged(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "README.md", "modified\n")
	runGitInDir(t, dir, "add", "README.md")

	tool := NewGitDiffTool(git)
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"staged": true},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "modified")
}

func TestGitDiffToolNoChanges(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitDiffTool(git)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Equal(t, "(no changes)", res.Output)
}

func TestGitDiffToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitDiffTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Commit
// ---------------------------------------------------------------------------

func TestGitCommit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "file.txt", "content\n")
	runGitInDir(t, dir, "add", "file.txt")

	hash, err := git.Commit(context.Background(), GitCommitOptions{Message: "add file"})
	require.NoError(t, err)
	assert.NotEmpty(t, hash)

	logOut := runGitInDir(t, dir, "log", "--oneline", "-1")
	assert.Contains(t, logOut, "add file")
}

func TestGitCommitAddAll(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "file.txt", "content\n")

	hash, err := git.Commit(context.Background(), GitCommitOptions{Message: "add file", AddAll: true})
	require.NoError(t, err)
	assert.NotEmpty(t, hash)

	logOut := runGitInDir(t, dir, "log", "--oneline", "-1")
	assert.Contains(t, logOut, "add file")
}

func TestGitCommitEmptyMessage(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	_, err := git.Commit(context.Background(), GitCommitOptions{Message: ""})
	assert.Error(t, err)
}

func TestGitCommitNothingToCommit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	_, err := git.Commit(context.Background(), GitCommitOptions{Message: "nothing"})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Commit — tool wrapper
// ---------------------------------------------------------------------------

func TestGitCommitTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "file.txt", "content\n")
	runGitInDir(t, dir, "add", "file.txt")

	tool := NewGitCommitTool(git)
	assert.Equal(t, "git_commit", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"message": "add file"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "committed:")
}

func TestGitCommitToolAddAll(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "file.txt", "content\n")

	tool := NewGitCommitTool(git)
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"message": "add file", "add_all": true},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "committed:")
}

func TestGitCommitToolMissingMessage(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitCommitTool(git)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

func TestGitCommitToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitCommitTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"message": "test"},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Remote
// ---------------------------------------------------------------------------

func TestGitRemote(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	remoteDir := t.TempDir()
	runGitInDir(t, remoteDir, "init", "--bare")
	runGitInDir(t, dir, "remote", "add", "origin", remoteDir)

	remotes, err := git.Remote(context.Background())
	require.NoError(t, err)
	require.Len(t, remotes, 2) // fetch + push

	assert.Equal(t, "origin", remotes[0].Name)
	assert.NotEmpty(t, remotes[0].URL)
}

func TestGitRemoteEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	remotes, err := git.Remote(context.Background())
	require.NoError(t, err)
	assert.Empty(t, remotes)
}

func TestGitRemoteNotARepo(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := NewDefaultGitTool(t.TempDir())
	_, err := git.Remote(context.Background())
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Remote — tool wrapper
// ---------------------------------------------------------------------------

func TestGitRemoteTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	remoteDir := t.TempDir()
	runGitInDir(t, remoteDir, "init", "--bare")
	runGitInDir(t, dir, "remote", "add", "origin", remoteDir)

	tool := NewGitRemoteTool(git)
	assert.Equal(t, "git_remote", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "origin")
}

func TestGitRemoteToolNoRemotes(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitRemoteTool(git)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Equal(t, "(no remotes)", res.Output)
}

func TestGitRemoteToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitRemoteTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Pull
// ---------------------------------------------------------------------------

func TestGitPull(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	cloneDir, br := setupRemoteAndClone(t, dir)

	// Make a new commit in the original repo and push.
	writeFileInDir(t, dir, "new.txt", "new\n")
	runGitInDir(t, dir, "add", "new.txt")
	runGitInDir(t, dir, "commit", "-m", "add new.txt")
	runGitInDir(t, dir, "push", "origin", br)

	// Pull in the clone.
	cloneTool := NewDefaultGitTool(cloneDir)
	err := cloneTool.Pull(context.Background(), "origin", br)
	require.NoError(t, err)

	// Verify the pulled file exists.
	_, err = os.Stat(filepath.Join(cloneDir, "new.txt"))
	assert.NoError(t, err)
}

func TestGitPullNoRemote(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	err := git.Pull(context.Background(), "origin", "main")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Pull — tool wrapper
// ---------------------------------------------------------------------------

func TestGitPullTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	cloneDir, br := setupRemoteAndClone(t, dir)

	// Make a new commit and push.
	writeFileInDir(t, dir, "new.txt", "new\n")
	runGitInDir(t, dir, "add", "new.txt")
	runGitInDir(t, dir, "commit", "-m", "add new.txt")
	runGitInDir(t, dir, "push", "origin", br)

	cloneTool := NewDefaultGitTool(cloneDir)
	tool := NewGitPullTool(cloneTool)
	assert.Equal(t, "git_pull", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"remote": "origin",
			"branch": br,
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "pulled")
}

func TestGitPullToolNoRemote(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitPullTool(git)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"remote": "origin",
			"branch": "main",
		},
	})
	assert.Error(t, err)
}

func TestGitPullToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitPullTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Fetch
// ---------------------------------------------------------------------------

func TestGitFetch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	cloneDir, br := setupRemoteAndClone(t, dir)

	// Make a new commit and push.
	writeFileInDir(t, dir, "new.txt", "new\n")
	runGitInDir(t, dir, "add", "new.txt")
	runGitInDir(t, dir, "commit", "-m", "add new.txt")
	runGitInDir(t, dir, "push", "origin", br)

	// Fetch in the clone.
	cloneTool := NewDefaultGitTool(cloneDir)
	err := cloneTool.Fetch(context.Background(), "origin")
	require.NoError(t, err)

	// Verify the remote-tracking branch has the new commit.
	remoteRef := runGitInDir(t, cloneDir, "log", "origin/"+br, "--format=%s", "-1")
	assert.Contains(t, strings.TrimSpace(remoteRef), "add new.txt")
}

func TestGitFetchNoRemote(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	err := git.Fetch(context.Background(), "origin")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Fetch — tool wrapper
// ---------------------------------------------------------------------------

func TestGitFetchTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	cloneDir, _ := setupRemoteAndClone(t, dir)

	// Make a new commit and push.
	writeFileInDir(t, dir, "new.txt", "new\n")
	runGitInDir(t, dir, "add", "new.txt")
	runGitInDir(t, dir, "commit", "-m", "add new.txt")
	br := currentBranch(t, dir)
	runGitInDir(t, dir, "push", "origin", br)

	cloneTool := NewDefaultGitTool(cloneDir)
	tool := NewGitFetchTool(cloneTool)
	assert.Equal(t, "git_fetch", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"remote": "origin"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "fetched")
}

func TestGitFetchToolNoRemote(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitFetchTool(git)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"remote": "origin"},
	})
	assert.Error(t, err)
}

func TestGitFetchToolNilGit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewGitFetchTool(nil)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}
