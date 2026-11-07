//go:build darwin || linux

package tools

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

type searchOnlyDirectoryPreview struct {
	calls int
}

func (p *searchOnlyDirectoryPreview) Generate(_ context.Context, _, _, _ string) (string, error) {
	p.calls++
	return "preview", nil
}

func TestEditThroughSearchOnlyAncestor(t *testing.T) {
	for _, withPreview := range []bool{false, true} {
		name := "without_preview"
		if withPreview {
			name = "with_preview"
		}
		t.Run(name, func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root may bypass directory permissions")
			}
			dir := t.TempDir()
			ancestor := filepath.Join(dir, "ancestor")
			child := filepath.Join(ancestor, "child")
			path := filepath.Join(child, "file")
			require.NoError(t, os.MkdirAll(child, 0700))
			require.NoError(t, os.WriteFile(path, []byte("old content"), 0640))
			require.NoError(t, os.Chmod(path, 0640))
			t.Cleanup(func() {
				if err := os.Chmod(ancestor, 0700); err != nil {
					t.Errorf("restore ancestor permissions: %v", err)
				}
			})
			require.NoError(t, os.Chmod(ancestor, 0100))

			ancestorFile, openErr := os.Open(ancestor)
			if openErr == nil {
				require.NoError(t, ancestorFile.Close())
				t.Skip("read-only directory open was permitted despite mode 0100")
			}
			require.ErrorIs(t, openErr, syscall.EACCES)
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "old content", string(content))
			_, err = resolveAndValidatePath("edit", "", path, NewPathWhitelist([]string{dir}))
			require.NoError(t, err)
			t.Log("preconditions met: ancestor read denied, target readable and legacy path validation allowed")

			preview := &searchOnlyDirectoryPreview{}
			opts := []EditFileToolOption{WithEditPathWhitelist([]string{dir})}
			if withPreview {
				opts = append(opts, WithEditDiffGenerator(preview))
			}
			result, err := NewEditFileTool(opts...).Execute(context.Background(), ToolCall{Args: map[string]any{
				"file_path": path, "old_string": "old", "new_string": "new",
			}})
			require.NoError(t, err)
			require.NotNil(t, result)
			if withPreview {
				require.Equal(t, 1, preview.calls)
				require.Equal(t, "preview", result.Metadata["diff"])
			} else {
				require.NotContains(t, result.Metadata, "diff")
			}
			content, err = os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "new content", string(content))
			info, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0640), info.Mode().Perm())
		})
	}
}
