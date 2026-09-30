package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
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
			script := "#!/bin/sh\nn=1004\nwhile [ \"$n\" -ge 0 ]; do printf 'entry-%04d.txt\\000' \"$n\"; n=$((n - 1)); done\n"
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

func TestDirectoryToolsRenderUnusualNamesReversibly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture filenames require POSIX path bytes")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	names := writeUnusualDirectoryCaptureFiles(t, dir)
	wantByName := map[string]string{
		"   ":                strconv.Quote("   "),
		"odd\nname":          strconv.Quote("odd\nname"),
		"ordinary name":      "ordinary name",
		string([]byte{0xff}): strconv.Quote(string([]byte{0xff})),
	}
	want := make([]string, 0, len(names))
	for _, name := range names {
		want = append(want, wantByName[name])
	}
	wantOutput := strings.Join(want, "\n")

	readResult, err := NewReadTool(WithWorkdir(dir)).Execute(context.Background(), ToolCall{Args: map[string]any{"path": "."}})
	require.NoError(t, err)
	assert.Equal(t, wantOutput, readResult.Output)
	assert.Equal(t, len(names), readResult.Metadata["entries"])
	assert.Equal(t, false, readResult.Metadata["truncated"])

	lsResult, err := NewLSTool().Execute(context.Background(), ToolCall{Args: map[string]any{"path": dir}})
	require.NoError(t, err)
	assert.Equal(t, wantOutput, lsResult.Output)
	assert.Equal(t, len(names), lsResult.Metadata["entries"])
	assert.Equal(t, false, lsResult.Metadata["truncated"])

	findResult, err := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true)).Execute(context.Background(), ToolCall{Args: map[string]any{"type": "f"}})
	require.NoError(t, err)
	assert.Equal(t, wantOutput, findResult.Output)
	assert.Equal(t, len(names), findResult.Metadata["matches"])
	assert.Equal(t, false, findResult.Metadata["truncated"])
}

func TestFindFDShimUsesNULRecordsForUnusualNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper fixtures require POSIX sh")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	helperDir := t.TempDir()
	script := "#!/bin/sh\nfor arg do\n  if [ \"$arg\" = \"--print0\" ]; then print0=yes; fi\ndone\n[ \"$print0\" = yes ] || exit 23\nprintf 'odd\\nname\\000   \\000\\377\\000\\000'\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(script), 0o700))
	t.Setenv("PATH", helperDir)

	result, err := NewFindTool(WithFindWorkdir(t.TempDir())).Execute(context.Background(), ToolCall{Args: map[string]any{"type": "f"}})
	require.NoError(t, err)
	want := strings.Join([]string{strconv.Quote("   "), strconv.Quote("odd\nname"), strconv.Quote(string([]byte{0xff}))}, "\n")
	assert.Equal(t, want, result.Output)
	assert.Equal(t, 3, result.Metadata["matches"])
	assert.Equal(t, false, result.Metadata["truncated"])
}

func TestFindSystemFindUsesNULRecordsForUnusualNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("system find fixture requires POSIX symlinks and paths")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	findPath, err := exec.LookPath("find")
	require.NoError(t, err)
	dir := t.TempDir()
	names := writeUnusualDirectoryCaptureFiles(t, dir)
	helperDir := t.TempDir()
	require.NoError(t, os.Symlink(findPath, filepath.Join(helperDir, "find")))
	t.Setenv("PATH", helperDir)

	result, err := NewFindTool(WithFindWorkdir(dir)).Execute(context.Background(), ToolCall{Args: map[string]any{"type": "f"}})
	require.NoError(t, err)
	wantPaths := make([]string, 0, len(names))
	for _, name := range names {
		path := name
		if name == "   " || name == "odd\nname" || !isValidUTF8CaptureName(name) {
			path = strconv.Quote(path)
		}
		wantPaths = append(wantPaths, path)
	}
	assert.Equal(t, strings.Join(wantPaths, "\n"), result.Output)
	assert.Equal(t, len(names), result.Metadata["matches"])
	assert.Equal(t, false, result.Metadata["truncated"])
}

func TestFindSystemFindNormalizesDirectoryPathsToSearchRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("system find fixture requires POSIX paths")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	findPath, err := exec.LookPath("find")
	require.NoError(t, err)
	helperDir := t.TempDir()
	require.NoError(t, os.Symlink(findPath, filepath.Join(helperDir, "find")))
	t.Setenv("PATH", helperDir)

	dir := setupFindDir(t)
	t.Chdir(dir)

	for _, tc := range []struct {
		name string
		tool *FindTool
		path string
		want string
	}{
		{name: "dot root", tool: NewFindTool(), path: ".", want: "a.txt\nb.go\nsub\nsub/c.go\nsub/d.txt\nsub/deep\nsub/deep/e.txt"},
		{name: "absolute root", tool: NewFindTool(WithFindWorkdir("")), path: dir, want: "a.txt\nb.go\nsub\nsub/c.go\nsub/d.txt\nsub/deep\nsub/deep/e.txt"},
		{name: "nested relative root", tool: NewFindTool(), path: "sub", want: "c.go\nd.txt\ndeep\ndeep/e.txt"},
		{name: "nested absolute root", tool: NewFindTool(WithFindWorkdir("")), path: filepath.Join(dir, "sub"), want: "c.go\nd.txt\ndeep\ndeep/e.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": tc.path}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.Output)
			assert.Equal(t, strings.Count(tc.want, "\n")+1, result.Metadata["matches"])
			assert.False(t, result.Metadata["truncated"].(bool))
		})
	}
}

func TestFindFDNormalizesDirectoryPathsToSearchRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fd fixture requires POSIX paths")
	}
	fdPath, err := exec.LookPath("fd")
	if err != nil {
		t.Skip("fd is not installed")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	helperDir := t.TempDir()
	require.NoError(t, os.Symlink(fdPath, filepath.Join(helperDir, "fd")))
	t.Setenv("PATH", helperDir)

	dir := setupFindDir(t)
	t.Chdir(dir)

	for _, tc := range []struct {
		name string
		tool *FindTool
		path string
		want string
	}{
		{name: "dot root", tool: NewFindTool(), path: ".", want: "a.txt\nb.go\nsub\nsub/c.go\nsub/d.txt\nsub/deep\nsub/deep/e.txt"},
		{name: "absolute root", tool: NewFindTool(WithFindWorkdir("")), path: dir, want: "a.txt\nb.go\nsub\nsub/c.go\nsub/d.txt\nsub/deep\nsub/deep/e.txt"},
		{name: "nested relative root", tool: NewFindTool(), path: "sub", want: "c.go\nd.txt\ndeep\ndeep/e.txt"},
		{name: "nested absolute root", tool: NewFindTool(WithFindWorkdir("")), path: filepath.Join(dir, "sub"), want: "c.go\nd.txt\ndeep\ndeep/e.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": tc.path}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.Output)
			assert.Equal(t, strings.Count(tc.want, "\n")+1, result.Metadata["matches"])
			assert.False(t, result.Metadata["truncated"].(bool))
		})
	}
}

func TestFindFDShimNormalizesDirectoryPathsToSearchRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fd shim requires POSIX shell")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	helperDir := t.TempDir()
	marker := filepath.Join(helperDir, "fd-called")
	script := "#!/bin/sh\nprintf called > " + quoteDirectoryCaptureShellArg(marker) + "\n" + `
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--" ]; then shift; pattern=$1; root=$2; break; fi
  shift
done
[ "$pattern" = "" ] || exit 23
case "$root" in
  .) printf '.\000./sub/deep/e.txt\000./a.txt\000./sub/c.go\000./b.go\000./sub/d.txt\000./sub/deep\000./sub\000' ;;
  sub) printf 'sub\000sub/deep/e.txt\000sub/c.go\000sub/d.txt\000sub/deep\000' ;;
  */sub) printf '%s\000%s/deep/e.txt\000%s/c.go\000%s/d.txt\000%s/deep\000' "$root" "$root" "$root" "$root" "$root" ;;
  *) printf '%s\000%s/sub/deep/e.txt\000%s/a.txt\000%s/sub/c.go\000%s/b.go\000%s/sub/d.txt\000%s/sub/deep\000%s/sub\000' "$root" "$root" "$root" "$root" "$root" "$root" "$root" "$root" ;;
esac
`
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(script), 0o700))
	t.Setenv("PATH", helperDir)

	dir := setupFindDir(t)
	t.Chdir(dir)

	for _, tc := range []struct {
		name string
		tool *FindTool
		path string
		want string
	}{
		{name: "dot root", tool: NewFindTool(), path: ".", want: "a.txt\nb.go\nsub\nsub/c.go\nsub/d.txt\nsub/deep\nsub/deep/e.txt"},
		{name: "absolute root", tool: NewFindTool(WithFindWorkdir("")), path: dir, want: "a.txt\nb.go\nsub\nsub/c.go\nsub/d.txt\nsub/deep\nsub/deep/e.txt"},
		{name: "nested relative root", tool: NewFindTool(), path: "sub", want: "c.go\nd.txt\ndeep\ndeep/e.txt"},
		{name: "nested absolute root", tool: NewFindTool(WithFindWorkdir("")), path: filepath.Join(dir, "sub"), want: "c.go\nd.txt\ndeep\ndeep/e.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(marker, nil, 0o600))
			result, err := tc.tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": tc.path}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, result.Output)
			assert.Equal(t, strings.Count(tc.want, "\n")+1, result.Metadata["matches"])
			assert.False(t, result.Metadata["truncated"].(bool))
			data, err := os.ReadFile(marker)
			require.NoError(t, err)
			assert.Equal(t, "called", string(data), "the exact-output assertion must exercise the fd shim")
		})
	}
}

func TestFindGlobPatternMatchesSameEntriesAcrossBackends(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backend fixtures require POSIX helper shims")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	findPath, err := exec.LookPath("find")
	require.NoError(t, err)
	dir := t.TempDir()
	for _, name := range []string{"foo.txt", "FOO.txt", "xfoo.txt", ".foo-hidden.txt", "ignored-foo.txt"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("xfoo.txt\nignored-foo.txt\n"), 0o600))
	call := ToolCall{Args: map[string]any{"pattern": "*foo*.txt", "type": "f"}}

	goResult, err := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true)).Execute(context.Background(), call)
	require.NoError(t, err)
	assert.Equal(t, []string{".foo-hidden.txt", "foo.txt", "ignored-foo.txt", "xfoo.txt"}, sortedFindOutputBasenames(goResult.Output))
	assert.Equal(t, 4, goResult.Metadata["matches"])

	fdDir := t.TempDir()
	fdScript := fmt.Sprintf(`#!/bin/sh
glob=no
print0=no
kind=
	pattern=
	root=
	while [ "$#" -gt 0 ]; do
  case "$1" in
    --glob) glob=yes; shift ;;
    --print0) print0=yes; shift ;;
    --case-sensitive) case_sensitive=yes; shift ;;
    --hidden) hidden=yes; shift ;;
    --no-ignore) no_ignore=yes; shift ;;
    --type) kind=$2; shift 2 ;;
    --) shift; pattern=$1; root=$2; break ;;
    *) shift ;;
  esac
done
[ "$glob" = yes ] && [ "$print0" = yes ] && [ "$case_sensitive" = yes ] && [ "$hidden" = yes ] && [ "$no_ignore" = yes ] && [ "$pattern" = '*foo*.txt' ] || exit 24
exec %s "$root" -mindepth 1 -type "$kind" -name "$pattern" -print0
`, quoteDirectoryCaptureShellArg(findPath))
	require.NoError(t, os.WriteFile(filepath.Join(fdDir, "fd"), []byte(fdScript), 0o700))
	t.Setenv("PATH", fdDir)
	fdResult, err := NewFindTool(WithFindWorkdir(dir)).Execute(context.Background(), call)
	require.NoError(t, err)
	assert.Equal(t, []string{".foo-hidden.txt", "foo.txt", "ignored-foo.txt", "xfoo.txt"}, sortedFindOutputBasenames(fdResult.Output))
	assert.Equal(t, 4, fdResult.Metadata["matches"])

	systemFindDir := t.TempDir()
	require.NoError(t, os.Symlink(findPath, filepath.Join(systemFindDir, "find")))
	t.Setenv("PATH", systemFindDir)
	systemResult, err := NewFindTool(WithFindWorkdir(dir)).Execute(context.Background(), call)
	require.NoError(t, err)
	assert.Equal(t, []string{".foo-hidden.txt", "foo.txt", "ignored-foo.txt", "xfoo.txt"}, sortedFindOutputBasenames(systemResult.Output))
	assert.Equal(t, 4, systemResult.Metadata["matches"])
}

func sortedFindOutputBasenames(output string) []string {
	if output == "" {
		return []string{}
	}
	lines := strings.Split(output, "\n")
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		names = append(names, filepath.Base(line))
	}
	sort.Strings(names)
	return names
}

func TestFindSystemFindExcludesDirectoryRootAndFileRootIsIncluded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("system find fixture requires POSIX symlinks")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	findPath, err := exec.LookPath("find")
	require.NoError(t, err)
	helperDir := t.TempDir()
	require.NoError(t, os.Symlink(findPath, filepath.Join(helperDir, "find")))
	t.Setenv("PATH", helperDir)

	dir := t.TempDir()
	rootName := filepath.Base(dir)
	call := ToolCall{Args: map[string]any{"path": ".", "pattern": rootName}}
	systemDirectoryResult, err := NewFindTool(WithFindWorkdir(dir)).Execute(context.Background(), call)
	require.NoError(t, err)
	assert.Empty(t, systemDirectoryResult.Output)
	assert.Equal(t, 0, systemDirectoryResult.Metadata["matches"])

	goDirectoryResult, err := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true)).Execute(context.Background(), call)
	require.NoError(t, err)
	assert.Empty(t, goDirectoryResult.Output)
	assert.Equal(t, 0, goDirectoryResult.Metadata["matches"])

	fdMarker := filepath.Join(helperDir, "fd-called")
	fdScript := "#!/bin/sh\nprintf called > " + quoteDirectoryCaptureShellArg(fdMarker) + "\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(fdScript), 0o700))
	rootFile := filepath.Join(dir, "root-file.txt")
	require.NoError(t, os.WriteFile(rootFile, nil, 0o600))
	fileCall := ToolCall{Args: map[string]any{"path": rootFile, "pattern": "root-file.txt", "type": "f"}}
	fileRootResult, err := NewFindTool(WithFindWorkdir(dir)).Execute(context.Background(), fileCall)
	require.NoError(t, err)
	assert.Equal(t, rootFile, fileRootResult.Output)
	assert.Equal(t, 1, fileRootResult.Metadata["matches"])
	assert.NoFileExists(t, fdMarker, "a file root must be handled before directory-only helpers")
	goFileResult, err := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true)).Execute(context.Background(), fileCall)
	require.NoError(t, err)
	assert.Equal(t, rootFile, goFileResult.Output)
	assert.Equal(t, fileRootResult.Metadata["matches"], goFileResult.Metadata["matches"])

	t.Chdir(dir)
	relativeFileResult, err := NewFindTool().Execute(context.Background(), ToolCall{Args: map[string]any{
		"path": "root-file.txt", "pattern": "root-file.txt", "type": "f",
	}})
	require.NoError(t, err)
	assert.Equal(t, "root-file.txt", relativeFileResult.Output)
}

func TestFindFDZeroExitWithNoResultsReturnsEmptyResult(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper fixtures require POSIX sh")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	helperDir := t.TempDir()
	fdMarker := filepath.Join(helperDir, "fd-called")
	findMarker := filepath.Join(helperDir, "find-called")
	fdScript := "#!/bin/sh\nprintf called > " + quoteDirectoryCaptureShellArg(fdMarker) + "\nexit 0\n"
	findScript := "#!/bin/sh\nprintf called > " + quoteDirectoryCaptureShellArg(findMarker) + "\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(fdScript), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "find"), []byte(findScript), 0o700))
	t.Setenv("PATH", helperDir)

	result, err := NewFindTool(WithFindWorkdir(t.TempDir())).Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "missing*.txt", "type": "f"}})
	require.NoError(t, err)
	assert.Empty(t, result.Output)
	assert.Equal(t, 0, result.Metadata["matches"])
	assert.Equal(t, false, result.Metadata["truncated"])
	assert.FileExists(t, fdMarker)
	assert.NoFileExists(t, findMarker)
}

func TestFindFDExitOneFallsBackAsHelperError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper fixtures require POSIX sh")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	helperDir := t.TempDir()
	fdMarker := filepath.Join(helperDir, "fd-called")
	findMarker := filepath.Join(helperDir, "find-called")
	fdScript := "#!/bin/sh\nprintf called > " + quoteDirectoryCaptureShellArg(fdMarker) + "\nprintf 'partial.txt\\000'\nexit 1\n"
	findScript := "#!/bin/sh\nprintf called > " + quoteDirectoryCaptureShellArg(findMarker) + "\nprintf 'fallback.txt\\000'\n"
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(fdScript), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(helperDir, "find"), []byte(findScript), 0o700))
	t.Setenv("PATH", helperDir)

	result, err := NewFindTool(WithFindWorkdir(t.TempDir())).Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "*.txt", "type": "f"}})
	require.NoError(t, err)
	assert.Equal(t, "fallback.txt", result.Output)
	assert.Equal(t, 1, result.Metadata["matches"])
	assert.FileExists(t, fdMarker)
	assert.FileExists(t, findMarker)
}

func TestFindFDArgvKeepsDashPatternsAfterOptionSeparator(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper fixtures require POSIX sh")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	for _, pattern := range []string{"--exec=touch", "-glob"} {
		t.Run(pattern, func(t *testing.T) {
			helperDir := t.TempDir()
			workdir := t.TempDir()
			argvFile := filepath.Join(helperDir, "argv")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + quoteDirectoryCaptureShellArg(argvFile) + "\n"
			require.NoError(t, os.WriteFile(filepath.Join(helperDir, "fd"), []byte(script), 0o700))
			t.Setenv("PATH", helperDir)

			_, err := NewFindTool(WithFindWorkdir(workdir)).Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": pattern, "type": "f"}})
			require.NoError(t, err)
			data, err := os.ReadFile(argvFile)
			require.NoError(t, err)
			assert.Equal(t, []string{"--print0", "--glob", "--case-sensitive", "--hidden", "--no-ignore", "--type", "f", "--", pattern, filepath.Clean(workdir)}, strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"))
		})
	}
}

func TestFindAvailableFDDoesNotExecuteDashPattern(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fd integration fixture requires POSIX filenames")
	}
	fdPath, err := exec.LookPath("fd")
	if err != nil {
		t.Skip("fd is not installed")
	}
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	path := filepath.Join(dir, "--exec=touch")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	oldTime := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.Local)
	require.NoError(t, os.Chtimes(path, oldTime, oldTime))
	t.Setenv("PATH", filepath.Dir(fdPath))

	result, err := NewFindTool(WithFindWorkdir(dir)).Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "--exec=touch", "type": "f"}})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Metadata["matches"])
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(oldTime), "a dash-prefixed pattern must not activate fd's command option")
}

func writeUnusualDirectoryCaptureFiles(t *testing.T, dir string) []string {
	t.Helper()
	names := []string{"odd\nname", "   ", "ordinary name"}
	if runtime.GOOS != "darwin" {
		// APFS/HFS rejects invalid UTF-8 path components. The capture-writer and
		// fd-shim tests still exercise raw invalid bytes on every POSIX platform.
		names = append(names, string([]byte{0xff}))
	}
	for _, name := range names {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	sort.Strings(names)
	return names
}

func isValidUTF8CaptureName(value string) bool {
	return strings.ToValidUTF8(value, "") == value
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
	fdScript := "#!/bin/sh\nprintf '%04097d\\000' 0\nprintf 'discarded-after-limit\\000'\nprintf done > " + quoteDirectoryCaptureShellArg(completedMarker) + "\n"
	findScript := "#!/bin/sh\nprintf called > " + quoteDirectoryCaptureShellArg(fallbackMarker) + "\nprintf 'fallback.txt\\000'\n"
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
	script := "#!/bin/sh\nn=999\nwhile [ \"$n\" -ge 0 ]; do printf '%01097d\\000' \"$n\"; n=$((n - 1)); done\n"
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
