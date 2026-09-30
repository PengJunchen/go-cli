package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestAssembleAgentConfiguredPathsConstrainDefaultFileTools(t *testing.T) {
	allowedDir, err := os.MkdirTemp(".", ".r4-path-scope-")
	require.NoError(t, err)
	t.Cleanup(func() {
		if removeErr := os.RemoveAll(allowedDir); removeErr != nil {
			t.Errorf("remove test scope: %v", removeErr)
		}
	})
	allowedFile := filepath.Join(allowedDir, "inside.txt")
	require.NoError(t, os.WriteFile(allowedFile, []byte("needle inside"), 0o600))

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("needle outside"), 0o600))
	outsideNestedDir := filepath.Join(outsideDir, "nested")
	require.NoError(t, os.Mkdir(outsideNestedDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(outsideNestedDir, "nested.txt"), []byte("needle nested"), 0o600))
	linkDir := filepath.Join(allowedDir, "escape")
	require.NoError(t, os.Symlink(outsideDir, linkDir))

	cfg := newAssembleTestConfig(t)
	cfg.Sandbox.AllowedPaths = []string{allowedDir}

	assembly, err := AssembleAgent(
		context.Background(), cfg, "openai", "test-model", io.Discard,
		WithApproveMode(ApproveAuto),
		WithNoSandbox(),
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	ctx := context.Background()
	allowedCalls := []tools.ToolCall{
		{Name: "read", Args: map[string]any{"path": allowedFile}},
		{Name: "grep", Args: map[string]any{"pattern": "needle", "path": allowedDir}},
		{Name: "find", Args: map[string]any{"path": allowedDir, "pattern": "inside.txt"}},
		{Name: "ls", Args: map[string]any{"path": allowedDir}},
	}
	for _, call := range allowedCalls {
		def, getErr := assembly.ToolRegistry.Get(ctx, call.Name)
		require.NoError(t, getErr, call.Name)
		_, execErr := def.Execute(ctx, call)
		require.NoError(t, execErr, call.Name)
	}

	blockedCalls := []tools.ToolCall{
		{Name: "read", Args: map[string]any{"path": outsideFile}},
		{Name: "grep", Args: map[string]any{"pattern": "needle", "path": outsideDir}},
		{Name: "find", Args: map[string]any{"path": outsideDir}},
		{Name: "ls", Args: map[string]any{"path": outsideDir}},
	}
	for _, call := range blockedCalls {
		def, getErr := assembly.ToolRegistry.Get(ctx, call.Name)
		require.NoError(t, getErr, call.Name)
		_, execErr := def.Execute(ctx, call)
		require.Error(t, execErr, call.Name+" must honor config Sandbox.AllowedPaths even with WithNoSandbox")
	}

	symlinkCalls := []tools.ToolCall{
		{Name: "read", Args: map[string]any{"path": filepath.Join(linkDir, "outside.txt")}},
		{Name: "grep", Args: map[string]any{"pattern": "needle", "path": filepath.Join(linkDir, "outside.txt")}},
		{Name: "find", Args: map[string]any{"path": filepath.Join(linkDir, "nested")}},
		{Name: "ls", Args: map[string]any{"path": filepath.Join(linkDir, "nested")}},
	}
	for _, call := range symlinkCalls {
		def, getErr := assembly.ToolRegistry.Get(ctx, call.Name)
		require.NoError(t, getErr, call.Name)
		_, execErr := def.Execute(ctx, call)
		require.Error(t, execErr, call.Name+" must reject a parent symlink that escapes config Sandbox.AllowedPaths")
	}
}

func TestAssemblePreparedWorkspaceReadsValidatePathsBeforePolicy(t *testing.T) {
	allowedDir := t.TempDir()
	allowedFile := filepath.Join(allowedDir, "inside.txt")
	require.NoError(t, os.WriteFile(allowedFile, []byte("needle inside"), 0o600))

	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("needle outside"), 0o600))
	outsideNestedDir := filepath.Join(outsideDir, "nested")
	require.NoError(t, os.Mkdir(outsideNestedDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(outsideNestedDir, "nested.txt"), []byte("needle nested"), 0o600))
	linkDir := filepath.Join(allowedDir, "parent-escape")
	require.NoError(t, os.Symlink(outsideDir, linkDir))

	cfg := newAssembleTestConfig(t)
	cfg.Sandbox.AllowedPaths = []string{allowedDir}
	assembly, err := AssembleAgent(
		context.Background(), cfg, "openai", "test-model", io.Discard,
		WithApproveMode(ApproveAuto),
		WithNoSandbox(),
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok)
	policySetter, ok := assembly.ToolRegistry.(interface {
		SetPreparedToolPolicy(tools.PreparedToolPolicy) error
	})
	require.True(t, ok)
	policyCalls := make([]tools.FrozenToolCall, 0, 4)
	require.NoError(t, policySetter.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		policyCalls = append(policyCalls, request.Call)
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))

	allowedCalls := []tools.ToolCall{
		{Name: "read", Args: map[string]any{"path": allowedFile}},
		{Name: "grep", Args: map[string]any{"pattern": "needle", "path": allowedDir}},
		{Name: "find", Args: map[string]any{"path": allowedDir, "pattern": "inside.txt"}},
		{Name: "ls", Args: map[string]any{"path": allowedDir}},
	}
	for index, call := range allowedCalls {
		allowedPath := call.Args["path"].(string)
		invocation, prepareErr := preparer.PrepareToolCall(context.Background(), call)
		require.NoError(t, prepareErr, call.Name)
		require.Equal(t, []tools.TrustedToolEffect{tools.EffectWorkspaceRead}, invocation.EffectSet().Values(), call.Name)
		require.Equal(t, allowedPath, invocation.NormalizedCall().Args["path"], call.Name)
		require.Equal(t, allowedPath, policyCalls[index].ToolCall().Args["path"], call.Name)
		if call.Name == "read" {
			call.Args["path"] = outsideFile
		} else {
			call.Args["path"] = outsideDir
		}
		result, dispatchErr := invocation.Dispatch(context.Background())
		require.NoError(t, dispatchErr, call.Name)
		require.NotNil(t, result, call.Name)
		if call.Name == "read" {
			require.Equal(t, "needle inside", result.Output)
		} else {
			require.Contains(t, result.Output, "inside.txt", call.Name)
		}
	}
	classifiedAllowedCalls := len(policyCalls)
	require.Equal(t, len(allowedCalls), classifiedAllowedCalls)

	blockedCalls := []tools.ToolCall{
		{Name: "read", Args: map[string]any{"path": outsideFile}},
		{Name: "grep", Args: map[string]any{"pattern": "needle", "path": outsideDir}},
		{Name: "find", Args: map[string]any{"path": outsideDir}},
		{Name: "ls", Args: map[string]any{"path": outsideDir}},
		{Name: "read", Args: map[string]any{"path": filepath.Join(linkDir, "outside.txt")}},
		{Name: "grep", Args: map[string]any{"pattern": "needle", "path": filepath.Join(linkDir, "outside.txt")}},
		{Name: "find", Args: map[string]any{"path": filepath.Join(linkDir, "nested")}},
		{Name: "ls", Args: map[string]any{"path": filepath.Join(linkDir, "nested")}},
	}
	for _, call := range blockedCalls {
		invocation, prepareErr := preparer.PrepareToolCall(context.Background(), call)
		require.Error(t, prepareErr, call.Name)
		require.Nil(t, invocation, call.Name)
		require.Contains(t, prepareErr.Error(), "whitelist", call.Name)
		require.Equal(t, classifiedAllowedCalls, len(policyCalls), "escaped path must be rejected before policy can classify workspace_read: %s", call.Name)
	}
}

func TestAssembleAgentEmptyAllowedPathsLeavesReadUnrestricted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outside.txt")
	require.NoError(t, os.WriteFile(path, []byte("unrestricted"), 0o600))

	assembly, err := AssembleAgent(
		context.Background(), newAssembleTestConfig(t), "openai", "test-model", io.Discard,
		WithApproveMode(ApproveAuto),
	)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	def, err := assembly.ToolRegistry.Get(context.Background(), "read")
	require.NoError(t, err)
	result, err := def.Execute(context.Background(), tools.ToolCall{
		Name: "read", Args: map[string]any{"path": path},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "unrestricted", result.Output)
}
