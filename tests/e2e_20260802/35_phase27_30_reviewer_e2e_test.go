//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file covers the reviewer E2E tests for Phases 27-30: git deep
// integration, LSP integration with mock server, remote execution with mock
// SSH client, and custom tool registration.
package e2e_20260802 //nolint:staticcheck

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Shared helpers
// =============================================================================

// phase27SetupGitRepo creates a temp git repo with user config, GPG signing
// disabled, and an initial commit, returning the repo directory.
func phase27SetupGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	phase27RunGit(t, dir, "init")
	phase27RunGit(t, dir, "config", "user.email", "e2e@test.com")
	phase27RunGit(t, dir, "config", "user.name", "E2E Test")
	phase27RunGit(t, dir, "config", "commit.gpgsign", "false")
	phase27WriteFile(t, dir, "README.md", "# Test\n")
	phase27RunGit(t, dir, "add", "README.md")
	phase27RunGit(t, dir, "commit", "-m", "initial commit")
	return dir
}

// phase27RunGit runs a git command in dir and fails the test on error.
func phase27RunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s in %s failed: %s", strings.Join(args, " "), dir, string(out))
}

// phase27WriteFile writes content to name within dir.
func phase27WriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0644))
}

// phase27CurrentBranch returns the current branch name of the repo at dir.
func phase27CurrentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "branch", "--show-current")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "get current branch: %s", string(out))
	return strings.TrimSpace(string(out))
}

// =============================================================================
// Phase 27: Git Deep Integration E2E Tests
// commit_spec: test(tools): add git deep integration E2E tests
// =============================================================================

// Test ID:   ET-PHASE27-GITLOG
// Task ref:  tasks.json#27 git deep integration E2E tests
// Feature:   GitTool.Log returns structured GitLogEntry values
func TestET_Phase27_GitLogStructured(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := phase27SetupGitRepo(t)
	phase27WriteFile(t, dir, "file.txt", "content\n")
	phase27RunGit(t, dir, "add", "file.txt")
	phase27RunGit(t, dir, "commit", "-m", "add file.txt")

	git := tools.NewDefaultGitTool(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	entries, err := git.Log(ctx, tools.GitLogOptions{})
	require.NoError(t, err)
	require.Len(t, entries, 2)

	// Most recent commit first.
	assert.Equal(t, "add file.txt", entries[0].Message)
	assert.Equal(t, "initial commit", entries[1].Message)

	// Verify structured fields.
	assert.NotEmpty(t, entries[0].Hash)
	assert.Equal(t, "E2E Test", entries[0].Author)
	assert.Equal(t, "e2e@test.com", entries[0].Email)
	assert.NotEmpty(t, entries[0].Date)

	// MaxCount option limits results.
	entries, err = git.Log(ctx, tools.GitLogOptions{MaxCount: 1})
	require.NoError(t, err)
	assert.Len(t, entries, 1)
	assert.Equal(t, "add file.txt", entries[0].Message)
}

// Test ID:   ET-PHASE27-BRANCH
// Task ref:  tasks.json#27 git deep integration E2E tests
// Feature:   GitTool.Branch lists branches and GitTool.Checkout switches branch
func TestET_Phase27_GitBranchAndCheckout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := phase27SetupGitRepo(t)
	phase27RunGit(t, dir, "branch", "feature")
	phase27RunGit(t, dir, "branch", "develop")

	git := tools.NewDefaultGitTool(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	branches, err := git.Branch(ctx)
	require.NoError(t, err)

	names := map[string]tools.GitBranch{}
	for _, b := range branches {
		names[b.Name] = b
	}
	require.Contains(t, names, "feature")
	require.Contains(t, names, "develop")

	// Exactly one current branch.
	currentCount := 0
	for _, b := range branches {
		if b.Current {
			currentCount++
		}
	}
	assert.Equal(t, 1, currentCount)

	// Checkout feature and verify switch.
	require.NoError(t, git.Checkout(ctx, "feature"))
	assert.Equal(t, "feature", phase27CurrentBranch(t, dir))

	// Checkout develop and verify switch.
	require.NoError(t, git.Checkout(ctx, "develop"))
	assert.Equal(t, "develop", phase27CurrentBranch(t, dir))
}

// Test ID:   ET-PHASE27-BLAME
// Task ref:  tasks.json#27 git deep integration E2E tests
// Feature:   GitTool.Blame returns line-by-line author attribution
func TestET_Phase27_GitBlame(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := phase27SetupGitRepo(t)
	content := "line one\nline two\nline three\n"
	phase27WriteFile(t, dir, "file.txt", content)
	phase27RunGit(t, dir, "add", "file.txt")
	phase27RunGit(t, dir, "commit", "-m", "add file.txt")

	git := tools.NewDefaultGitTool(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	lines, err := git.Blame(ctx, "file.txt", 1, 3)
	require.NoError(t, err)
	require.Len(t, lines, 3)

	assert.Equal(t, 1, lines[0].LineNum)
	assert.Equal(t, "line one", lines[0].Content)
	assert.Equal(t, "E2E Test", lines[0].Author)
	assert.Equal(t, "e2e@test.com", lines[0].Email)
	assert.NotEmpty(t, lines[0].Hash)
	assert.NotEmpty(t, lines[0].Date)

	assert.Equal(t, 2, lines[1].LineNum)
	assert.Equal(t, "line two", lines[1].Content)

	assert.Equal(t, 3, lines[2].LineNum)
	assert.Equal(t, "line three", lines[2].Content)
}

// Test ID:   ET-PHASE27-PUSHAPPROVAL
// Task ref:  tasks.json#27 git deep integration E2E tests
// Feature:   GitPushTool description marks destructive operation as requiring approval
func TestET_Phase27_GitPushApproval(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := phase27SetupGitRepo(t)
	git := tools.NewDefaultGitTool(dir)
	tool := tools.NewGitPushTool(git)

	desc := tool.Description()
	assert.Contains(t, desc, "REQUIRES APPROVAL")
	assert.Contains(t, desc, "git_push")
}

// =============================================================================
// Phase 28: LSP Integration E2E Tests
// commit_spec: test(tools): add LSP integration tests with mock server
// =============================================================================

// phase28WriteFrame writes a Content-Length framed JSON-RPC message.
func phase28WriteFrame(w io.Writer, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := w.Write([]byte(header)); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

// phase28ReadFrame reads a Content-Length framed message and returns the
// parsed JSON object.
func phase28ReadFrame(r *bufio.Reader) (map[string]any, error) {
	var contentLength int
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:"))
			contentLength, err = strconv.Atoi(val)
			if err != nil {
				return nil, err
			}
		}
	}
	if contentLength <= 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// phase28EchoServer is a minimal JSON-RPC server that echoes back the method
// name as the result for each request.
func phase28EchoServer(conn net.Conn) {
	reader := bufio.NewReader(conn)
	for {
		msg, err := phase28ReadFrame(reader)
		if err != nil {
			return
		}
		if err := phase28WriteFrame(conn, map[string]any{
			"jsonrpc": "2.0",
			"id":      msg["id"],
			"result":  map[string]any{"method": msg["method"]},
		}); err != nil {
			return
		}
	}
}

// Test ID:   ET-PHASE28-JSONRPC
// Task ref:  tasks.json#28 LSP integration tests with mock server
// Feature:   JSONRPCClient request/response over net.Pipe
func TestET_Phase28_JSONRPCRequestResponse(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		phase28EchoServer(serverConn)
	}()

	rpc := tools.NewJSONRPCClient(clientConn, clientConn)
	defer func() {
		rpc.Close()
		_ = clientConn.Close()
		_ = serverConn.Close()
		<-done
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var result struct {
		Method string `json:"method"`
	}
	err := rpc.Call(ctx, "test/method", map[string]any{"hello": "world"}, &result)
	require.NoError(t, err)
	assert.Equal(t, "test/method", result.Method)
}

// Test ID:   ET-PHASE28-LSPDEF
// Task ref:  tasks.json#28 LSP integration tests with mock server
// Feature:   NewLSPTool returns valid ToolDefinition with correct name and parameters
func TestET_Phase28_LSPToolDefinition(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := tools.NewLSPTool(nil)
	assert.Equal(t, "lsp_query", tool.Name())
	assert.NotEmpty(t, tool.Description())

	params, ok := tool.(tools.Parameterized).Parameters().(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", params["type"])

	props, ok := params["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "operation")
	assert.Contains(t, props, "uri")
	assert.Contains(t, props, "line")
	assert.Contains(t, props, "character")

	required, ok := params["required"].([]string)
	require.True(t, ok)
	assert.Contains(t, required, "operation")
	assert.Contains(t, required, "uri")
}

// Test ID:   ET-PHASE28-LSPERR
// Task ref:  tasks.json#28 LSP integration tests with mock server
// Feature:   lspTool.Execute with nil client returns graceful error
func TestET_Phase28_LSPToolExecuteWithError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := tools.NewLSPTool(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := tool.Execute(ctx, tools.ToolCall{
		ID: "tc-1",
		Args: map[string]any{
			"operation": "definition",
			"uri":       "file:///test.go",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil LSP client")
}

// =============================================================================
// Phase 29: Remote Execution E2E Tests
// commit_spec: test(tools): add remote execution tests with mock SSH server
// =============================================================================

// phase29MockSSHClient is a test double for tools.SSHClient. It records calls
// and returns configurable results.
type phase29MockSSHClient struct {
	execFn    func(ctx context.Context, command string) (string, string, int, error)
	execCalls []string
}

func (m *phase29MockSSHClient) Connect(_ context.Context) error { return nil }

func (m *phase29MockSSHClient) Exec(ctx context.Context, command string) (string, string, int, error) {
	m.execCalls = append(m.execCalls, command)
	if m.execFn != nil {
		return m.execFn(ctx, command)
	}
	return "", "", 0, nil
}

func (m *phase29MockSSHClient) Close() error { return nil }

var _ tools.SSHClient = (*phase29MockSSHClient)(nil)

// Test ID:   ET-PHASE29-SANDBOX
// Task ref:  tasks.json#29 remote execution tests with mock SSH server
// Feature:   RemoteBashTool rejects blocked commands before reaching SSH
func TestET_Phase29_RemoteBashToolSandboxValidation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &phase29MockSSHClient{}
	tool := tools.NewRemoteBashTool(mc, tools.WithRemoteBashSandbox(
		tools.NewDefaultBashSandbox(),
	))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := tool.Execute(ctx, tools.ToolCall{
		Args: map[string]any{"command": "rm -rf /"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklisted")
	// The command should never have been sent to the remote host.
	assert.Empty(t, mc.execCalls)
}

// Test ID:   ET-PHASE29-OUTPUTLIMIT
// Task ref:  tasks.json#29 remote execution tests with mock SSH server
// Feature:   RemoteBashTool limits captured output
func TestET_Phase29_RemoteBashToolOutputLimit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	longOutput := strings.Repeat("A", 500)
	mc := &phase29MockSSHClient{
		execFn: func(_ context.Context, _ string) (string, string, int, error) {
			return longOutput, "", 0, nil
		},
	}
	tool := tools.NewRemoteBashTool(mc, tools.WithRemoteBashMaxOutput(50))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := tool.Execute(ctx, tools.ToolCall{
		Args: map[string]any{"command": "cat bigfile"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "[output truncated]")
	assert.Less(t, len(res.Output), 100, "output should be truncated well below original size")
}

// Test ID:   ET-PHASE29-APPROVAL
// Task ref:  tasks.json#29 remote execution tests with mock SSH server
// Feature:   RemoteBashTool description marks operation as requiring approval
func TestET_Phase29_RemoteBashToolApprovalDescription(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	mc := &phase29MockSSHClient{}
	tool := tools.NewRemoteBashTool(mc)

	desc := tool.Description()
	assert.Contains(t, desc, "REQUIRES APPROVAL")
	assert.Contains(t, desc, "remote_bash")
}

// =============================================================================
// Phase 30: Custom Tool Registration E2E Tests
// commit_spec: test(tools): add custom tool registration E2E tests
// =============================================================================

// Test ID:   ET-PHASE30-CUSTOMEXEC
// Task ref:  tasks.json#30 custom tool registration E2E tests
// Feature:   CustomCommandTool wraps and executes an external command
func TestET_Phase30_CustomToolExecution(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := tools.NewCustomCommandTool("echoer", "echoes input", []string{"echo"}, nil, nil, 0, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "tc-1",
		Args: map[string]any{"input": "hello"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "hello\n", res.Output)
	assert.Equal(t, "tc-1", res.ToolCallID)
	assert.Equal(t, 0, res.Metadata["exit_code"])
}

// Test ID:   ET-PHASE30-ENVWORKDIR
// Task ref:  tasks.json#30 custom tool registration E2E tests
// Feature:   CustomCommandTool applies env vars and working directory
func TestET_Phase30_CustomToolWithEnvAndWorkDir(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := tools.NewCustomCommandTool("envtool", "prints env and pwd",
		[]string{"sh", "-c"}, nil,
		map[string]string{"CUSTOM_E2E_VAR": "env-value-42"},
		0, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The input is the command string passed to "sh -c". It prints both the
	// env var and the working directory.
	res, err := tool.Execute(ctx, tools.ToolCall{
		Args: map[string]any{"input": "echo $CUSTOM_E2E_VAR; pwd"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "env-value-42")
	assert.Contains(t, res.Output, dir)
}

// Test ID:   ET-PHASE30-WHITELIST
// Task ref:  tasks.json#30 custom tool registration E2E tests
// Feature:   RegisterDefaults with builtin whitelist filters registered tools
func TestET_Phase30_BuiltinWhitelistFiltering(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := tools.NewDefaultToolRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := tools.RegisterDefaults(ctx, reg, tools.WithRegisteredBuiltinWhitelist([]string{"bash", "read"}))
	require.NoError(t, err)

	defs, err := reg.List(ctx)
	require.NoError(t, err)

	names := make(map[string]bool, len(defs))
	for _, d := range defs {
		names[d.Name()] = true
	}

	assert.True(t, names["bash"], "bash should be registered")
	assert.True(t, names["read"], "read should be registered")
	assert.False(t, names["write"], "write should NOT be registered")
	assert.False(t, names["edit"], "edit should NOT be registered")
	assert.False(t, names["grep"], "grep should NOT be registered")
	assert.Len(t, defs, 2, "only whitelisted tools should be registered")
}
