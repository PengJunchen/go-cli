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

// --- P0 security: semicolon / backtick / eval / bash bypass vectors ---

// TestSandboxSemicolonBlocked ensures a semicolon-separated second command that
// is blacklisted is caught. The semicolon itself is not blocked; it is used to
// split the input so each sub-command is inspected individually.
func TestSandboxSemicolonBlocked(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "echo ok; rm file", "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

// TestSandboxBacktickBlocked ensures backtick command substitution is detected
// as a subshell and its inner command is inspected.
func TestSandboxBacktickBlocked(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "echo `rm file`", "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

// TestSandboxEvalBlocked ensures the eval builtin is blocked.
func TestSandboxEvalBlocked(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), `eval "rm file"`, "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

// TestSandboxBashSubshellBlocked ensures spawning a subshell via bash -c is
// blocked.
func TestSandboxBashSubshellBlocked(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), `bash -c "rm file"`, "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

// TestSandboxLegitimateSemicolonNotBlocked ensures that a semicolon joining two
// safe commands is allowed. Semicolons must split, not blanket-block.
func TestSandboxLegitimateSemicolonNotBlocked(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "ls; pwd", "/anywhere")
	require.NoError(t, err)
}

// TestSandboxDollarParenRegression ensures the pre-existing $(...) detection
// still works after the backtick changes.
func TestSandboxDollarParenRegression(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "echo $(rm file)", "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

// --- CommandFilter-level coverage for the new vectors ---

func TestCommandFilter_SemicolonBlockedCommand(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("echo ok; rm file"))
}

func TestCommandFilter_SemicolonSafeCommands(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked("ls; pwd"))
}

func TestCommandFilter_BacktickBlockedCommand(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("echo `rm file`"))
}

func TestCommandFilter_BacktickSafeCommand(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked("echo `date`"))
}

func TestCommandFilter_EvalBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`eval "rm file"`))
}

func TestCommandFilter_BashSubshellBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`bash -c "rm file"`))
}

func TestCommandFilter_ShSubshellBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`sh -c "rm file"`))
}

func TestCommandFilter_SourceBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("source malicious.sh"))
}

func TestCommandFilter_ExecBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("exec rm file"))
}

// --- splitCommands unit tests ---

func TestSplitCommands_Semicolon(t *testing.T) {
	got := splitCommands("echo ok; rm file")
	assert.Equal(t, []string{"echo ok", "rm file"}, got)
}

func TestSplitCommands_SemicolonMultiple(t *testing.T) {
	got := splitCommands("a; b ; c")
	assert.Equal(t, []string{"a", "b", "c"}, got)
}

func TestSplitCommands_SemicolonTrailing(t *testing.T) {
	got := splitCommands("echo hi;")
	assert.Equal(t, []string{"echo hi"}, got)
}

func TestSplitCommands_Pipe(t *testing.T) {
	got := splitCommands("echo hello | cat")
	assert.Equal(t, []string{"echo hello", "cat"}, got)
}

func TestSplitCommands_LogicalOperators(t *testing.T) {
	got := splitCommands("echo hi && rm -rf / || true")
	assert.Equal(t, []string{"echo hi", "rm -rf /", "true"}, got)
}

func TestSplitCommands_MixedSeparators(t *testing.T) {
	got := splitCommands("a; b && c | d")
	assert.Equal(t, []string{"a", "b", "c", "d"}, got)
}

// --- findSubShells unit tests ---

func TestFindSubShells_DollarParen(t *testing.T) {
	ranges := findSubShells("echo $(rm file)")
	require.Len(t, ranges, 1)
	assert.Equal(t, "rm file", "echo $(rm file)"[ranges[0].innerStart:ranges[0].innerEnd])
}

func TestFindSubShells_Backtick(t *testing.T) {
	ranges := findSubShells("echo `rm file`")
	require.Len(t, ranges, 1)
	assert.Equal(t, "rm file", "echo `rm file`"[ranges[0].innerStart:ranges[0].innerEnd])
}

func TestFindSubShells_BacktickAndDollarParen(t *testing.T) {
	ranges := findSubShells("echo `rm`; echo $(kill)")
	require.Len(t, ranges, 2)
}

func TestFindSubShells_NoSubShells(t *testing.T) {
	assert.Empty(t, findSubShells("echo hello world"))
}

// --- Interpreter & network tool blacklist tests (task 46-1) ---

func TestCommandFilter_Python3Blocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("python3 -c 'import os; os.system(\"rm -rf /\")'"))
}

func TestCommandFilter_CurlBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("curl http://example.com"))
}

func TestCommandFilter_NcBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("nc example.com 4444"))
}

func TestCommandFilter_WgetBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("wget http://example.com/file"))
}

func TestCommandFilter_PerlBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("perl -e 'system(\"rm -rf /\")'"))
}

func TestCommandFilter_NodeBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("node -e 'require(\"child_process\").execSync(\"rm -rf /\")'"))
}

func TestCommandFilter_RubyBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("ruby -e 'system(\"rm -rf /\")'"))
}

// Ensure existing legitimate commands remain allowed after the expansion.
func TestCommandFilter_LegitimateCommandsNotBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked("echo hello"))
	assert.False(t, f.IsBlocked("ls -la"))
	assert.False(t, f.IsBlocked("pwd"))
	assert.False(t, f.IsBlocked("cat file.txt"))
}

// --- Symlink bypass tests (task 46-2) ---

func TestPathWhitelist_SymlinkBypassBlocked(t *testing.T) {
	safeDir := t.TempDir()
	outsideDir := t.TempDir()
	// Create a symlink inside safeDir pointing to outsideDir.
	link := filepath.Join(safeDir, "escape")
	require.NoError(t, os.Symlink(outsideDir, link))

	wl := NewPathWhitelist([]string{safeDir})
	// Without EvalSymlinks, link would be allowed (it's under safeDir).
	// With EvalSymlinks, link resolves to outsideDir which is NOT in whitelist.
	assert.False(t, wl.IsAllowed(link), "symlink pointing outside whitelist should be blocked")
}

func TestPathWhitelist_RealNestedSymlinkAllowed(t *testing.T) {
	safeDir := t.TempDir()
	nested := filepath.Join(safeDir, "subdir")
	require.NoError(t, os.Mkdir(nested, 0o750))

	wl := NewPathWhitelist([]string{safeDir})
	// A real directory inside safeDir (no symlink) should still be allowed.
	assert.True(t, wl.IsAllowed(nested))
}

// --- Heredoc detection tests (task 46-2) ---

func TestCommandFilter_HeredocBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("cat << EOF > /etc/cron.d/evil"))
	assert.True(t, f.IsBlocked("cat <<EOF"))
	assert.True(t, f.IsBlocked("cat <<-EOF"))
}

func TestCommandFilter_NoHeredoc(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	// Commands without << should not be blocked by heredoc detection.
	assert.False(t, f.IsBlocked("echo hello"))
	assert.False(t, f.IsBlocked("ls -la"))
	// Single < (input redirection) is not a heredoc.
	assert.False(t, f.IsBlocked("cat < /etc/hostname"))
}

// --- Sandbox integration tests (task 46-2) ---

func TestSandbox_HeredocBlocked(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "cat << EOF > /etc/cron.d/evil", "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

func TestSandbox_SymlinkBypassBlocked(t *testing.T) {
	safeDir := t.TempDir()
	outsideDir := t.TempDir()
	link := filepath.Join(safeDir, "escape")
	require.NoError(t, os.Symlink(outsideDir, link))

	sb := NewDefaultBashSandbox(WithWhitelist([]string{safeDir}))
	err := sb.Validate(context.Background(), "echo hello", link)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")
}

func TestSandbox_LegitimateAfterHeredocCheck(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "echo hello", "/anywhere")
	require.NoError(t, err)

	err = sb.Validate(context.Background(), "ls -la", "/anywhere")
	require.NoError(t, err)
}

// --- Task 47-3 AC-1: Shell quoting/escape/function normalization ---

func TestNormalizeCommandToken_SingleQuote(t *testing.T) {
	assert.Equal(t, "rm", normalizeCommandToken("'rm'"))
}

func TestNormalizeCommandToken_DoubleQuote(t *testing.T) {
	assert.Equal(t, "rm", normalizeCommandToken(`"rm"`))
}

func TestNormalizeCommandToken_DollarSingleQuote(t *testing.T) {
	assert.Equal(t, "rm", normalizeCommandToken("$'rm'"))
}

func TestNormalizeCommandToken_Backslash(t *testing.T) {
	assert.Equal(t, "rm", normalizeCommandToken(`\rm`))
}

func TestNormalizeCommandToken_FunctionDef(t *testing.T) {
	assert.Equal(t, "rm", normalizeCommandToken("rm(){}"))
}

func TestNormalizeCommandToken_PlainToken(t *testing.T) {
	assert.Equal(t, "ls", normalizeCommandToken("ls"))
}

func TestCommandFilter_QuotedRm_SingleQuote(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`'rm' file`))
}

func TestCommandFilter_QuotedRm_DoubleQuote(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`"rm" file`))
}

func TestCommandFilter_QuotedRm_DollarSingleQuote(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`$'rm' file`))
}

func TestCommandFilter_QuotedRm_BackslashEscape(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`\rm file`))
}

func TestCommandFilter_FunctionDefinitionBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("rm(){ rm file; }"))
}

func TestCommandFilter_FunctionBodyBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	// Function name "helper" is not blacklisted, but its body contains "rm".
	assert.True(t, f.IsBlocked("helper(){ rm file; }"))
}

func TestCommandFilter_FunctionKeywordBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("function rm { rm file; }"))
}

func TestCommandFilter_QuotedRmWithSudo(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`sudo 'rm' file`))
	assert.True(t, f.IsBlocked(`sudo "rm" file`))
	assert.True(t, f.IsBlocked(`sudo \rm file`))
}

func TestCommandFilter_QuotedRmInSubshell(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`echo $('rm' file)`))
}

func TestCommandFilter_VariableAssignmentQuoted(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`x="rm"`))
	assert.True(t, f.IsBlocked(`x='rm'`))
	assert.True(t, f.IsBlocked(`x=$'rm'`))
	assert.True(t, f.IsBlocked(`x=\rm`))
}

// --- Task 47-3 AC-2: Indirect executors ---

func TestCommandFilter_AwkSystemBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`awk '{system("rm file")}'`))
}

func TestCommandFilter_AwkNoSystemAllowed(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked(`awk '{print $1}'`))
}

func TestCommandFilter_GawkSystemBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`gawk 'BEGIN{system("rm")}'`))
}

func TestCommandFilter_FindExecBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`find . -exec rm {} \;`))
}

func TestCommandFilter_FindExecdirBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`find . -execdir rm {} \;`))
}

func TestCommandFilter_FindNoExecAllowed(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked(`find . -name '*.go'`))
}

func TestCommandFilter_MakeFileBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`make -f /tmp/evil.mk`))
}

func TestCommandFilter_MakeLongFileBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`make --file /tmp/evil.mk`))
}

func TestCommandFilter_MakeMakefileBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`make --makefile /tmp/evil.mk`))
}

func TestCommandFilter_MakeNoFileAllowed(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked(`make build`))
}

func TestCommandFilter_GitConfigBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`git -c core.hooksPath=/tmp/x status`))
}

func TestCommandFilter_GitLongConfigBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`git --config core.hooksPath=/tmp/x status`))
}

func TestCommandFilter_GitStatusAllowed(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked(`git status`))
}

// --- Task 47-3 AC-3: env / printenv blacklist ---

func TestCommandFilter_EnvBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("env"))
	assert.True(t, f.IsBlocked("env | grep SECRET"))
}

func TestCommandFilter_PrintenvBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("printenv"))
	assert.True(t, f.IsBlocked("printenv PATH"))
}

func TestCommandFilter_EnvRmBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	// "env rm" must be blocked — env is blacklisted directly.
	assert.True(t, f.IsBlocked("env rm file"))
}

// --- Task 47-3 AC-4: Whitelist mode ---

func TestCommandFilter_WhitelistAllowsKnownPrefix(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	f.whitelist = DefaultCommandWhitelist
	assert.False(t, f.IsBlocked("go test ./..."))
	assert.False(t, f.IsBlocked("ls -la"))
	assert.False(t, f.IsBlocked("git status"))
}

func TestCommandFilter_WhitelistBlocksUnknown(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	f.whitelist = DefaultCommandWhitelist
	assert.True(t, f.IsBlocked("docker build ."))
	assert.True(t, f.IsBlocked("npm install"))
}

func TestCommandFilter_WhitelistStillBlocksBlacklisted(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	f.whitelist = DefaultCommandWhitelist
	assert.True(t, f.IsBlocked("rm -rf /"))
}

func TestCommandFilter_WhitelistEmptyDisablesMode(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.False(t, f.IsBlocked("docker build ."))
	assert.False(t, f.IsBlocked("npm install"))
}

func TestCommandFilter_WhitelistGoTestOnly(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	f.whitelist = []string{"go test"}
	assert.False(t, f.IsBlocked("go test ./..."))
	assert.True(t, f.IsBlocked("go run main.go"))
}

func TestSandbox_WhitelistModeBlocks(t *testing.T) {
	sb := NewDefaultBashSandbox(WithCommandWhitelist(DefaultCommandWhitelist))
	err := sb.Validate(context.Background(), "docker build .", "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "whitelist")
}

func TestSandbox_WhitelistModeAllows(t *testing.T) {
	sb := NewDefaultBashSandbox(WithCommandWhitelist(DefaultCommandWhitelist))
	err := sb.Validate(context.Background(), "go test ./...", "/anywhere")
	require.NoError(t, err)
}

func TestSandbox_WhitelistModeBlocksIndirectExecutor(t *testing.T) {
	sb := NewDefaultBashSandbox(WithCommandWhitelist(DefaultCommandWhitelist))
	// find is in the whitelist, but -exec is an indirect executor.
	err := sb.Validate(context.Background(), "find . -exec rm {} \\;", "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blacklist")
}

// --- Task 48-4: command substitution bypass detection ---
//
// A blacklisted command hidden inside $() or backticks (e.g. "$(echo rm)")
// resolves to that command at runtime. The sandbox cannot evaluate shell
// substitution, so it conservatively (deny-first) blocks any substitution
// whose inner content contains a blacklisted command name as a literal token.

func TestCommandFilter_SubstitutionEchoRmBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	// "$(echo rm) file" resolves to "rm file" at runtime.
	assert.True(t, f.IsBlocked("$(echo rm) file"))
}

func TestCommandFilter_BacktickEchoRmBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("`echo rm` file"))
}

func TestCommandFilter_SubstitutionPrintfRmBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("$(printf rm)"))
}

func TestCommandFilter_BacktickPrintfRmBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked("`printf rm`"))
}

func TestCommandFilter_SubstitutionPathRmBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	// Path-prefixed blacklisted command inside a substitution.
	assert.True(t, f.IsBlocked("$(echo /usr/bin/rm) file"))
}

func TestCommandFilter_SubstitutionQuotedRmBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	assert.True(t, f.IsBlocked(`$(echo 'rm') file`))
	assert.True(t, f.IsBlocked(`$(echo "rm") file`))
}

func TestCommandFilter_SubstitutionSafeDateNotBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	// echo is not blacklisted and date is not blacklisted.
	assert.False(t, f.IsBlocked("echo $(date)"))
	assert.False(t, f.IsBlocked("echo `date`"))
}

func TestCommandFilter_SubstitutionSafeLsNotBlocked(t *testing.T) {
	f := NewCommandFilter(defaultCommandBlacklist)
	// A substitution of a safe command with args must not be falsely blocked.
	assert.False(t, f.IsBlocked("echo $(ls -la)"))
}

func TestSandbox_SubstitutionEchoRmBlocked(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "$(echo rm) file", "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "substitution")
}

func TestSandbox_BacktickEchoRmBlocked(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "`echo rm` file", "/anywhere")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "substitution")
}

func TestSandbox_SubstitutionSafeDateNotBlocked(t *testing.T) {
	sb := NewDefaultBashSandbox()
	err := sb.Validate(context.Background(), "echo $(date)", "/anywhere")
	require.NoError(t, err)
}
