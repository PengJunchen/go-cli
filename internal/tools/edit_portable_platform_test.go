//go:build windows || freebsd

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// These tests must also run on the target OS; cross-compilation alone does not
// establish filesystem or handle semantics.
func TestPortablePlatformEditRegularAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "edit.txt")
	require.NoError(t, os.WriteFile(path, []byte("old text"), 0o600))
	before, err := os.Stat(path)
	require.NoError(t, err)
	result, err := NewEditFileTool(WithEditPathWhitelist([]string{path})).Execute(context.Background(), ToolCall{
		Args: map[string]any{"file_path": path, "old_string": "old", "new_string": "new"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new text", string(content))
	after, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, before.Mode().Perm(), after.Mode().Perm())
	temps, err := filepath.Glob(filepath.Join(dir, ".write-*"))
	require.NoError(t, err)
	require.Empty(t, temps)
}

func TestPortablePlatformEditRejectsChangesAndCancellation(t *testing.T) {
	for _, scenario := range []string{"content", "identity", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "edit.txt")
			require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
			target, err := captureEditTarget(path, NewPathWhitelist([]string{dir}))
			require.NoError(t, err)
			defer func() { _ = target.close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "content":
				require.NoError(t, os.WriteFile(path, []byte("changed"), 0o600))
			case "identity":
				replacement := filepath.Join(dir, "replacement")
				require.NoError(t, os.WriteFile(replacement, []byte("old"), 0o600))
				require.NoError(t, os.Rename(replacement, path))
			case "canceled":
				cancel()
			}
			err = target.write(ctx, []byte("new"))
			if scenario == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, ErrEditConflict)
			}
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			if scenario == "content" {
				require.Equal(t, "changed", string(content))
			} else {
				require.Equal(t, "old", string(content))
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".write-*"))
			require.NoError(t, err)
			require.Empty(t, temps)
		})
	}
}

func TestPortablePlatformEditRejectsSymlinkAndScopeEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	external := filepath.Join(outside, "outside")
	require.NoError(t, os.WriteFile(external, []byte("outside"), 0o600))
	_, err := captureEditTarget(external, NewPathWhitelist([]string{root}))
	require.Error(t, err)
	link := filepath.Join(root, "link")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("platform does not permit creating a test symlink: %v", err)
	}
	_, err = captureEditTarget(link, NewPathWhitelist([]string{root}))
	require.Error(t, err)
	content, err := os.ReadFile(external)
	require.NoError(t, err)
	require.Equal(t, "outside", string(content))
}
