package tools

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGrepPureGoGlobValidation(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	empty := filepath.Join(root, "empty")
	populated := filepath.Join(root, "populated")
	require.NoError(t, os.Mkdir(empty, 0o700))
	require.NoError(t, os.Mkdir(populated, 0o700))
	file := filepath.Join(populated, "match.txt")
	require.NoError(t, os.WriteFile(file, []byte("needle\n"), 0o600))
	tool := NewGrepTool(WithGrepWorkdir(root), WithForcePureGo(true))

	for _, tc := range []struct {
		name string
		root string
	}{
		{name: "empty directory", root: empty},
		{name: "populated directory", root: populated},
		{name: "direct file", root: file},
	} {
		for _, glob := range []string{"[", "prefix[", `unterminated\`} {
			t.Run(tc.name+"/"+glob, func(t *testing.T) {
				result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{
					"pattern": "needle", "path": tc.root, "glob": glob,
				}})
				require.ErrorIs(t, err, path.ErrBadPattern)
				require.Contains(t, err.Error(), "invalid glob filter")
				require.Contains(t, err.Error(), "Correct or remove the glob argument")
				require.NotContains(t, err.Error(), glob)
				if result != nil {
					require.Empty(t, result.Output)
				}
			})
		}
	}

	for _, tc := range []struct {
		name    string
		root    string
		glob    string
		matched bool
	}{
		{name: "valid empty directory", root: empty, glob: "*.txt"},
		{name: "valid populated directory", root: populated, glob: "*.txt", matched: true},
		{name: "valid direct file", root: file, glob: "*.txt", matched: true},
		{name: "empty filter", root: file, glob: "", matched: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{
				"pattern": "needle", "path": tc.root, "glob": tc.glob,
			}})
			require.NoError(t, err)
			require.NotNil(t, result)
			if tc.matched {
				require.Contains(t, result.Output, file+":1:needle")
			} else {
				require.Empty(t, result.Output)
			}
		})
	}
}

func TestGrepPureGoGlobValidationPrecedesFilesystemButNotCancellation(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	opened := false
	open := func(string) (grepDirectoryReader, error) {
		opened = true
		return nil, os.ErrPermission
	}
	_, err := grepPureGoWithOpen(context.Background(), mustCompile(t, "needle"), missing, "[", PathWhitelist{}, newGrepMatchCapture(1, 100), open)
	require.ErrorIs(t, err, path.ErrBadPattern, "the invalid filter is reported before inspecting the missing root")
	require.False(t, opened)
	_, err = grepPureGoWithOpen(context.Background(), mustCompile(t, "needle"), root, "[", PathWhitelist{}, newGrepMatchCapture(1, 100), open)
	require.ErrorIs(t, err, path.ErrBadPattern)
	require.False(t, opened, "invalid filters must not open a directory")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = grepPureGoWithOpen(ctx, mustCompile(t, "needle"), missing, "[", PathWhitelist{}, newGrepMatchCapture(1, 100), open)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, opened)
}

func TestGrepHelperFallbackValidatesOnlyTheGoPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-local shell helper requires POSIX")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	file := filepath.Join(root, "match.txt")
	require.NoError(t, os.WriteFile(file, []byte("needle\n"), 0o600))
	marker := filepath.Join(t.TempDir(), "started")
	helperDir := writeGrepShim(t, "printf started > \"$GREP_GLOB_MARKER\"\nexit 2\n")
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GREP_GLOB_MARKER", marker)
	tool := NewGrepTool(WithGrepWorkdir(root))
	results, truncated, lifecycle, started, err := tool.search(context.Background(), mustCompile(t, "needle"), root, "[")
	require.True(t, started)
	require.True(t, lifecycle.containmentProven)
	require.True(t, lifecycle.readersJoined)
	require.FileExists(t, marker)
	require.ErrorIs(t, err, path.ErrBadPattern)
	require.False(t, truncated)
	require.Empty(t, results)
}

func TestGrepHelperKeepsItsOwnGlobGrammar(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-local shell helper requires POSIX")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	file := filepath.Join(root, "match.txt")
	require.NoError(t, os.WriteFile(file, []byte("needle\n"), 0o600))
	argsPath := filepath.Join(t.TempDir(), "args")
	event := grepJSONEvent(file, "needle", 1)
	body := "printf '%s\\n' \"$@\" > \"$GREP_GLOB_ARGS\"\nprintf '%s\\n' " + shellQuoteGrep(event) + "\n"
	helperDir := writeGrepShim(t, body)
	t.Setenv("PATH", helperDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GREP_GLOB_ARGS", argsPath)
	require.False(t, matchGlob("{*.go,*.txt}", "match.txt"), "the helper fixture exercises its own brace syntax")
	tool := NewGrepTool(WithGrepWorkdir(root))
	// The synthetic helper accepts both values, including one rejected by Go.
	// This checks routing, not whether a real rg accepts the latter syntax.
	for _, glob := range []string{"[", "{*.go,*.txt}"} {
		t.Run(glob, func(t *testing.T) {
			result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{
				"pattern": "needle", "path": root, "glob": glob,
			}})
			require.NoError(t, err)
			require.Contains(t, result.Output, file+":1:needle")
			args, err := os.ReadFile(argsPath)
			require.NoError(t, err)
			require.Contains(t, string(args), "--glob="+glob+"\n")
		})
	}
}
