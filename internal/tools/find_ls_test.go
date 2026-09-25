package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// setupFindDir creates a nested temp tree for find/ls assertions.
func setupFindDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), []byte("b"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "c.go"), []byte("c"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "d.txt"), []byte("d"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "sub", "deep"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "deep", "e.txt"), []byte("e"), 0o600))
	return dir
}

func TestFindPureGoMatchesPaths(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	require.NoError(t, err)

	for _, want := range []string{"a.txt", "b.go", "sub", "sub/c.go", "sub/d.txt", "sub/deep", "sub/deep/e.txt"} {
		assert.Contains(t, res.Output, want, "expected %q in output", want)
	}
	assert.Equal(t, 7, res.Metadata["matches"])
	assert.Equal(t, false, res.Metadata["truncated"])
}

func TestFindGoReturnsGloballySortedTopKAndReportsTruncation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	const total = findMaxResults + 5
	for i := total - 1; i >= 0; i-- {
		name := fmt.Sprintf("entry-%04d.txt", i)
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	result, err := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true)).Execute(context.Background(), ToolCall{
		Args: map[string]any{"type": "f"},
	})
	require.NoError(t, err)
	want := make([]string, findMaxResults)
	for i := range want {
		want[i] = fmt.Sprintf("entry-%04d.txt", i)
	}
	assert.Equal(t, strings.Join(want, "\n"), result.Output)
	assert.Equal(t, findMaxResults, result.Metadata["matches"])
	assert.Equal(t, true, result.Metadata["truncated"])
}

func TestFindManagedBackendsReturnSameSortedTopK(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper fixtures require POSIX sh")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	for _, backend := range []string{"fd", "find"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			workdir := t.TempDir()
			for i := 1004; i >= 0; i-- {
				name := fmt.Sprintf("entry-%04d.txt", i)
				require.NoError(t, os.WriteFile(filepath.Join(workdir, name), nil, 0o600))
			}
			script := "#!/bin/sh\nn=1004\nwhile [ \"$n\" -ge 0 ]; do printf 'entry-%04d.txt\\n' \"$n\"; n=$((n - 1)); done\n"
			require.NoError(t, os.WriteFile(filepath.Join(dir, backend), []byte(script), 0o700))
			t.Setenv("PATH", dir)

			call := ToolCall{
				Args: map[string]any{"type": "f"},
			}
			result, err := NewFindTool(WithFindWorkdir(workdir)).Execute(context.Background(), call)
			require.NoError(t, err)
			want := make([]string, findMaxResults)
			for i := range want {
				want[i] = fmt.Sprintf("entry-%04d.txt", i)
			}
			assert.Equal(t, strings.Join(want, "\n"), result.Output)
			assert.Equal(t, findMaxResults, result.Metadata["matches"])
			assert.Equal(t, true, result.Metadata["truncated"])

			goResult, err := NewFindTool(WithFindWorkdir(workdir), WithFindForceNode(true)).Execute(context.Background(), call)
			require.NoError(t, err)
			assert.Equal(t, result.Output, goResult.Output, "Go and helper backends must agree for identical emitted path values")
			assert.Equal(t, result.Metadata["truncated"], goResult.Metadata["truncated"])
		})
	}
}

func TestFindCaptureLimitDrainsHelperAndStopsFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper fixtures require POSIX sh")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	helperDir := t.TempDir()
	markerDir := t.TempDir()
	completedMarker := filepath.Join(markerDir, "fd-finished")
	fallbackMarker := filepath.Join(markerDir, "find-fallback")
	fdScript := "#!/bin/sh\nprintf '%04097d\\n' 0\nprintf 'discarded-after-limit\\n'\nprintf done > " + quoteDirectoryCaptureShellArg(completedMarker) + "\n"
	findScript := "#!/bin/sh\nprintf called > " + quoteDirectoryCaptureShellArg(fallbackMarker) + "\nprintf 'fallback.txt\\n'\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(fdScript), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "find"), []byte(findScript), 0o700))
	t.Setenv("PATH", helperDir)

	workdir := t.TempDir()
	result, err := NewFindTool(WithFindWorkdir(workdir)).Execute(context.Background(), ToolCall{Args: map[string]any{}})
	require.ErrorIs(t, err, errDirectoryCaptureLimit)
	require.NotNil(t, result)
	assert.Empty(t, result.Output)
	assert.FileExists(t, completedMarker, "capture saturation must drain and join the helper before returning")
	assert.NoFileExists(t, fallbackMarker, "a typed capture error must stop backend fallback")
}

func TestFindOutputByteLimitKeepsWholeRecords(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper fixtures require POSIX sh")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	helperDir := t.TempDir()
	script := "#!/bin/sh\nn=999\nwhile [ \"$n\" -ge 0 ]; do printf '%01097d\\n' \"$n\"; n=$((n - 1)); done\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(script), 0o700))
	t.Setenv("PATH", helperDir)

	result, err := NewFindTool(WithFindWorkdir(t.TempDir())).Execute(context.Background(), ToolCall{Args: map[string]any{}})
	require.NoError(t, err)
	expectedRecords := directoryCaptureMaxOutputBytes / (1097 + 1)
	assert.Equal(t, expectedRecords, result.Metadata["matches"])
	assert.Equal(t, true, result.Metadata["truncated"])
	assert.Equal(t, expectedRecords*(1097+1)-1, len(result.Output))
	assert.NotContains(t, result.Output, "\n\n", "the byte cap must preserve complete records")
}

func TestFindRejectsOutsideWhitelistBeforeHelperDispatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper fixtures require POSIX sh")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	helperDir := t.TempDir()
	markers := []string{filepath.Join(t.TempDir(), "fd-called"), filepath.Join(t.TempDir(), "find-called")}
	for i, name := range []string{"fd", "find"} {
		script := "#!/bin/sh\nprintf called > " + quoteDirectoryCaptureShellArg(markers[i]) + "\nprintf 'unexpected\\n'\n"
		require.NoError(t, os.WriteFile(filepath.Join(helperDir, name), []byte(script), 0o700))
	}
	t.Setenv("PATH", helperDir)
	allowedDir := t.TempDir()
	outsideDir := t.TempDir()

	result, err := NewFindTool(WithFindWorkdir(allowedDir), WithFindPathWhitelist([]string{allowedDir})).Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": outsideDir},
	})
	require.Error(t, err)
	assert.Nil(t, result)
	for _, marker := range markers {
		assert.NoFileExists(t, marker, "path validation must reject before dispatching either helper")
	}
}

func quoteDirectoryCaptureShellArg(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func TestFindPureGoMatchesPattern(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "*.go"}})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "b.go")
	assert.Contains(t, res.Output, "sub/c.go")
	assert.NotContains(t, res.Output, ".txt")
}

func TestFindPureGoFiltersByTypeAndDepth(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	// type f returns only files.
	resDirs, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"type": "d"}})
	require.NoError(t, err)
	assert.Contains(t, resDirs.Output, "sub")
	assert.Contains(t, resDirs.Output, "sub/deep")
	assert.NotContains(t, resDirs.Output, ".txt")
	assert.NotContains(t, resDirs.Output, ".go")

	// max_depth 1 limits traversal to the top level.
	resShallow, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"max_depth": 1}})
	require.NoError(t, err)
	assert.Contains(t, resShallow.Output, "sub")
	assert.NotContains(t, resShallow.Output, "sub/c.go")
}

func TestFindPureGoDoesNotFollowSymlinkDirectories(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	root := t.TempDir()
	target := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(target, "secret.txt"), []byte("x"), 0o600))
	if err := os.Symlink(target, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result, err := NewFindTool(WithFindWorkdir(root), WithFindForceNode(true)).Execute(context.Background(), ToolCall{
		Args: map[string]any{"pattern": "secret.txt"},
	})
	require.NoError(t, err)
	assert.Empty(t, result.Output)
	assert.Equal(t, 0, result.Metadata["matches"])
	assert.Equal(t, false, result.Metadata["truncated"])

	fileTarget := filepath.Join(target, "linked-file-target.txt")
	require.NoError(t, os.WriteFile(fileTarget, []byte("x"), 0o600))
	fileLink := filepath.Join(t.TempDir(), "linked-file.txt")
	if err := os.Symlink(fileTarget, fileLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	rootLinkResult, err := findPureGo(context.Background(), fileLink, "linked-file.txt", "", 0)
	require.NoError(t, err)
	assert.Equal(t, []string{fileLink}, rootLinkResult.paths, "the legacy root-file path branch includes matching file symlinks")
}

func TestFindSingleFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	filePath := filepath.Join(dir, "a.txt")
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": filePath, "pattern": "*.txt"}})
	require.NoError(t, err)
	assert.Equal(t, filePath, res.Output)
}

func TestFindEmitsFindSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	e := &captureExporter{}
	tracer := tracing.NewTracer("trace-find", e)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))
	_, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "*.go"}})
	require.NoError(t, err)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.hasSpan("tool.call") }, 2*time.Second, 10*time.Millisecond)
}

func TestLSListsDirectory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool()
	// Workdir isn't used by NewLSTool's default; pass an explicit path.
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)

	for _, want := range []string{"a.txt", "b.go", "sub/"} {
		assert.Contains(t, res.Output, want)
	}
	assert.NotContains(t, res.Output, "c.go") // not an immediate child
	assert.Equal(t, 3, res.Metadata["entries"])
	assert.Equal(t, false, res.Metadata["truncated"])
}

func TestLSTruncatesToStableSortedTopKForEverySortMode(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	type expectedEntry struct {
		name string
		size int
		mod  time.Time
	}
	dir := t.TempDir()
	const total = directoryCaptureMaxEntries + 5
	entries := make([]expectedEntry, 0, total)
	baseTime := time.Unix(1_700_000_000, 0)
	for i := total - 1; i >= 0; i-- {
		name := fmt.Sprintf("entry-%04d", i)
		size := i % 5
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", size)), 0o600))
		mod := baseTime.Add(time.Duration(i%7) * time.Second)
		require.NoError(t, os.Chtimes(path, mod, mod))
		entries = append(entries, expectedEntry{name: name, size: size, mod: mod})
	}

	for _, sortBy := range []string{"name", "time", "size"} {
		t.Run(sortBy, func(t *testing.T) {
			ordered := append([]expectedEntry(nil), entries...)
			sort.Slice(ordered, func(i, j int) bool {
				switch sortBy {
				case "time":
					if !ordered[i].mod.Equal(ordered[j].mod) {
						return ordered[i].mod.After(ordered[j].mod)
					}
				case "size":
					if ordered[i].size != ordered[j].size {
						return ordered[i].size < ordered[j].size
					}
				}
				return ordered[i].name < ordered[j].name
			})

			result, err := NewLSTool().Execute(context.Background(), ToolCall{
				Args: map[string]any{"path": dir, "sort": sortBy},
			})
			require.NoError(t, err)
			want := make([]string, directoryCaptureMaxEntries)
			for i := range want {
				want[i] = ordered[i].name
			}
			assert.Equal(t, strings.Join(want, "\n"), result.Output)
			assert.Equal(t, directoryCaptureMaxEntries, result.Metadata["entries"])
			assert.Equal(t, true, result.Metadata["truncated"])
		})
	}
}

func TestLSAllIncludesDotfiles(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("x"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".hidden"), []byte("h"), 0o600))

	tool := NewLSTool()
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "all": true}})
	require.NoError(t, err)
	assert.Contains(t, res.Output, ".hidden")
	assert.Equal(t, 2, res.Metadata["entries"])

	resNoAll, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)
	assert.NotContains(t, resNoAll.Output, ".hidden")
	assert.Equal(t, 1, resNoAll.Metadata["entries"])
}

func TestLSLongFormat(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool()
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "long": true}})
	require.NoError(t, err)
	// Long format includes a mode string and size.
	assert.Contains(t, res.Output, "-rw")
	assert.Contains(t, res.Output, "a.txt")
}

func TestLSSortBehaviors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool()

	// name sort is default and stable.
	resName, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)
	assert.True(t, strings.Index(resName.Output, "a.txt") < strings.Index(resName.Output, "sub"))

	// size sort orders single-byte files before the (larger) directory listing.
	resSize, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "sort": "size"}})
	require.NoError(t, err)
	assert.Contains(t, resSize.Output, "a.txt")

	// time sort is accepted without error.
	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "sort": "time"}})
	require.NoError(t, err)

	// invalid sort errors.
	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "sort": "bogus"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sort")
}

func TestLSNonDirectoryErrors(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewLSTool()
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": filepath.Join(dir, "a.txt")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory")
}

func TestLSEmitsLSSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	e := &captureExporter{}
	tracer := tracing.NewTracer("trace-ls", e)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	tool := NewLSTool()
	dir := setupFindDir(t)
	_, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.hasSpan("tool.call") }, 2*time.Second, 10*time.Millisecond)
}

func TestFindAndLSName(t *testing.T) {
	assert.Equal(t, "find", NewFindTool().Name())
	assert.Contains(t, NewFindTool().Description(), "pattern")
	assert.Equal(t, "ls", NewLSTool().Name())
	assert.Contains(t, NewLSTool().Description(), "path")
}

func TestFindPureGoNonExistentPath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewFindTool(WithFindForceNode(true))
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": "/nonexistent/path/xyz"}})
	require.Error(t, err)
}

func TestFindPureGoCancelledContext(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Execute(ctx, ToolCall{Args: map[string]any{}})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestFindPureGoTypeFile(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"type": "f"}})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "a.txt")
	assert.Contains(t, res.Output, "b.go")
	// Directories should be excluded, but file paths under subdirectories remain.
	assert.NotContains(t, res.Output, "sub\n")
	assert.NotContains(t, res.Output, "sub/deep\n")
}

func TestFindSingleFileNoMatch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	filePath := filepath.Join(dir, "a.txt")
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": filePath, "pattern": "*.go"}})
	require.NoError(t, err)
	assert.Empty(t, res.Output)
}

func TestFindPureGoEmptyPatternMatchesAll(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": ""}})
	require.NoError(t, err)
	assert.Equal(t, 7, res.Metadata["matches"])
}

func TestFindMaxResultsTruncation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := setupFindDir(t)
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	require.NoError(t, err)
	// All 7 entries fit under the findMaxResults cap.
	assert.Equal(t, 7, res.Metadata["matches"])
}

func TestLSEmptyDirectory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewLSTool()
	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)
	assert.Equal(t, 0, res.Metadata["entries"])
}
