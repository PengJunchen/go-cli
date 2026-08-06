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

// --- PathWhitelist tests ---

func TestPathWhitelist_AllowedPath(t *testing.T) {
	wl := NewPathWhitelist([]string{"/tmp", "/home/user"})
	assert.True(t, wl.IsAllowed("/tmp/work"))
	assert.True(t, wl.IsAllowed("/home/user/project"))
}

func TestPathWhitelist_DisallowedPath(t *testing.T) {
	wl := NewPathWhitelist([]string{"/tmp"})
	assert.False(t, wl.IsAllowed("/etc/passwd"))
	assert.False(t, wl.IsAllowed("/var/log"))
}

func TestPathWhitelist_EmptyAllowsAll(t *testing.T) {
	wl := NewPathWhitelist(nil)
	// Empty whitelist means no restriction.
	assert.True(t, wl.IsAllowed("/any/path"))
	assert.True(t, wl.IsAllowed("/etc/passwd"))
}

func TestPathWhitelist_ExactPathAllowed(t *testing.T) {
	wl := NewPathWhitelist([]string{"/tmp"})
	assert.True(t, wl.IsAllowed("/tmp"))
}

// --- CommandFilter tests ---

func TestCommandFilter_BlockedCommand(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("rm -rf /"))
}

func TestCommandFilter_AllowedCommand(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked("echo hello"))
}

func TestCommandFilter_AllowedCommandWithArgs(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked("ls -la"))
}

// --- Pipe / substitution / operator handling ---

func TestCommandFilter_PipeWithBlockedCommand(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("rm -rf / | cat"))
}

func TestCommandFilter_CommandSubstitution(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("$(rm -rf /)"))
}

func TestCommandFilter_LogicalOperator(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("echo hi && rm -rf /"))
}

func TestCommandFilter_PipeSafeCommands(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked("echo hello | cat"))
}

func TestCommandFilter_CommandSubstitutionSafe(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked("echo $(date)"))
}

// --- DefaultBashSandbox.Validate tests ---

func TestSandbox_WhitelistedPathSafeCommand(t *testing.T) {
	dir := t.TempDir()
	sb := NewDefaultBashSandbox(WithWhitelist([]string{dir}))
	err := sb.Validate(context.Background(), "echo hello", dir)
	require.NoError(t, err)
}

func TestSandbox_NonWhitelistedPath(t *testing.T) {
	dir := t.TempDir()
	sb := NewDefaultBashSandbox(WithWhitelist([]string{dir}))
	err := sb.Validate(context.Background(), "echo hello", "/etc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")
}

func TestSandbox_BlacklistedCommandWhitelistedPath(t *testing.T) {
	dir := t.TempDir()
	sb := NewDefaultBashSandbox(WithWhitelist([]string{dir}))
	err := sb.Validate(context.Background(), "rm -rf /", dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

func TestSandbox_PipeWithBlacklistedCommand(t *testing.T) {
	dir := t.TempDir()
	sb := NewDefaultBashSandbox(WithWhitelist([]string{dir}))
	err := sb.Validate(context.Background(), "rm -rf / | cat", dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

func TestSandbox_CommandSubstitutionWithBlacklistedCommand(t *testing.T) {
	dir := t.TempDir()
	sb := NewDefaultBashSandbox(WithWhitelist([]string{dir}))
	err := sb.Validate(context.Background(), "echo $(rm -rf /)", dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

func TestSandbox_SafeCommandWhitelistedPath(t *testing.T) {
	dir := t.TempDir()
	sb := NewDefaultBashSandbox(WithWhitelist([]string{dir}))
	err := sb.Validate(context.Background(), "ls -la", dir)
	require.NoError(t, err)
}

func TestSandbox_EmptyWhitelistAllowsAnyPath(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "echo hello", "/anywhere")
	require.NoError(t, err)
}

func TestSandbox_EmptyWhitelistStillBlocksBlacklist(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "rm -rf /", "/anywhere")
	require.Error(t, err)
}

// --- BashTool with sandbox integration tests ---

func TestBashTool_SandboxSafeCommandExecutes(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	sb := NewDefaultBashSandbox(WithWhitelist([]string{dir}))
	tool := NewBashTool(WithBashWorkdir(dir), WithBashSandbox(sb))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo sandboxed"},
	})
	require.NoError(t, err)
	assert.Equal(t, "sandboxed\n", res.Output)
}

func TestBashTool_SandboxBlocksBlacklistedCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	sb := NewDefaultBashSandbox(WithWhitelist([]string{dir}))
	tool := NewBashTool(WithBashWorkdir(dir), WithBashSandbox(sb))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "rm -rf /"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox")
}

func TestBashTool_SandboxBlocksCommandWithoutExecuting(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	// Custom sandbox that blocks "touch"; if the command ran, the marker
	// file would exist. Proving the file is absent proves the sandbox
	// stopped execution before bash ran.
	marker := filepath.Join(dir, "marker")
	sb := NewDefaultBashSandbox(WithWhitelist([]string{dir}), WithBlacklist([]string{"touch"}))
	tool := NewBashTool(WithBashWorkdir(dir), WithBashSandbox(sb))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "touch " + marker},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox")
	_, statErr := os.Stat(marker)
	assert.Error(t, statErr, "marker file should not exist; command must not have executed")
}

// --- WithAllowedPaths tests ---

func TestSandbox_WithAllowedPaths_EmptyDefaultsToCWD(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	sb := NewDefaultBashSandbox(WithAllowedPaths(nil))
	// CWD should be allowed.
	err = sb.Validate(context.Background(), "echo hello", cwd)
	require.NoError(t, err)
}

func TestSandbox_WithAllowedPaths_EmptyBlocksOutsideCWD(t *testing.T) {
	sb := NewDefaultBashSandbox(WithAllowedPaths(nil))
	// A path outside CWD should be blocked.
	err := sb.Validate(context.Background(), "echo hello", "/etc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")
}

func TestSandbox_WithAllowedPaths_SpecificPaths(t *testing.T) {
	dir := t.TempDir()
	sb := NewDefaultBashSandbox(WithAllowedPaths([]string{dir}))
	// The specified path is allowed.
	err := sb.Validate(context.Background(), "echo hello", dir)
	require.NoError(t, err)
	// A different path is blocked.
	err = sb.Validate(context.Background(), "echo hello", "/etc")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")
}

func TestSandbox_WithAllowedPaths_NestedDirectoryAllowed(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "subdir")
	require.NoError(t, os.Mkdir(nested, 0o750))

	sb := NewDefaultBashSandbox(WithAllowedPaths([]string{dir}))
	err := sb.Validate(context.Background(), "echo hello", nested)
	require.NoError(t, err)
}
