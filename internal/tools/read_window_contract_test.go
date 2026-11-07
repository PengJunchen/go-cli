package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadWindowExplicitRange(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fixture.go"), []byte("one\ntwo\nthree\nfour\n"), 0o600))
	tool := NewReadTool(WithWorkdir(dir))
	result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": "fixture.go", "offset": 2, "limit": 2}})
	require.NoError(t, err)
	require.Equal(t, "[read window; not file content; lines=2-3/4; omitted_before=1; omitted_after=1; eof=false; next_offset=4]\ntwo\nthree\n", result.Output)
	require.Equal(t, map[string]any{"path": filepath.Join(dir, "fixture.go"), "bytes": 10, "source_bytes": 19, "windowed": true, "offset": 2, "limit": 2, "start_line": 2, "end_line": 3, "total_lines": 4, "returned_lines": 2, "omitted_before": 1, "omitted_after": 1, "has_more": true, "next_offset": 4, "truncated": true}, result.Metadata)
}

func TestReadWindowSchemaAdvertisesRanges(t *testing.T) {
	schema := NewReadTool().Parameters().(map[string]any)
	properties := schema["properties"].(map[string]any)
	require.Contains(t, properties, "offset")
	require.Contains(t, properties, "limit")
	for _, name := range []string{"offset", "limit"} {
		item := properties[name].(map[string]any)
		require.Equal(t, "integer", item["type"])
		require.Equal(t, 1, item["minimum"])
		require.Equal(t, 2147483647, item["maximum"])
	}
}

func TestReadWindowProjectionBoundaries(t *testing.T) {
	tests := []struct {
		name, source, excerpt                  string
		args                                   map[string]any
		start, end, total, before, after, next int
	}{
		{"first", "one\ntwo\nthree\n", "one\ntwo\n", map[string]any{"limit": 2}, 1, 2, 3, 0, 1, 3},
		{"middle", "one\ntwo\nthree\n", "two\n", map[string]any{"offset": 2, "limit": 1}, 2, 2, 3, 1, 1, 3},
		{"offset only", "one\ntwo\nthree", "two\nthree", map[string]any{"offset": 2}, 2, 3, 3, 1, 0, 0},
		{"full window", "one\ntwo\n", "one\ntwo\n", map[string]any{"offset": 1, "limit": 99}, 1, 2, 2, 0, 0, 0},
		{"EOF", "one\ntwo\n", "", map[string]any{"offset": 3}, 0, 0, 2, 2, 0, 0},
		{"beyond EOF max", "one\n", "", map[string]any{"offset": 2147483647, "limit": 2147483647}, 0, 0, 1, 1, 0, 0},
		{"empty", "", "", map[string]any{"offset": 1}, 0, 0, 0, 0, 0, 0},
		{"single newline", "\n", "\n", map[string]any{"offset": 1}, 1, 1, 1, 0, 0, 0},
		{"no phantom line", "a\n\n", "\n", map[string]any{"offset": 2, "limit": 1}, 2, 2, 2, 1, 0, 0},
		{"CRLF Unicode no tail", "α\r\nβ\r\n末尾", "β\r\n末尾", map[string]any{"offset": 2}, 2, 3, 3, 1, 0, 0},
		{"lone CR", "a\rb\nc", "a\rb\n", map[string]any{"limit": 1}, 1, 1, 2, 0, 1, 2},
		{"long source line", strings.Repeat("z", 70*1024) + "\ntail", strings.Repeat("z", 70*1024) + "\n", map[string]any{"limit": 1}, 1, 1, 2, 0, 1, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "input.txt")
			require.NoError(t, os.WriteFile(path, []byte(tt.source), 0o600))
			args := map[string]any{"path": path}
			for key, value := range tt.args {
				args[key] = value
			}
			got, err := NewReadTool().Execute(context.Background(), ToolCall{Args: args})
			require.NoError(t, err)
			notice, excerpt, ok := strings.Cut(got.Output, "\n")
			require.True(t, ok, "window notice must precede the verbatim excerpt")
			require.Contains(t, notice, "not file content")
			require.Equal(t, tt.excerpt, excerpt)
			require.Equal(t, len(tt.excerpt), got.Metadata["bytes"])
			require.Equal(t, len(tt.source), got.Metadata["source_bytes"])
			require.Equal(t, tt.start, got.Metadata["start_line"])
			require.Equal(t, tt.end, got.Metadata["end_line"])
			require.Equal(t, tt.total, got.Metadata["total_lines"])
			require.Equal(t, tt.before, got.Metadata["omitted_before"])
			require.Equal(t, tt.after, got.Metadata["omitted_after"])
			require.Equal(t, tt.after > 0, got.Metadata["has_more"])
			require.Equal(t, tt.before > 0 || tt.after > 0, got.Metadata["truncated"])
			if tt.next > 0 {
				require.Equal(t, tt.next, got.Metadata["next_offset"])
			} else {
				require.NotContains(t, got.Metadata, "next_offset")
			}
			if tt.start == 0 {
				require.Contains(t, notice, "empty excerpt, EOF")
			}
		})
	}
}

func TestReadWindowIntegerValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\ntwo\n"), 0o600))
	tool := NewReadTool()
	for _, value := range []any{1, int8(1), int64(1), uint64(1), float64(1), json.Number("1.0"), json.Number("1e0")} {
		t.Run(fmt.Sprintf("valid_%T_%v", value, value), func(t *testing.T) {
			result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": path, "limit": value}})
			require.NoError(t, err)
			_, excerpt, ok := strings.Cut(result.Output, "\n")
			require.True(t, ok)
			require.Equal(t, "one\n", excerpt)
		})
	}
	full, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": path, "limit": json.Number("1e3")}})
	require.NoError(t, err)
	_, excerpt, ok := strings.Cut(full.Output, "\n")
	require.True(t, ok)
	require.Equal(t, "one\ntwo\n", excerpt)
	for _, name := range []string{"offset", "limit"} {
		for _, value := range []any{nil, true, "1", 0, -1, 1.5, json.Number("1.5"), int64(2147483648), uint64(math.MaxUint64), math.NaN(), math.Inf(1)} {
			t.Run(fmt.Sprintf("invalid_%s_%T_%v", name, value, value), func(t *testing.T) {
				result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": path, name: value}})
				require.Error(t, err)
				require.Nil(t, result)
			})
		}
	}
}

func TestReadWindowPreservesLegacyAndWholeFileCap(t *testing.T) {
	dir := t.TempDir()
	tool := NewReadTool(WithWorkdir(dir))
	for name, content := range map[string][]byte{"text": []byte("one\r\ntwo"), "empty": {}, "invalid": {'a', 0xff, '\n'}} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, content, 0o600))
		result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": name}})
		require.NoError(t, err)
		require.Equal(t, string(content), result.Output)
		require.Equal(t, map[string]any{"path": path, "bytes": len(content)}, result.Metadata)
	}
	result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": "invalid", "limit": 1}})
	require.ErrorContains(t, err, "valid UTF-8")
	require.Nil(t, result)
	atCap := strings.Repeat("a", defaultReadMaxBytes)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cap"), []byte(atCap), 0o600))
	result, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": "cap"}})
	require.NoError(t, err)
	require.Equal(t, atCap, result.Output)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "cap"), []byte(atCap+"b"), 0o600))
	for _, args := range []map[string]any{{"path": "cap"}, {"path": "cap", "limit": 1}} {
		result, err = tool.Execute(context.Background(), ToolCall{Args: args})
		require.ErrorContains(t, err, "too large")
		require.Nil(t, result)
	}
	tiny := NewReadTool(WithWorkdir(dir), WithMaxBytes(3))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "small"), []byte("a\nb\n"), 0o600))
	result, err = tiny.Execute(context.Background(), ToolCall{Args: map[string]any{"path": "small", "limit": 1}})
	require.ErrorContains(t, err, "too large")
	require.Nil(t, result)
}

func TestReadWindowDirectoryScopeAndCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "inside")
	require.NoError(t, os.WriteFile(path, []byte("one\ntwo\n"), 0o600))
	tool := NewReadTool(WithWorkdir(root), WithReadPathWhitelist([]string{root}))
	listed, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": "."}})
	require.NoError(t, err)
	require.Equal(t, "inside", listed.Output)
	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": ".", "limit": 1}})
	require.ErrorContains(t, err, "directories")
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": outside, "offset": 1}})
	require.Error(t, err)
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "link")))
	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"path": "link", "limit": 1}})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := tool.Execute(ctx, ToolCall{Args: map[string]any{"path": "inside", "limit": 1}})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	_, err = projectReadWindow(ctx, path, []byte("one\n"), readWindow{enabled: true, offset: 1})
	require.ErrorIs(t, err, context.Canceled)
}
