package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestGrepBudgetDescriptionReflectsEffectiveLimits(t *testing.T) {
	for _, tc := range []struct {
		tool    *GrepTool
		matches string
		bytes   string
	}{
		{NewGrepTool(), "100 matches", "51200 bytes"},
		{NewGrepTool(WithGrepMaxMatches(150), WithGrepMaxOutput(90000)), "150 matches", "90000 bytes"},
		{NewGrepTool(WithGrepMaxMatches(-1), WithGrepMaxOutput(-1)), "0 matches", "0 bytes"},
	} {
		desc := tc.tool.Description()
		require.Contains(t, desc, tc.matches)
		require.Contains(t, desc, tc.bytes)
		for _, term := range []string{"path", "glob", "pattern", "read", "notice", "metadata"} {
			require.Contains(t, strings.ToLower(desc), term)
		}
		schema := tc.tool.Parameters().(map[string]any)
		props := schema["properties"].(map[string]any)
		require.Len(t, props, 3, "the model has no limit or pagination argument")
		for _, key := range []string{"pattern", "glob", "path"} {
			property := props[key].(map[string]any)
			require.NotEmpty(t, property["description"])
		}
		require.Equal(t, false, schema["additionalProperties"])
	}
}

func TestGrepBudgetMatchBoundaryAndOverride(t *testing.T) {
	for _, pureGo := range []bool{true, false} {
		name := "ripgrep"
		if pureGo {
			name = "pure-go"
		} else if _, err := exec.LookPath("rg"); err != nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				lines, limit, want int
				truncated          bool
			}{
				{100, 0, 100, false}, {101, 0, 100, true}, {130, 150, 130, false}, {151, 150, 150, true},
			} {
				t.Run(fmt.Sprintf("lines-%d-limit-%d", tc.lines, tc.limit), func(t *testing.T) {
					root := t.TempDir()
					file := filepath.Join(root, "matches.txt")
					require.NoError(t, os.WriteFile(file, []byte(strings.Repeat("needle\n", tc.lines)), 0o600))
					opts := []GrepToolOption{WithGrepWorkdir(root), WithForcePureGo(pureGo)}
					if tc.limit != 0 {
						opts = append(opts, WithGrepMaxMatches(tc.limit))
					}
					result, err := NewGrepTool(opts...).Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "needle", "path": file}})
					require.NoError(t, err)
					require.Equal(t, tc.want, result.Metadata["matches"])
					require.Equal(t, tc.truncated, result.Metadata["truncated"])
					require.Contains(t, result.Output, fmt.Sprintf(":%d:needle", tc.want))
					if tc.truncated {
						require.NotContains(t, result.Output, fmt.Sprintf(":%d:needle", tc.want+1))
						assertGrepBudgetNotice(t, result)
					} else {
						require.NotContains(t, result.Output, "[results truncated]")
					}
				})
			}
		})
	}
}

func TestGrepBudgetByteBoundaryUnicodeAndOverride(t *testing.T) {
	for _, pureGo := range []bool{true, false} {
		name := "ripgrep"
		if pureGo {
			name = "pure-go"
		} else if _, err := exec.LookPath("rg"); err != nil {
			continue
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			file := filepath.Join(root, "unicode.txt")
			available := 50*1024 - len(file) - len(":1:") - 1
			prefix := "needle"
			remaining := available - len(prefix)
			line := prefix + strings.Repeat("界", remaining/len("界")) + strings.Repeat("x", remaining%len("界"))
			require.NoError(t, os.WriteFile(file, []byte(line+"\n"), 0o600))
			tool := NewGrepTool(WithGrepWorkdir(root), WithForcePureGo(pureGo))
			call := ToolCall{Args: map[string]any{"pattern": "needle", "path": file}}
			exact, err := tool.Execute(context.Background(), call)
			require.NoError(t, err)
			require.Equal(t, file+":1:"+line, exact.Output)
			require.Equal(t, false, exact.Metadata["truncated"])
			require.True(t, utf8.ValidString(exact.Output))

			require.NoError(t, os.WriteFile(file, []byte(line+"x\n"), 0o600))
			over, err := tool.Execute(context.Background(), call)
			require.NoError(t, err)
			require.Equal(t, 0, over.Metadata["matches"])
			require.Equal(t, true, over.Metadata["truncated"])
			require.True(t, utf8.ValidString(over.Output))
			require.NotContains(t, over.Output, "needle", "an over-budget record is omitted whole")
			assertGrepBudgetNotice(t, over)
			payload, err := json.Marshal(over)
			require.NoError(t, err)
			var decoded struct {
				Output   string `json:"output"`
				Metadata struct {
					Truncated bool `json:"truncated"`
					Matches   int  `json:"matches"`
				} `json:"metadata"`
			}
			require.NoError(t, json.Unmarshal(payload, &decoded))
			require.Equal(t, over.Output, decoded.Output)
			require.True(t, decoded.Metadata.Truncated)
			require.Zero(t, decoded.Metadata.Matches)

			explicit := NewGrepTool(WithGrepWorkdir(root), WithForcePureGo(pureGo), WithGrepMaxOutput(100000))
			result, err := explicit.Execute(context.Background(), call)
			require.NoError(t, err)
			require.Equal(t, file+":1:"+line+"x", result.Output)
			require.Equal(t, false, result.Metadata["truncated"])
		})
	}
}

func TestGrepBudgetZeroAndNegativeExplicitLimits(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "file.txt"), []byte("needle\n"), 0o600))
	for _, opts := range [][]GrepToolOption{
		{WithGrepMaxMatches(0)}, {WithGrepMaxMatches(-1)}, {WithGrepMaxOutput(0)}, {WithGrepMaxOutput(-1)},
	} {
		tool := NewGrepTool(append([]GrepToolOption{WithGrepWorkdir(root), WithForcePureGo(true)}, opts...)...)
		result, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"pattern": "needle"}})
		require.NoError(t, err)
		require.Equal(t, 0, result.Metadata["matches"])
		require.Equal(t, true, result.Metadata["truncated"])
		assertGrepBudgetNotice(t, result)
	}
}

func assertGrepBudgetNotice(t *testing.T, result *ToolResult) {
	t.Helper()
	require.Contains(t, result.Output, "[results truncated]")
	for _, term := range []string{"path", "glob", "pattern", "read"} {
		require.Contains(t, strings.ToLower(result.Output), term)
	}
}
