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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
)

func TestAssembleAcceptsConfiguredPostToolUseShellHook(t *testing.T) {
	ctx := context.Background()
	workDir := t.TempDir()
	hookDir := filepath.Join(workDir, ".go-cli")
	require.NoError(t, os.MkdirAll(hookDir, 0o700))
	markerPath := filepath.Join(workDir, "post-tool-use-hook-ran")
	hooks := fmt.Sprintf("hooks:\n  - event: post_tool_use\n    command: touch %q\n    timeout: 1s\n", markerPath)
	require.NoError(t, os.WriteFile(filepath.Join(hookDir, "hooks.yaml"), []byte(hooks), 0o600))

	cfg := newTrustedProjectHookAssembleTestConfig(t, workDir)
	assembly, err := AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard)
	require.NoError(t, err)
	require.NotNil(t, assembly)
	t.Cleanup(assembly.Cleanup)

	_, err = os.Stat(markerPath)
	assert.ErrorIs(t, err, os.ErrNotExist, "PostToolUse command must not execute during assembly")
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

	cfg := newTrustedProjectHookAssembleTestConfig(t, workDir)
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
	assembly, err := AssembleAgent(ctx, newAssembleTestConfig(t), "openai", "test-model", io.Discard)
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
	cfg := newAssembleTestConfig(t)
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

type assembledWriteOutcome struct {
	result *tools.ToolResult
	err    error
}

func newAssembledApprovalWrite(t *testing.T, opts ...AssembleOption) (*AgentAssembly, tools.ToolDefinition, string) {
	t.Helper()
	workDir := t.TempDir()
	cfg := newAssembleTestConfig(t)
	cfg.Git.WorkDir = workDir
	assembly, err := AssembleAgent(context.Background(), cfg, "openai", "test-model", io.Discard, opts...)
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	write, err := assembly.ToolRegistry.Get(context.Background(), "write")
	require.NoError(t, err)
	return assembly, write, filepath.Join(workDir, "approval-matrix.txt")
}

func assembledWriteCall(path string) tools.ToolCall {
	return tools.ToolCall{
		Name: "write",
		Args: map[string]any{"path": path, "content": "approved"},
	}
}

func newTrustedProjectHookAssembleTestConfig(t *testing.T, workDir string) *config.Config {
	t.Helper()
	ctx := context.Background()
	t.Chdir(workDir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalConfigDir := filepath.Join(home, ".go-cli")
	require.NoError(t, os.MkdirAll(globalConfigDir, 0o700))
	globalConfig := "provider:\n  name: openai\n  api_key: test-key\n  base_url: http://127.0.0.1:1/v1\n  model: test-model\nmodel:\n  name: test-model\ntracing:\n  enabled: false\n"
	require.NoError(t, os.WriteFile(filepath.Join(globalConfigDir, "config.yaml"), []byte(globalConfig), 0o600))

	manager := approval.NewDefaultTrustManager(approval.NewInMemoryTrustStore())
	snapshot, err := config.CaptureProjectSources(ctx, workDir, nil)
	require.NoError(t, err)
	frozen, ok := manager.(approval.FrozenProjectSourceTrust)
	require.True(t, ok)
	require.NoError(t, frozen.TrustProjectSourceSnapshot(ctx, snapshot))

	cfg, err := config.LoadTrustedWithManager(ctx, manager)
	require.NoError(t, err)
	require.Equal(t, config.ProjectSourceTrustVerified, cfg.ProjectSourceTrustStatus)
	cfg.Git.WorkDir = workDir
	return cfg
}

func TestAssemblePreparedWriteApprovalModesKeepDecisionAtAssemblyBoundary(t *testing.T) {
	t.Run("headless ask denies without a terminal", func(t *testing.T) {
		previousStdin := os.Stdin
		nonTerminal, err := os.Open(os.DevNull)
		require.NoError(t, err)
		os.Stdin = nonTerminal
		t.Cleanup(func() {
			os.Stdin = previousStdin
			assert.NoError(t, nonTerminal.Close())
		})

		_, write, path := newAssembledApprovalWrite(t, WithApproveMode(ApproveAsk))
		result, err := write.Execute(context.Background(), assembledWriteCall(path))
		require.ErrorIs(t, err, approval.ErrToolDenied)
		assert.Nil(t, result)
		assert.NoFileExists(t, path, "headless Ask must deny rather than block or execute")
	})

	t.Run("explicit auto dispatches", func(t *testing.T) {
		_, write, path := newAssembledApprovalWrite(t, WithApproveMode(ApproveAuto))
		result, err := write.Execute(context.Background(), assembledWriteCall(path))
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, true, result.Metadata["queued"])
		contents, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "approved", string(contents))
	})

	t.Run("explicit deny does not dispatch", func(t *testing.T) {
		_, write, path := newAssembledApprovalWrite(t, WithApproveMode(ApproveDeny))
		result, err := write.Execute(context.Background(), assembledWriteCall(path))
		require.ErrorIs(t, err, approval.ErrToolDenied)
		assert.Nil(t, result)
		assert.NoFileExists(t, path)
	})

	t.Run("plan mode blocks even explicit auto", func(t *testing.T) {
		assembly, write, path := newAssembledApprovalWrite(t, WithApproveMode(ApproveAuto))
		require.NoError(t, assembly.PlanCtrl.Enter(context.Background(), "approval matrix"))
		result, err := write.Execute(context.Background(), assembledWriteCall(path))
		require.ErrorIs(t, err, approval.ErrToolDenied)
		assert.Nil(t, result)
		assert.NoFileExists(t, path)
	})
}

func TestAssembleTUIApprovalChannelControlsPreparedWriteDispatch(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		decision tui.ApprovalDecision
		allowed  bool
	}{
		{name: "allow", decision: tui.ApprovalAllow, allowed: true},
		{name: "deny", decision: tui.ApprovalDeny},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			approvalCh := make(chan tui.ApprovalRequest)
			_, write, path := newAssembledApprovalWrite(t, WithApprovalChannel(approvalCh))
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			done := make(chan assembledWriteOutcome, 1)
			go func() {
				result, err := write.Execute(ctx, assembledWriteCall(path))
				done <- assembledWriteOutcome{result: result, err: err}
			}()

			var request tui.ApprovalRequest
			select {
			case request = <-approvalCh:
			case <-time.After(3 * time.Second):
				t.Fatal("TUI approval channel did not receive the prepared write request")
			}
			assert.Equal(t, "write", request.ToolName)
			assert.Equal(t, path, request.Args["path"])
			assert.NotNil(t, request.ResponseCh)
			assert.NoFileExists(t, path, "executor must wait for the TUI approval response")
			request.ResponseCh <- tui.ApprovalResponse{Decision: testCase.decision}

			var outcome assembledWriteOutcome
			select {
			case outcome = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("prepared write did not finish after the TUI approval response")
			}
			if testCase.allowed {
				require.NoError(t, outcome.err)
				require.NotNil(t, outcome.result)
				assert.Equal(t, true, outcome.result.Metadata["queued"])
				contents, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, "approved", string(contents))
				return
			}

			require.ErrorIs(t, outcome.err, approval.ErrToolDenied)
			assert.Nil(t, outcome.result)
			assert.NoFileExists(t, path, "TUI denial must not reach the mutation executor")
		})
	}
}
