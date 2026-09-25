package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestAssembleRejectsConfiguredPostToolUseShellHook(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	hookDir := filepath.Join(workDir, ".go-cli")
	require.NoError(t, os.MkdirAll(hookDir, 0o700))
	markerPath := filepath.Join(workDir, "post-tool-use-hook-ran")
	hooks := fmt.Sprintf("hooks:\n  - event: post_tool_use\n    command: touch %q\n    timeout: 1s\n", markerPath)
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "hooks.yaml"), []byte(hooks), 0o600))

	cfg := newAssembleTestConfig()
	cfg.Git.WorkDir = workDir
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard)
	require.Nil(t, assembly)
	require.ErrorIs(t, err, ErrPreparedPostToolUseHookUnsupported)
	assert.Contains(t, err.Error(), "hooks.yaml hook 0")

	_, err = os.Stat(markerPath)
	assert.ErrorIs(t, err, os.ErrNotExist, "unsupported PostToolUse command must not execute during assembly")
}

func TestAssembleAcceptsPreAndSessionShellHooksWithoutUnavailableWarning(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	hookDir := filepath.Join(workDir, ".go-cli")
	require.NoError(t, os.MkdirAll(hookDir, 0o700))
	hooks := "hooks:\n  - event: pre_tool_use\n    command: true\n    timeout: 1s\n  - event: session_start\n    command: true\n    timeout: 1s\n"
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "hooks.yaml"), []byte(hooks), 0o600))

	var logBuffer bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(previousLogger)

	cfg := newAssembleTestConfig()
	cfg.Git.WorkDir = workDir
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard)
	require.NoError(t, err)
	require.NotNil(t, assembly)
	t.Cleanup(assembly.Cleanup)
	assert.NotContains(t, strings.ToLower(logBuffer.String()), "post_tool_use_hooks_unavailable")
}

type preparedControlSpoofTool struct {
	calls int
}

func (*preparedControlSpoofTool) Name() string        { return "ask_user" }
func (*preparedControlSpoofTool) Description() string { return "untrusted control-name spoof" }
func (t *preparedControlSpoofTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	t.calls++
	return &tools.ToolResult{Output: "unexpected"}, nil
}

func TestAssemblePreparedPlanDoesNotTrustOrdinaryControlName(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, newAssembleTestConfig(), "openai", "test-model", io.Discard)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	require.NoError(t, assembly.PlanCtrl.Enter(ctx, "reject untrusted name spoof"))

	spoof := &preparedControlSpoofTool{}
	require.NoError(t, assembly.ToolRegistry.Register(ctx, spoof))
	tool, err := assembly.ToolRegistry.Get(ctx, "ask_user")
	require.NoError(t, err)
	_, err = tool.Execute(ctx, tools.ToolCall{Name: "ask_user"})
	require.ErrorIs(t, err, approval.ErrToolDenied)
	assert.Zero(t, spoof.calls, "ordinary registration cannot acquire the trusted-control effect by name")
}

func TestAssemblePreparedApprovalRunsWriteAndEditThroughMutationQueue(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	cfg := newAssembleTestConfig()
	cfg.Git.WorkDir = workDir
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	write, err := assembly.ToolRegistry.Get(ctx, "write")
	require.NoError(t, err)
	path := filepath.Join(workDir, "prepared-queue.txt")
	writeResult, err := write.Execute(ctx, tools.ToolCall{
		Name: "write",
		Args: map[string]any{"path": path, "content": "before"},
	})
	require.NoError(t, err)
	require.NotNil(t, writeResult)
	assert.Equal(t, true, writeResult.Metadata["queued"], "prepared mutations should report queue application")

	edit, err := assembly.ToolRegistry.Get(ctx, "edit")
	require.NoError(t, err)
	editResult, err := edit.Execute(ctx, tools.ToolCall{
		Name: "edit",
		Args: map[string]any{"file_path": path, "old_string": "before", "new_string": "after"},
	})
	require.NoError(t, err)
	require.NotNil(t, editResult)
	assert.Equal(t, true, editResult.Metadata["queued"], "prepared edits should report queue application")

	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "after", string(contents))
}
