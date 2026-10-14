//go:build unix

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestFindTypeFMatchesOnlyRegularFilesAcrossBackends(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	regularPath := filepath.Join(dir, "regular.txt")
	require.NoError(t, os.WriteFile(regularPath, []byte("regular"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "childdir"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "childdir", "nested.txt"), nil, 0o600))
	require.NoError(t, os.Symlink(regularPath, filepath.Join(dir, "childfile-link.txt")))
	require.NoError(t, os.Symlink(filepath.Join(dir, "childdir"), filepath.Join(dir, "childdir-link")))
	require.NoError(t, os.Symlink(filepath.Join(dir, "missing-target"), filepath.Join(dir, "dangling-link")))

	emptyNames := []string{"childdir", "childdir-link", "childfile-link.txt", "dangling-link", "nested.txt", "regular.txt"}
	fifoPath := filepath.Join(dir, "special-pipe")
	if err := syscall.Mkfifo(fifoPath, 0o600); err == nil {
		emptyNames = append(emptyNames, "special-pipe")
	} else {
		t.Logf("FIFO fixture unavailable: %v", err)
	}

	findPath, err := exec.LookPath("find")
	require.NoError(t, err)
	helperDir := t.TempDir()
	require.NoError(t, os.Symlink(findPath, filepath.Join(helperDir, "find")))
	t.Setenv("PATH", helperDir)

	for _, tc := range []struct {
		name string
		kind string
		want []string
	}{
		{name: "regular files only", kind: "f", want: []string{"nested.txt", "regular.txt"}},
		{name: "directories only", kind: "d", want: []string{"childdir"}},
		{name: "empty type includes all entries", kind: "", want: emptyNames},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := ToolCall{Args: map[string]any{"path": ".", "type": tc.kind}}
			goResult, err := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true)).Execute(context.Background(), call)
			require.NoError(t, err)
			systemResult, err := NewFindTool(WithFindWorkdir(dir)).Execute(context.Background(), call)
			require.NoError(t, err)

			assert.Equal(t, tc.want, sortedFindOutputBasenames(goResult.Output))
			assert.Equal(t, tc.want, sortedFindOutputBasenames(systemResult.Output))
			assert.Equal(t, len(tc.want), goResult.Metadata["matches"])
			assert.Equal(t, len(tc.want), systemResult.Metadata["matches"])
			assert.False(t, goResult.Metadata["truncated"].(bool))
			assert.False(t, systemResult.Metadata["truncated"].(bool))
		})
	}
}

func TestFindKindFilterPreservesFileRootAndRegularFileTopK(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	rootFile := filepath.Join(dir, "root.txt")
	require.NoError(t, os.WriteFile(rootFile, []byte("root"), 0o600))

	for _, tc := range []struct {
		name    string
		kind    string
		wantOne bool
	}{
		{name: "type f", kind: "f", wantOne: true},
		{name: "empty type", kind: "", wantOne: true},
		{name: "type d", kind: "d"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true)).Execute(context.Background(), ToolCall{
				Args: map[string]any{"path": rootFile, "type": tc.kind},
			})
			require.NoError(t, err)
			if tc.wantOne {
				assert.Equal(t, rootFile, result.Output)
				assert.Equal(t, 1, result.Metadata["matches"])
			} else {
				assert.Empty(t, result.Output)
				assert.Equal(t, 0, result.Metadata["matches"])
			}
			assert.False(t, result.Metadata["truncated"].(bool))
		})
	}

	for i := findMaxResults + 4; i >= 0; i-- {
		name := filepath.Join(dir, fmt.Sprintf("entry-%04d.txt", i))
		require.NoError(t, os.WriteFile(name, nil, 0o600))
	}
	result, err := NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true)).Execute(context.Background(), ToolCall{
		Args: map[string]any{"path": ".", "type": "f"},
	})
	require.NoError(t, err)
	want := make([]string, findMaxResults)
	for i := range want {
		want[i] = fmt.Sprintf("entry-%04d.txt", i)
	}
	assert.Equal(t, want, sortedFindOutputBasenames(result.Output))
	assert.Equal(t, findMaxResults, result.Metadata["matches"])
	assert.True(t, result.Metadata["truncated"].(bool))
}
