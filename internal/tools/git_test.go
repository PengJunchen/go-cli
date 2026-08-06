package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// runGitInDir runs a git command in dir and fails the test on error.
func runGitInDir(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s in %s: %s", strings.Join(args, " "), dir, out)
	return string(out)
}

// writeFileInDir writes content to name within dir.
func writeFileInDir(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// setupGitRepo creates a temp git repo with an initial commit and returns the
// DefaultGitTool pointing at it. The repo is configured with a test author so
// commits are deterministic.
func setupGitRepo(t *testing.T) *DefaultGitTool {
	t.Helper()
	dir := t.TempDir()
	runGitInDir(t, dir, "init")
	runGitInDir(t, dir, "config", "user.email", "test@example.com")
	runGitInDir(t, dir, "config", "user.name", "Test User")
	writeFileInDir(t, dir, "README.md", "# Test\n")
	runGitInDir(t, dir, "add", "README.md")
	runGitInDir(t, dir, "commit", "-m", "initial commit")
	return NewDefaultGitTool(dir)
}

// currentBranch returns the current branch name of the repo at dir.
func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	out := runGitInDir(t, dir, "branch", "--show-current")
	return strings.TrimSpace(out)
}

// ---------------------------------------------------------------------------
// GitTool.Log
// ---------------------------------------------------------------------------

func TestGitLog(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "file.txt", "content\n")
	runGitInDir(t, dir, "add", "file.txt")
	runGitInDir(t, dir, "commit", "-m", "add file.txt")

	entries, err := git.Log(context.Background(), GitLogOptions{})
	require.NoError(t, err)
	require.Len(t, entries, 2)

	// Most recent commit first.
	assert.Equal(t, "add file.txt", entries[0].Message)
	assert.Equal(t, "initial commit", entries[1].Message)

	assert.Equal(t, "Test User", entries[0].Author)
	assert.Equal(t, "test@example.com", entries[0].Email)
	assert.NotEmpty(t, entries[0].Hash)
	assert.NotEmpty(t, entries[0].Date)
}

func TestGitLogMaxCount(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	for i := 0; i < 3; i++ {
		writeFileInDir(t, dir, "f"+string(rune('a'+i))+".txt", "x\n")
		runGitInDir(t, dir, "add", ".")
		runGitInDir(t, dir, "commit", "-m", "commit "+string(rune('0'+i+1)))
	}

	entries, err := git.Log(context.Background(), GitLogOptions{MaxCount: 2})
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestGitLogPath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "a.txt", "a\n")
	runGitInDir(t, dir, "add", "a.txt")
	runGitInDir(t, dir, "commit", "-m", "add a")

	writeFileInDir(t, dir, "b.txt", "b\n")
	runGitInDir(t, dir, "add", "b.txt")
	runGitInDir(t, dir, "commit", "-m", "add b")

	entries, err := git.Log(context.Background(), GitLogOptions{Path: "a.txt"})
	require.NoError(t, err)
	// Only the "add a" commit touched a.txt (initial commit only added README.md).
	require.Len(t, entries, 1)
	assert.Equal(t, "add a", entries[0].Message)
}

func TestGitLogAuthor(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "f.txt", "x\n")
	runGitInDir(t, dir, "add", "f.txt")
	runGitInDir(t, dir, "commit", "-m", "by test")

	entries, err := git.Log(context.Background(), GitLogOptions{Author: "Test User"})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(entries), 1)

	// Non-matching author returns empty.
	entries, err = git.Log(context.Background(), GitLogOptions{Author: "nobody"})
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// ---------------------------------------------------------------------------
// GitTool.Branch
// ---------------------------------------------------------------------------

func TestGitBranch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	runGitInDir(t, dir, "branch", "feature")
	runGitInDir(t, dir, "branch", "develop")

	branches, err := git.Branch(context.Background())
	require.NoError(t, err)

	names := map[string]GitBranch{}
	for _, b := range branches {
		names[b.Name] = b
	}

	assert.True(t, names["feature"].Name != "", "feature branch should be listed")
	assert.True(t, names["develop"].Name != "", "develop branch should be listed")

	// Exactly one branch is current.
	currentCount := 0
	for _, b := range branches {
		if b.Current {
			currentCount++
			assert.False(t, b.Remote, "current local branch should not be remote")
		}
	}
	assert.Equal(t, 1, currentCount)
}

func TestGitBranchRemote(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	// Set up a bare remote and push.
	remoteDir := t.TempDir()
	runGitInDir(t, remoteDir, "init", "--bare")
	runGitInDir(t, dir, "remote", "add", "origin", remoteDir)
	br := currentBranch(t, dir)
	runGitInDir(t, dir, "push", "origin", br)

	branches, err := git.Branch(context.Background())
	require.NoError(t, err)

	hasRemote := false
	for _, b := range branches {
		if b.Remote {
			hasRemote = true
			assert.Contains(t, b.Name, "origin/")
		}
	}
	assert.True(t, hasRemote, "should list at least one remote branch")
}

// ---------------------------------------------------------------------------
// GitTool.Checkout
// ---------------------------------------------------------------------------

func TestGitCheckout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	runGitInDir(t, dir, "branch", "feature")

	require.NoError(t, git.Checkout(context.Background(), "feature"))
	assert.Equal(t, "feature", currentBranch(t, dir))
}

func TestGitCheckoutNonExistent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)

	err := git.Checkout(context.Background(), "nonexistent-branch")
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// GitTool.Blame
// ---------------------------------------------------------------------------

func TestGitBlame(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	content := "line one\nline two\nline three\n"
	writeFileInDir(t, dir, "file.txt", content)
	runGitInDir(t, dir, "add", "file.txt")
	runGitInDir(t, dir, "commit", "-m", "add file.txt")

	lines, err := git.Blame(context.Background(), "file.txt", 1, 3)
	require.NoError(t, err)
	require.Len(t, lines, 3)

	assert.Equal(t, 1, lines[0].LineNum)
	assert.Equal(t, "line one", lines[0].Content)
	assert.Equal(t, "Test User", lines[0].Author)
	assert.Equal(t, "test@example.com", lines[0].Email)
	assert.NotEmpty(t, lines[0].Hash)
	assert.NotEmpty(t, lines[0].Date)

	assert.Equal(t, 2, lines[1].LineNum)
	assert.Equal(t, "line two", lines[1].Content)

	assert.Equal(t, 3, lines[2].LineNum)
	assert.Equal(t, "line three", lines[2].Content)
}

func TestGitBlameRange(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	content := "a\nb\nc\nd\ne\n"
	writeFileInDir(t, dir, "file.txt", content)
	runGitInDir(t, dir, "add", "file.txt")
	runGitInDir(t, dir, "commit", "-m", "add file.txt")

	lines, err := git.Blame(context.Background(), "file.txt", 2, 4)
	require.NoError(t, err)
	require.Len(t, lines, 3)

	assert.Equal(t, 2, lines[0].LineNum)
	assert.Equal(t, "b", lines[0].Content)
	assert.Equal(t, 3, lines[1].LineNum)
	assert.Equal(t, "c", lines[1].Content)
	assert.Equal(t, 4, lines[2].LineNum)
	assert.Equal(t, "d", lines[2].Content)
}

// ---------------------------------------------------------------------------
// GitTool.Push
// ---------------------------------------------------------------------------

func TestGitPush(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	remoteDir := t.TempDir()
	runGitInDir(t, remoteDir, "init", "--bare")
	runGitInDir(t, dir, "remote", "add", "origin", remoteDir)

	br := currentBranch(t, dir)
	err := git.Push(context.Background(), "origin", br, false)
	require.NoError(t, err)

	// Verify the remote received the branch.
	remoteBranches := runGitInDir(t, remoteDir, "branch")
	assert.Contains(t, remoteBranches, br)
}

func TestGitPushForce(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	remoteDir := t.TempDir()
	runGitInDir(t, remoteDir, "init", "--bare")
	runGitInDir(t, dir, "remote", "add", "origin", remoteDir)

	br := currentBranch(t, dir)
	require.NoError(t, git.Push(context.Background(), "origin", br, false))

	// Diverge history via amend.
	writeFileInDir(t, dir, "new.txt", "new\n")
	runGitInDir(t, dir, "add", "new.txt")
	runGitInDir(t, dir, "commit", "--amend", "-m", "amended commit")

	// Non-force push should fail (diverged).
	err := git.Push(context.Background(), "origin", br, false)
	assert.Error(t, err)

	// Force push should succeed.
	err = git.Push(context.Background(), "origin", br, true)
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// ToolDefinition wrappers
// ---------------------------------------------------------------------------

func TestGitLogTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitLogTool(git)
	assert.Equal(t, "git_log", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
}

func TestGitLogToolWithMaxCount(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	writeFileInDir(t, dir, "a.txt", "a\n")
	runGitInDir(t, dir, "add", "a.txt")
	runGitInDir(t, dir, "commit", "-m", "second")

	tool := NewGitLogTool(git)
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"max_count": float64(1)},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
}

func TestGitBranchTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitBranchTool(git)
	assert.Equal(t, "git_branch", tool.Name())
	assert.NotEmpty(t, tool.Description())
	assert.NotNil(t, tool.Parameters())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
}

func TestGitCheckoutTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd
	runGitInDir(t, dir, "branch", "feature")

	tool := NewGitCheckoutTool(git)
	assert.Equal(t, "git_checkout", tool.Name())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"branch": "feature"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	assert.Equal(t, "feature", currentBranch(t, dir))
}

func TestGitCheckoutToolMissingBranch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitCheckoutTool(git)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

func TestGitBlameTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	content := "line one\nline two\n"
	writeFileInDir(t, dir, "file.txt", content)
	runGitInDir(t, dir, "add", "file.txt")
	runGitInDir(t, dir, "commit", "-m", "add file.txt")

	tool := NewGitBlameTool(git)
	assert.Equal(t, "git_blame", tool.Name())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"file":       "file.txt",
			"start_line": float64(1),
			"end_line":   float64(2),
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
}

func TestGitBlameToolMissingFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	tool := NewGitBlameTool(git)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	assert.Error(t, err)
}

func TestGitPushTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	remoteDir := t.TempDir()
	runGitInDir(t, remoteDir, "init", "--bare")
	runGitInDir(t, dir, "remote", "add", "origin", remoteDir)

	br := currentBranch(t, dir)
	tool := NewGitPushTool(git)
	assert.Equal(t, "git_push", tool.Name())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"remote": "origin",
			"branch": br,
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
}

func TestGitPushToolForce(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	git := setupGitRepo(t)
	dir := git.cwd

	remoteDir := t.TempDir()
	runGitInDir(t, remoteDir, "init", "--bare")
	runGitInDir(t, dir, "remote", "add", "origin", remoteDir)

	br := currentBranch(t, dir)

	tool := NewGitPushTool(git)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"remote": "origin",
			"branch": br,
		},
	})
	require.NoError(t, err)

	// Diverge and force push.
	writeFileInDir(t, dir, "new.txt", "new\n")
	runGitInDir(t, dir, "add", "new.txt")
	runGitInDir(t, dir, "commit", "--amend", "-m", "amended")

	var res *ToolResult
	res, err = tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"remote": "origin",
			"branch": br,
			"force":  true,
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
}
