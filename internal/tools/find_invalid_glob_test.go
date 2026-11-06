package tools

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFindPureGoInvalidGlobFeedback(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	files := filepath.Join(root, "files")
	require.NoError(t, os.Mkdir(empty, 0o700))
	require.NoError(t, os.Mkdir(files, 0o700))
	file := filepath.Join(files, "target.txt")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	tool := NewFindTool(WithFindWorkdir(root), WithFindForceNode(true))
	for _, tc := range []struct{ name, root string }{
		{"nonempty directory", files}, {"empty directory", empty}, {"single file", file},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, pattern := range []string{"[", "prefix*[", "trailing\\"} {
				t.Run(pattern, func(t *testing.T) {
					result, err := tool.Execute(t.Context(), ToolCall{Args: map[string]any{"path": tc.root, "pattern": pattern}})
					require.ErrorIs(t, err, path.ErrBadPattern)
					require.Contains(t, err.Error(), "Correct or remove the pattern argument")
					require.NotNil(t, result)
					require.Empty(t, result.Output)
				})
			}
		})
	}
}

func TestFindPureGoGlobValidationKeepsValidResults(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.go"), nil, 0o600))
	tool := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true))
	for _, tc := range []struct {
		pattern, want string
		count         int
	}{
		{"", "a.txt\nb.go", 2}, {"*.txt", "a.txt", 1}, {"[ab]*", "a.txt\nb.go", 2}, {"*.absent", "", 0},
	} {
		t.Run(tc.pattern, func(t *testing.T) {
			result, err := tool.Execute(t.Context(), ToolCall{Args: map[string]any{"pattern": tc.pattern}})
			require.NoError(t, err)
			require.False(t, result.IsError)
			require.Equal(t, tc.want, result.Output)
			require.Equal(t, tc.count, result.Metadata["matches"])
			require.Equal(t, false, result.Metadata["truncated"])
		})
	}
}

func TestFindPureGoGlobValidationPrecedesFilesystemButNotCancellation(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := findPureGo(t.Context(), missing, "[", "", 0)
	require.ErrorIs(t, err, path.ErrBadPattern)
	require.NotErrorIs(t, err, os.ErrNotExist)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = findPureGo(ctx, missing, "[", "", 0)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, path.ErrBadPattern)
	tool := NewFindTool(WithFindWorkdir(t.TempDir()), WithFindForceNode(true))
	_, err = tool.Execute(ctx, ToolCall{Args: map[string]any{"pattern": "["}})
	require.ErrorIs(t, err, context.Canceled)
}

func TestFindHelperGlobDialectRemainsUnchanged(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper fixture")
	}
	dir := t.TempDir()
	helpers := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "arguments")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kept.txt"), nil, 0o600))
	script := "#!/bin/sh\nprintf '%s\n' \"$@\" > \"$FIND_GLOB_ARGUMENTS\"\nprintf 'kept.txt\\000'\n"
	require.NoError(t, os.WriteFile(filepath.Join(helpers, "fd"), []byte(script), 0o700))
	t.Setenv("PATH", helpers)
	t.Setenv("FIND_GLOB_ARGUMENTS", argsFile)
	result, err := NewFindTool(WithFindWorkdir(dir)).Execute(t.Context(), ToolCall{Args: map[string]any{"pattern": "["}})
	require.NoError(t, err)
	require.Equal(t, "kept.txt", result.Output)
	args, err := os.ReadFile(argsFile)
	require.NoError(t, err)
	require.Contains(t, strings.Split(string(args), "\n"), "[")
}
