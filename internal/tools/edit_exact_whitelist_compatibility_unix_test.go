//go:build darwin || linux

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

type exactWhitelistPreview struct {
	calls int
}

func (p *exactWhitelistPreview) Generate(_ context.Context, oldContent, newContent, _ string) (string, error) {
	p.calls++
	return oldContent + " -> " + newContent, nil
}

func TestEditExactFileWhitelistCompatibility(t *testing.T) {
	for _, withPreview := range []bool{false, true} {
		name := "without_preview"
		if withPreview {
			name = "with_preview"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "allowed.txt")
			require.NoError(t, os.WriteFile(path, []byte("old content"), 0600))

			whitelist := NewPathWhitelist([]string{path})
			_, err := resolveAndValidatePath("edit", "", path, whitelist)
			require.NoError(t, err, "the pre-existing path authorization accepts the exact file")

			preview := &exactWhitelistPreview{}
			opts := []EditFileToolOption{WithEditPathWhitelist([]string{path})}
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
				require.Equal(t, "old content -> new content", result.Metadata["diff"])
			} else {
				require.NotContains(t, result.Metadata, "diff")
			}
			content, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, "new content", string(content))
		})
	}

	t.Run("sibling_rejected", func(t *testing.T) {
		dir := t.TempDir()
		allowed := filepath.Join(dir, "allowed.txt")
		sibling := filepath.Join(dir, "allowed.txt.extra")
		require.NoError(t, os.WriteFile(allowed, []byte("old allowed"), 0600))
		require.NoError(t, os.WriteFile(sibling, []byte("old sibling"), 0600))
		result, err := NewEditFileTool(WithEditPathWhitelist([]string{allowed})).Execute(context.Background(), ToolCall{Args: map[string]any{
			"file_path": sibling, "old_string": "old", "new_string": "new",
		}})
		require.ErrorContains(t, err, "whitelist")
		require.Nil(t, result)
		target, err := captureEditTarget(sibling, NewPathWhitelist([]string{allowed}))
		if target != nil {
			defer func() { _ = target.close() }()
		}
		require.ErrorContains(t, err, "whitelist")
		content, err := os.ReadFile(sibling)
		require.NoError(t, err)
		require.Equal(t, "old sibling", string(content))
	})
}

func TestEditWhitelistDirectoryAndMultipleRoots(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other.txt")
	path := filepath.Join(dir, "allowed.txt")
	require.NoError(t, os.WriteFile(other, []byte("other content"), 0600))
	require.NoError(t, os.WriteFile(path, []byte("old content"), 0600))
	tool := NewEditFileTool(WithEditPathWhitelist([]string{other, dir}))
	result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{
		"file_path": path, "old_string": "old", "new_string": "new",
	}})
	require.NoError(t, err)
	require.NotNil(t, result)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new content", string(content))
	content, err = os.ReadFile(other)
	require.NoError(t, err)
	require.Equal(t, "other content", string(content))
}
