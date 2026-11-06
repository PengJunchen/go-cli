package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const expectedDirectoryTruncationNotice = "[listing truncated; not an entry]\nNarrow the path or filter to see omitted entries.\n"

func TestDirectoryTruncationFeedbackExactEntryBoundary(t *testing.T) {
	dir := t.TempDir()
	names := make([]string, directoryCaptureMaxEntries)
	for i := range names {
		names[i] = fmt.Sprintf("entry-%04d", i)
		require.NoError(t, os.WriteFile(filepath.Join(dir, names[i]), nil, 0o600))
	}
	check := func(truncated bool) {
		for _, tc := range []struct {
			name     string
			execute  func(context.Context, ToolCall) (*ToolResult, error)
			countKey string
		}{
			{"read", NewReadTool(WithWorkdir(dir)).Execute, "entries"},
			{"ls", NewLSTool().Execute, "entries"},
			{"find", NewFindTool(WithFindWorkdir(dir), WithFindForceNode(true)).Execute, "matches"},
		} {
			t.Run(fmt.Sprintf("%s/truncated=%t", tc.name, truncated), func(t *testing.T) {
				result, err := tc.execute(context.Background(), ToolCall{Args: map[string]any{"path": dir, "type": "f"}})
				require.NoError(t, err)
				require.False(t, result.IsError)
				want := strings.Join(names, "\n")
				if truncated {
					want = expectedDirectoryTruncationNotice + want
				}
				require.Equal(t, want, result.Output)
				require.Equal(t, len(names), result.Metadata[tc.countKey])
				require.Equal(t, truncated, result.Metadata["truncated"])
				require.LessOrEqual(t, len(result.Output), directoryCaptureMaxOutputBytes)
			})
		}
	}
	check(false)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "zz-omitted"), nil, 0o600))
	check(true)
}

func TestDirectoryTruncationFeedbackByteCapRetainsWholeEscapedRecords(t *testing.T) {
	output := newBoundedDirectoryOutput(directoryCaptureMaxOutputBytes)
	escaped := directoryDisplayName("entry\nwith\tcontrol")
	require.True(t, output.AppendLine(escaped))
	for i := 0; i < 998; i++ {
		require.True(t, output.AppendLine(strings.Repeat("x", 1047)))
	}
	last := strings.Repeat("z", directoryCaptureMaxOutputBytes-output.builder.Len()-1)
	require.True(t, output.AppendLine(last))
	before := output.String()
	require.Len(t, before, directoryCaptureMaxOutputBytes-1)
	text, records := renderDirectoryOutput(output, true)
	require.True(t, strings.HasPrefix(text, expectedDirectoryTruncationNotice+escaped+"\n"))
	require.LessOrEqual(t, len(text), directoryCaptureMaxOutputBytes)
	require.Equal(t, 999, records)
	require.Equal(t, strings.TrimSuffix(before, "\n"+last), strings.TrimPrefix(text, expectedDirectoryTruncationNotice))
	again, againRecords := renderDirectoryOutput(output, true)
	require.Equal(t, text, again)
	require.Equal(t, records, againRecords)
	untruncated, allRecords := renderDirectoryOutput(output, false)
	require.Equal(t, before, untruncated)
	require.Equal(t, 1000, allRecords)
}
