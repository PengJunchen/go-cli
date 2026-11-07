package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestEditMatchFailureProvidesRecovery(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	const original = "first:\tprivate_value\nsecond:\tprivate_value\n"
	const replacement = "replacement_private_value"
	tests := []struct {
		name       string
		old        string
		kind       string
		prefix     func(string) string
		uniqueness bool
	}{
		{
			name:   "no match",
			old:    "private_missing_value",
			kind:   "no_match",
			prefix: func(path string) string { return "edit: old_string not found in " + path },
		},
		{
			name: "multiple matches",
			old:  "private_value",
			kind: "multiple_match",
			prefix: func(path string) string {
				return fmt.Sprintf("edit: old_string matches 2 times in %s, expected exactly once", path)
			},
			uniqueness: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "sample.txt")
			require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
			tool := NewEditFileTool()
			ctx := context.Background()

			result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{
				"file_path": path, "old_string": tc.old, "new_string": replacement,
			}})
			require.Error(t, err)
			assert.Nil(t, result)
			var matchFailure interface {
				error
				MatchFailureKind() string
			}
			require.ErrorAs(t, err, &matchFailure)
			assert.Equal(t, tc.kind, matchFailure.MatchFailureKind())
			assert.True(t, strings.HasPrefix(err.Error(), tc.prefix(path)))
			for _, phrase := range []string{"file was not changed", "read", "latest", "exact", "contiguous", "whitespace", "newlines", "same arguments"} {
				assert.Contains(t, strings.ToLower(err.Error()), phrase)
			}
			if tc.uniqueness {
				assert.Contains(t, strings.ToLower(err.Error()), "unique")
				assert.Contains(t, strings.ToLower(err.Error()), "context")
			}
			assert.NotContains(t, err.Error(), original)
			assert.NotContains(t, err.Error(), tc.old)
			assert.NotContains(t, err.Error(), replacement)
			unchanged, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, original, string(unchanged))
			info, statErr := os.Stat(path)
			require.NoError(t, statErr)
			assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

			readResult, readErr := NewReadTool().Execute(ctx, ToolCall{Args: map[string]any{"path": path}})
			require.NoError(t, readErr)
			const exactContext = "first:\tprivate_value\n"
			require.Contains(t, readResult.Output, exactContext)
			_, err = tool.Execute(ctx, ToolCall{Args: map[string]any{
				"file_path": path, "old_string": exactContext, "new_string": replacement + "\n",
			}})
			require.NoError(t, err)
			updated, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, replacement+"\nsecond:\tprivate_value\n", string(updated))
		})
	}
}
