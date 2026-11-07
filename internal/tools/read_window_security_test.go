//go:build darwin || linux

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type windowPreviewRecorder struct {
	before, after string
	mutate        func() error
}

func (d *windowPreviewRecorder) Generate(_ context.Context, before, after, _ string) (string, error) {
	d.before, d.after = before, after
	if d.mutate != nil {
		if err := d.mutate(); err != nil {
			return "", err
		}
	}
	return "full preview", nil
}

func TestReadWindowPreparedProofBindsRangeAndFreezesArgs(t *testing.T) {
	ctx := context.Background()
	root := canonicalScopedReadTestRoot(t)
	path := filepath.Join(root, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o600))
	base, registry := newScopedReadProofRegistry(t, root)
	args := map[string]any{"path": path, "offset": json.Number("2.0"), "limit": json.Number("1e0")}
	call := ToolCall{ID: "window-proof", Name: "read", Args: args}
	invocation, err := registry.PrepareToolCall(ctx, call)
	require.NoError(t, err)
	require.Equal(t, RegistrationSourceBuiltin, invocation.ToolIdentity().RegistrationSource())
	require.Equal(t, []TrustedToolEffect{EffectWorkspaceRead}, invocation.EffectSet().Values())
	proof, ok := ScopedWorkspaceReadProofFor(invocation, root)
	require.True(t, ok)
	require.True(t, proof.MatchesAction(invocation.ToolIdentity(), invocation.EffectSet(), invocation.SchemaDigest(), invocation.ArgumentDigest(), call.ID, root))
	for _, changedArgs := range []map[string]any{
		{"path": path, "offset": json.Number("1.0"), "limit": json.Number("1e0")},
		{"path": path, "offset": json.Number("2.0"), "limit": json.Number("2e0")},
	} {
		changed, prepareErr := registry.PrepareToolCall(ctx, ToolCall{ID: call.ID, Name: call.Name, Args: changedArgs})
		require.NoError(t, prepareErr)
		require.NotEqual(t, invocation.ArgumentDigest(), changed.ArgumentDigest())
		require.False(t, proof.MatchesAction(changed.ToolIdentity(), changed.EffectSet(), changed.SchemaDigest(), changed.ArgumentDigest(), call.ID, root))
	}
	args["offset"] = 1
	args["limit"] = 3
	changed, err := registry.PrepareToolCall(ctx, ToolCall{ID: call.ID, Name: call.Name, Args: args})
	require.NoError(t, err)
	require.NotEqual(t, invocation.ArgumentDigest(), changed.ArgumentDigest())
	require.False(t, proof.MatchesAction(changed.ToolIdentity(), changed.EffectSet(), changed.SchemaDigest(), changed.ArgumentDigest(), call.ID, root))
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	_, excerpt, found := strings.Cut(result.Output, "\n")
	require.True(t, found)
	require.Equal(t, "two\n", excerpt, "execution must use the frozen range")
	require.Equal(t, true, result.Metadata["truncated"])
	for _, value := range []any{nil, 1.5, 0, int64(2147483648), "2"} {
		_, err := registry.PrepareToolCall(ctx, ToolCall{ID: "bad", Name: "read", Args: map[string]any{"path": path, "offset": value}})
		require.Error(t, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	_, err = registry.PrepareToolCall(ctx, ToolCall{ID: "outside", Name: "read", Args: map[string]any{"path": outside, "offset": 1}})
	require.Error(t, err)
	require.NoError(t, base.Register(ctx, &scopedReadImpostor{}))
	custom, err := registry.PrepareToolCall(ctx, ToolCall{ID: "custom", Name: "read", Args: map[string]any{"path": path}})
	require.NoError(t, err)
	require.Equal(t, RegistrationSourceToolRegistry, custom.ToolIdentity().RegistrationSource())
	_, ok = ScopedWorkspaceReadProofFor(custom, root)
	require.False(t, ok, "a same-name custom reader must not gain builtin scope authority")
}

func TestReadWindowEditUsesCompleteSourceAndPreview(t *testing.T) {
	ctx := context.Background()
	t.Run("unique match and full preview", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file.txt")
		original := "head\nold\ntail\n"
		require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
		window, err := NewReadTool().Execute(ctx, ToolCall{Args: map[string]any{"path": path, "offset": 2, "limit": 1}})
		require.NoError(t, err)
		_, excerpt, _ := strings.Cut(window.Output, "\n")
		require.Equal(t, "old\n", excerpt)
		preview := &windowPreviewRecorder{}
		result, err := NewEditFileTool(WithEditDiffGenerator(preview)).Execute(ctx, ToolCall{Args: map[string]any{"file_path": path, "old_string": excerpt, "new_string": "new\n"}})
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, original, preview.before)
		require.Equal(t, "head\nnew\ntail\n", preview.after)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, preview.after, string(data))
	})
	t.Run("duplicate outside window rejects", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file.txt")
		original := "head\nold\nbetween\nold\ntail\n"
		require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
		window, err := NewReadTool().Execute(ctx, ToolCall{Args: map[string]any{"path": path, "offset": 2, "limit": 1}})
		require.NoError(t, err)
		_, excerpt, _ := strings.Cut(window.Output, "\n")
		require.Equal(t, 1, strings.Count(excerpt, "old"))
		result, err := NewEditFileTool().Execute(ctx, ToolCall{Args: map[string]any{"file_path": path, "old_string": excerpt, "new_string": "new\n"}})
		require.ErrorContains(t, err, "matches 2 times")
		require.Nil(t, result)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, original, string(data))
	})
	t.Run("outside window mutation conflicts", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file.txt")
		original, external := "head\nold\ntail\n", "head\nold\nexternal\n"
		require.NoError(t, os.WriteFile(path, []byte(original), 0o600))
		window, err := NewReadTool().Execute(ctx, ToolCall{Args: map[string]any{"path": path, "offset": 2, "limit": 1}})
		require.NoError(t, err)
		_, excerpt, _ := strings.Cut(window.Output, "\n")
		preview := &windowPreviewRecorder{mutate: func() error { return os.WriteFile(path, []byte(external), 0o600) }}
		result, err := NewEditFileTool(WithEditDiffGenerator(preview)).Execute(ctx, ToolCall{Args: map[string]any{"file_path": path, "old_string": excerpt, "new_string": "new\n"}})
		require.ErrorIs(t, err, ErrEditConflict)
		require.Nil(t, result)
		require.Equal(t, original, preview.before)
		require.Equal(t, "head\nnew\ntail\n", preview.after)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, external, string(data))
	})
}
