package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCaptureExecutable(t *testing.T, dir, name, stdout string) (string, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("capture executable fixture requires a POSIX shell")
	}
	argsPath := filepath.Join(dir, "captured-args")
	envPath := filepath.Join(dir, "captured-ripgrep-config")
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$TOOL_CAPTURE_ARGS\"\n" +
		"printf '%s' \"${RIPGREP_CONFIG_PATH-<unset>}\" > \"$TOOL_CAPTURE_RIPGREP_CONFIG\"\n" +
		"printf '%s' \"$TOOL_CAPTURE_STDOUT\"\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))
	t.Setenv("TOOL_CAPTURE_ARGS", argsPath)
	t.Setenv("TOOL_CAPTURE_RIPGREP_CONFIG", envPath)
	t.Setenv("TOOL_CAPTURE_STDOUT", stdout)
	return path, argsPath, envPath
}

func TestGrepSeparatesPatternAndIgnoresRipgrepConfig(t *testing.T) {
	dir := t.TempDir()
	_, argsPath, envPath := writeCaptureExecutable(t, dir, "rg", "fixture:1:matched\\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RIPGREP_CONFIG_PATH", filepath.Join(dir, "hostile-rg-config"))

	pattern := "--pre=tool-controlled"
	compiled, err := regexp.Compile(pattern)
	require.NoError(t, err)
	_, err = grepWithRipgrep(context.Background(), compiled, filepath.Join(dir, "root"), "")
	require.NoError(t, err)

	args, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"--no-config", "--line-number", "-n", "-e", pattern, "--", filepath.Join(dir, "root"),
	}, strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"))
	configEnv, err := os.ReadFile(envPath)
	require.NoError(t, err)
	assert.Equal(t, "<unset>", string(configEnv), "ripgrep must not inherit its executable config path")
}

func TestFindSeparatesPatternFromFdOptions(t *testing.T) {
	dir := t.TempDir()
	_, argsPath, _ := writeCaptureExecutable(t, dir, "fd", "fixture\\n")
	t.Setenv("PATH", dir)

	pattern := "--exec=tool-controlled"
	_, ok, err := findWithFd(context.Background(), dir, pattern, "f", 3)
	require.NoError(t, err)
	require.True(t, ok)

	args, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"--print0", "--glob", "--case-sensitive", "--hidden", "--no-ignore", "--type", "f", "--max-depth", "3", "--", pattern, dir}, strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"))

	_, ok, err = findWithFd(context.Background(), dir, "", "", 0)
	require.NoError(t, err)
	require.True(t, ok)
	args, err = os.ReadFile(argsPath)
	require.NoError(t, err)
	assert.Equal(t, []string{"--print0", "--glob", "--case-sensitive", "--hidden", "--no-ignore", "--", "", dir}, strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"), "an explicit empty pattern keeps the following path out of the pattern slot")
}

func writePoisonHelper(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("poison helper fixture requires a POSIX shell")
	}
	path := filepath.Join(t.TempDir(), "helper")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700))
	return path
}

func TestGitStatusDisablesConfiguredFSMonitorAndOptionalIndexWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git helper fixture requires a POSIX shell")
	}
	git := setupGitRepo(t)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	helper := writePoisonHelper(t, "printf hit >> \"$TOOL_POISON_MARKER\"\nprintf 'token\\n'\n")
	runGitInDir(t, git.cwd, "config", "core.fsmonitor", helper)
	t.Setenv("TOOL_POISON_MARKER", marker)
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")

	_, err := git.Status(context.Background())
	require.NoError(t, err)
	assert.NoFileExists(t, marker, "status must not run the configured fsmonitor helper")

	indexPath := filepath.Join(git.cwd, ".git", "index")
	before, err := os.Stat(indexPath)
	require.NoError(t, err)
	filePath := filepath.Join(git.cwd, "README.md")
	future := time.Now().Add(5 * time.Second)
	require.NoError(t, os.Chtimes(filePath, future, future), "force a stat-cache refresh candidate")
	_, err = git.Status(context.Background())
	require.NoError(t, err)
	after, err := os.Stat(indexPath)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime(), "read-only status must not rewrite the index cache")
	assert.NoFileExists(t, marker)
}

func TestGitDiffDisablesConfiguredExternalDiff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git helper fixture requires a POSIX shell")
	}
	git := setupGitRepo(t)
	marker := filepath.Join(t.TempDir(), "diff-helper-ran")
	helper := writePoisonHelper(t, "printf hit >> \"$TOOL_POISON_MARKER\"\nprintf converted\\n\n")
	runGitInDir(t, git.cwd, "config", "diff.external", helper)
	writeFileInDir(t, git.cwd, "README.md", "changed\n")
	t.Setenv("TOOL_POISON_MARKER", marker)

	_, err := git.Diff(context.Background(), GitDiffOptions{})
	require.NoError(t, err)
	assert.NoFileExists(t, marker, "diff must not run configured external diff or textconv helpers")
}

func TestGitDiffDisablesConfiguredTextconv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("git helper fixture requires a POSIX shell")
	}
	git := setupGitRepo(t)
	fileName := "fixture.converted"
	writeFileInDir(t, git.cwd, ".gitattributes", "*.converted diff=tool_safety_test\n")
	writeFileInDir(t, git.cwd, fileName, "original bytes\n")
	runGitInDir(t, git.cwd, "add", ".gitattributes", fileName)
	runGitInDir(t, git.cwd, "commit", "-m", "add textconv fixture")
	writeFileInDir(t, git.cwd, fileName, "changed bytes\n")

	marker := filepath.Join(t.TempDir(), "textconv-ran")
	helper := writePoisonHelper(t, "printf hit >> \"$TOOL_POISON_MARKER\"\nprintf converted\\n\n")
	runGitInDir(t, git.cwd, "config", "diff.tool_safety_test.textconv", helper)
	t.Setenv("TOOL_POISON_MARKER", marker)

	_, err := git.Diff(context.Background(), GitDiffOptions{})
	require.NoError(t, err)
	assert.NoFileExists(t, marker, "diff must not run configured textconv helpers")
}

func TestGitBlameTreatsLeadingDashFilenameAsPath(t *testing.T) {
	git := setupGitRepo(t)
	writeFileInDir(t, git.cwd, "--help", "line under blame\n")
	runGitInDir(t, git.cwd, "add", "--", "--help")
	runGitInDir(t, git.cwd, "commit", "-m", "add dash file")

	lines, err := git.Blame(context.Background(), "--help", 1, 1)
	require.NoError(t, err)
	require.Len(t, lines, 1)
	assert.Equal(t, "line under blame", lines[0].Content)
}

func TestGitReadCommandsKeepHostSelectedRepositoryContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git executable fixture requires a POSIX shell")
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "git-args")
	contextPath := filepath.Join(dir, "git-context")
	fake := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$TOOL_CAPTURE_ARGS\"\n" +
		"printf '%s|%s|%s|%s|%s|%s' \"${GIT_DIR-unset}\" \"${GIT_WORK_TREE-unset}\" \"${GIT_INDEX_FILE-unset}\" \"${GIT_OPTIONAL_LOCKS-unset}\" \"${GIT_CONFIG_COUNT-unset}\" \"${GIT_CONFIG_PARAMETERS-unset}\" > \"$TOOL_CAPTURE_CONTEXT\"\n" +
		"is_log=0\nfor arg in \"$@\"; do if [ \"$arg\" = log ]; then is_log=1; fi; done\n" +
		"if [ \"$is_log\" = 1 ]; then printf 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\\x1fTester\\x1ftest@example.com\\x1f2026-09-25\\x1fsubject\\n'; else printf 'true\\n'; fi\n"
	require.NoError(t, os.WriteFile(fake, []byte(script), 0o700))
	t.Setenv("TOOL_CAPTURE_ARGS", argsPath)
	t.Setenv("TOOL_CAPTURE_CONTEXT", contextPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GIT_DIR", "host-git-dir")
	t.Setenv("GIT_WORK_TREE", "host-work-tree")
	t.Setenv("GIT_INDEX_FILE", "host-index")
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "log.showSignature")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_CONFIG_PARAMETERS", "ambient override")

	git := NewDefaultGitTool(dir)
	entries, err := git.Log(context.Background(), GitLogOptions{})
	require.NoError(t, err)
	require.Len(t, entries, 1)

	args, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "diff.external=", "-c", "log.showSignature=false", "log", "--format=%H%x1f%an%x1f%ae%x1f%ad%x1f%s",
	}, strings.Split(strings.TrimSuffix(string(args), "\n"), "\n"))
	contextValues, err := os.ReadFile(contextPath)
	require.NoError(t, err)
	assert.Equal(t, "host-git-dir|host-work-tree|host-index|0|unset|unset", string(contextValues), "read hardening must preserve repository location while suppressing config overrides and optional writes")
}

func TestGitReadOnlyPreservesMissingExecutableError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	git := NewDefaultGitTool(t.TempDir())

	_, err := git.runReadOnly(context.Background(), "status")
	require.Error(t, err)
	assert.NotEmpty(t, err.Error())

	var execErr *exec.Error
	require.ErrorAs(t, err, &execErr)
	assert.Equal(t, "git", execErr.Name)
}

func TestGitReadOnlyPreservesContextCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("context cancellation fixture requires a POSIX shell")
	}
	dir := t.TempDir()
	startedPath := filepath.Join(dir, "started")
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\nprintf started > \"$TOOL_GIT_STARTED\"\nexec /bin/sleep 30\n"
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", dir)
	t.Setenv("TOOL_GIT_STARTED", startedPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		_, err := NewDefaultGitTool(dir).runReadOnly(ctx, "status")
		errCh <- err
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(startedPath)
		return err == nil
	}, 3*time.Second, 10*time.Millisecond, "fake git should start before cancellation")
	cancel()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("read-only git command did not stop after context cancellation")
	}
}

func TestGitReadOnlyErrorIncludesGitDiagnostic(t *testing.T) {
	dir := t.TempDir()
	fakeGit := filepath.Join(dir, "git")
	script := "#!/bin/sh\nprintf 'fatal: configured read failure\\n' >&2\nexit 23\n"
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", dir)

	_, err := NewDefaultGitTool(dir).runReadOnly(context.Background(), "status")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fatal: configured read failure")
}
