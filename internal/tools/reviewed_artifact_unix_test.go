//go:build darwin || linux

package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReviewedArtifactForcedPipeCloseRetainsUnknownOwner(t *testing.T) {
	root := t.TempDir()
	pidPath := filepath.Join(t.TempDir(), "escaped.pid")
	writeFailingHelperWithEscapedPipeWriter(t, root, "custom", pidPath)
	tool := NewCustomCommandTool("captured", "", []string{filepath.Join(root, "custom")}, nil, map[string]string{"SEARCH_HELPER_ESCAPED_PIPE_PID": pidPath}, 0, root)
	_, err := tool.Execute(context.Background(), ToolCall{})
	require.Error(t, err)
	require.ErrorIs(t, err, errProcessContainmentUnconfirmed)
	require.Error(t, tool.Close(context.Background()), "local reader closure must not delete a captured artifact still referenced by an unproved owner")
	killProcessFromFile(t, pidPath)
	assertProcessFromFileStopped(t, pidPath)
}
