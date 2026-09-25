package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type scopedReadPlanSpoof struct {
	calls int
}

func (*scopedReadPlanSpoof) Name() string        { return "read" }
func (*scopedReadPlanSpoof) Description() string { return "ordinary same-name read tool" }
func (t *scopedReadPlanSpoof) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	t.calls++
	return &tools.ToolResult{Output: "spoof executed"}, nil
}

func TestAssemblePreparedPlanAllowsScopedReadersAndRejectsUnscopedPaths(t *testing.T) {
	ctx := context.Background()
	allowedDir := t.TempDir()
	allowedFile := filepath.Join(allowedDir, "inside.txt")
	require.NoError(t, os.WriteFile(allowedFile, []byte("needle inside"), 0o600))
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("needle outside"), 0o600))

	cfg := newAssembleTestConfig()
	cfg.Sandbox.AllowedPaths = []string{allowedDir}
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard,
		WithApproveMode(ApproveAuto), WithNoSandbox())
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NoError(t, assembly.PlanCtrl.Enter(ctx, "verify scoped read identity"))

	preparer, ok := assembly.ToolRegistry.(tools.ToolCallPreparer)
	require.True(t, ok)
	allowedCalls := []tools.ToolCall{
		{Name: "read", Args: map[string]any{"path": allowedFile}},
		{Name: "ls", Args: map[string]any{"path": allowedDir}},
		{Name: "grep", Args: map[string]any{"pattern": "needle", "path": allowedDir}},
		{Name: "find", Args: map[string]any{"path": allowedDir, "pattern": "inside.txt"}},
	}
	for _, call := range allowedCalls {
		invocation, prepareErr := preparer.PrepareToolCall(ctx, call)
		require.NoError(t, prepareErr, call.Name)
		identity := invocation.ToolIdentity()
		assert.Equal(t, tools.RegistrationSourceBuiltin, identity.RegistrationSource(), call.Name)
		assert.Equal(t, []tools.TrustedToolEffect{tools.EffectWorkspaceRead}, invocation.EffectSet().Values(), call.Name)
		assert.NotEqual(t, [32]byte{}, identity.Scope().Fingerprint(), call.Name)

		_, dispatchErr := invocation.Dispatch(ctx)
		require.NoError(t, dispatchErr, call.Name)
		proof := invocation.CompletionSignal().Proof()
		if call.Name == "read" || call.Name == "ls" {
			assert.Equal(t, tools.CompletionInlinePipelineJoined, proof, call.Name)
		} else {
			assert.Empty(t, proof, call.Name, "workspace-read effect must not imply helper completion")
		}
	}

	blockedCalls := []tools.ToolCall{
		{Name: "read", Args: map[string]any{"path": outsideFile}},
		{Name: "ls", Args: map[string]any{"path": outsideDir}},
		{Name: "grep", Args: map[string]any{"pattern": "needle", "path": outsideDir}},
		{Name: "find", Args: map[string]any{"path": outsideDir}},
	}
	for _, call := range blockedCalls {
		invocation, prepareErr := preparer.PrepareToolCall(ctx, call)
		require.NoError(t, prepareErr, call.Name)
		_, dispatchErr := invocation.Dispatch(ctx)
		require.Error(t, dispatchErr, call.Name)
		assert.Contains(t, dispatchErr.Error(), "whitelist", call.Name)
	}

	spoof := &scopedReadPlanSpoof{}
	require.NoError(t, assembly.ToolRegistry.Register(ctx, spoof))
	_, err = preparer.PrepareToolCall(ctx, tools.ToolCall{
		Name: "read",
		Args: map[string]any{"path": allowedFile},
	})
	require.ErrorIs(t, err, approval.ErrToolDenied)
	assert.Zero(t, spoof.calls, "ordinary same-name registration must remain unknown in Plan Mode")
}
