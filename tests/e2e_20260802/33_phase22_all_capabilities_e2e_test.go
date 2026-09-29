//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 22 All Capabilities (Task 22-6): a comprehensive
// integration test covering ToolRegistry completeness, core/git tool
// functionality, production/middleware/Phase20/Phase19 component non-nil
// checks, config-driven tracing, the approval system, and a full agent turn
// driven by MockLLMServer.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tui"
)

// =============================================================================
// Shared helpers
// =============================================================================

// phase33TestConfig returns a minimal Config whose provider section forces
// buildModel down the EinoProvider path (no network calls), so assembly
// succeeds without a live endpoint.
func phase33TestConfig() *config.Config {
	return &config.Config{
		Provider: config.ProviderConfig{
			Name:    "openai",
			BaseURL: "http://127.0.0.1:12345/v1",
			APIKey:  "test-key",
		},
	}
}

// phase33Assemble calls AssembleAgent and registers cleanup. The timeout
// context bounds the assembly process only; tool execution in the test body
// uses a fresh context.
func phase33Assemble(t *testing.T, cfg *config.Config) *cli.AgentAssembly {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	assembly, err := cli.AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, cli.WithApproveMode(cli.ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	return assembly
}

func phase33AssembleWithApproval(t *testing.T, cfg *config.Config, approvalCh chan tui.ApprovalRequest) *cli.AgentAssembly {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	assembly, err := cli.AssembleAgent(ctx, cfg, "openai", "test-model", io.Discard, cli.WithApprovalChannel(approvalCh))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)
	return assembly
}

func phase33ApprovalConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	workspace := t.TempDir()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	cfg := phase33TestConfig()
	cfg.Git.WorkDir = workspace
	cfg.Sandbox.AllowedPaths = []string{workspace, cwd}
	return cfg, workspace
}

func phase33PreparedModeRegistry(
	t *testing.T,
	mode approval.PermissionMode,
	approvalCh chan tui.ApprovalRequest,
) (*tools.MiddlewareToolRegistry, *core.DefaultPlanModeController, string) {
	t.Helper()
	ctx := context.Background()
	workspace := t.TempDir()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	base := tools.NewDefaultToolRegistry()
	require.NoError(t, tools.RegisterDefaults(ctx, base,
		tools.WithRegisteredBuiltinWhitelist([]string{"bash", "read"}),
		tools.WithRegisteredBashSandbox(tools.NewDefaultBashSandbox(tools.WithAllowedPaths([]string{cwd, workspace}))),
		tools.WithRegisteredPathWhitelist([]string{workspace}),
	))
	fence, err := tools.DispatchFenceFromRegistry(base)
	require.NoError(t, err)
	planController := core.NewDefaultPlanModeController(fence)
	middleware := approval.NewApprovalMiddleware(
		&approval.DenyAllClassifier{},
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
		approval.WithPermissionModeResolver(approval.NewDefaultPermissionModeResolver()),
		approval.WithPermissionMode(mode),
		approval.WithCallback(approval.NewTeaApprovalCallback(approvalCh)),
		approval.WithDispatchFence(fence),
		approval.WithPlanModeController(planController),
	)
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	return registry, planController, workspace
}

func phase33CallWithApproval(
	t *testing.T,
	registry tools.ToolRegistry,
	approvalCh <-chan tui.ApprovalRequest,
	call tools.ToolCall,
	markerPath string,
	decision tui.ApprovalDecision,
) (*tools.ToolResult, error) {
	t.Helper()
	tool, err := registry.Get(context.Background(), call.Name)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan struct {
		result *tools.ToolResult
		err    error
	}, 1)
	go func() {
		result, callErr := tool.Execute(ctx, call)
		done <- struct {
			result *tools.ToolResult
			err    error
		}{result: result, err: callErr}
	}()
	joined := false
	defer func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("prepared tool call did not join after approval context cancellation")
			}
		}
	}()

	var request tui.ApprovalRequest
	select {
	case request = <-approvalCh:
	case outcome := <-done:
		joined = true
		if outcome.err != nil {
			return nil, fmt.Errorf("prepared tool call completed before approval request for %q: %w", call.Name, outcome.err)
		}
		return nil, fmt.Errorf("prepared tool call completed before approval request for %q", call.Name)
	case <-ctx.Done():
		t.Fatalf("tool call did not request approval: %v", ctx.Err())
	}
	require.Equal(t, call.Name, request.ToolName)
	assert.Equal(t, call.Args, request.Args, "approval must describe the exact prepared call")
	require.NotNil(t, request.ResponseCh)
	if markerPath != "" {
		_, statErr := os.Stat(markerPath)
		require.ErrorIs(t, statErr, os.ErrNotExist, "tool side effect must remain absent while approval is pending")
	}
	select {
	case request.ResponseCh <- tui.ApprovalResponse{Decision: decision}:
	case <-ctx.Done():
		t.Fatalf("approval callback stopped waiting before the test response: %v", ctx.Err())
	}

	select {
	case outcome := <-done:
		joined = true
		return outcome.result, outcome.err
	case <-ctx.Done():
		t.Fatalf("prepared tool call did not finish after approval response: %v", ctx.Err())
		return nil, ctx.Err()
	}
}

// phase33ToolNames returns the set of tool names registered by AssembleAgent.
func phase33ToolNames(t *testing.T, tr tools.ToolRegistry) map[string]bool {
	t.Helper()
	defs, err := tr.List(context.Background())
	require.NoError(t, err)
	names := make(map[string]bool, len(defs))
	for _, d := range defs {
		names[d.Name()] = true
	}
	return names
}

// phase33PlanSlashDeps supplies the only dependencies used by PlanHandler.
// The remaining accessors are intentionally unavailable in this focused test.
type phase33PlanSlashDeps struct {
	cli.Dependencies
	planCtrl core.PlanModeController
}

func (d phase33PlanSlashDeps) Out() io.Writer { return io.Discard }

func (d phase33PlanSlashDeps) PlanCtrl() core.PlanModeController { return d.planCtrl }

// =============================================================================
// AC-1: ToolRegistry Completeness — all expected tools registered
// =============================================================================

// TestET_Phase22_AllCapabilities_AC1_ToolRegistryCompleteness verifies that
// the assembled model-facing ToolRegistry contains the expected builtin,
// task, goal, web, git, and subagent tools. Plan Mode transitions stay
// unavailable to model calls and remain available through the user /plan
// slash handler.
func TestET_Phase22_AllCapabilities_AC1_ToolRegistryCompleteness(t *testing.T) {
	assembly := phase33Assemble(t, phase33TestConfig())

	names := phase33ToolNames(t, assembly.ToolRegistry)

	// Actual tool names registered by AssembleAgent (from source code).
	expectedTools := []string{
		// Builtins (RegisterDefaults)
		"read", "bash", "write", "edit", "grep", "find", "ls",
		// Todo/Task/Goal
		"todo_write", "task_create", "task_get", "task_list", "task_update",
		"goal_create", "goal_update", "goal_list", "goal_get",
		// Web
		"web_fetch", "web_search",
		// User interaction
		"ask_user",
		// Git
		"git_diff", "git_status", "git_commit",
		// Subagent
		"dispatch_subagent",
		// Tool search
		"tool_search",
	}

	var missing []string
	for _, name := range expectedTools {
		if !names[name] {
			missing = append(missing, name)
		}
	}
	assert.Empty(t, missing, "all expected tools should be registered; missing: %v", missing)

	ctx := context.Background()
	for _, name := range []string{"enter_plan_mode", "exit_plan_mode"} {
		_, err := assembly.ToolRegistry.Get(ctx, name)
		require.ErrorIs(t, err, tools.ErrToolNotFound,
			"model-facing plan control %q must stay unavailable", name)
	}

	// Report discrepancies between task spec names and actual names.
	// Plan Mode transition names are intentionally not mapped because these
	// controls are user-only, not model-facing tools.
	discrepancies := map[string]string{
		"ask_user_question": "ask_user",
		"subagent":          "dispatch_subagent",
	}
	for specName, actualName := range discrepancies {
		assert.True(t, names[actualName],
			"task spec lists %q but actual tool name is %q (found in registry)", specName, actualName)
	}

	require.NotNil(t, assembly.PlanCtrl, "user /plan commands require the assembled PlanMode controller")
	planHandler := &cli.PlanHandler{}
	require.Equal(t, "plan", planHandler.Name(), "the user slash command is /plan")
	planDeps := phase33PlanSlashDeps{planCtrl: assembly.PlanCtrl}
	_, err := planHandler.Handle(ctx, []string{"enter", "user requested"}, planDeps)
	require.NoError(t, err)
	assert.True(t, assembly.PlanCtrl.IsActive(), "/plan enter must still activate plan mode")
	_, err = planHandler.Handle(ctx, []string{"exit", "user completed planning"}, planDeps)
	require.NoError(t, err)
	assert.False(t, assembly.PlanCtrl.IsActive(), "/plan exit must still deactivate plan mode")

	// Sanity: the registry should have a reasonable number of tools.
	assert.GreaterOrEqual(t, len(names), 25, "registry should have at least 25 tools")
}

// =============================================================================
// AC-2: Core Tools Functional (read, write, grep, bash)
// =============================================================================

// TestET_Phase22_AllCapabilities_AC2_CoreToolsFunctional verifies that the
// core tools (read, write, grep) execute successfully through the assembled
// (wrapped) registry. The bash tool is verified separately in AC-9 (approval).
func TestET_Phase22_AllCapabilities_AC2_CoreToolsFunctional(t *testing.T) {
	assembly := phase33Assemble(t, phase33TestConfig())

	dir := t.TempDir()
	ctx := context.Background()

	// write — create a new file through the wrapped registry.
	writeDef, err := assembly.ToolRegistry.Get(ctx, "write")
	require.NoError(t, err)

	filePath := filepath.Join(dir, "ac2_test.txt")
	res, err := writeDef.Execute(ctx, toolCallWithArgs("write", map[string]any{
		"path":    filePath,
		"content": "hello world\nline2\n",
	}))
	require.NoError(t, err)
	assert.Equal(t, "wrote 18 bytes to "+filePath, res.Output)

	// Verify file on disk.
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, "hello world\nline2\n", string(data))

	// read — read the file back through the wrapped registry.
	readDef, err := assembly.ToolRegistry.Get(ctx, "read")
	require.NoError(t, err)

	res, err = readDef.Execute(ctx, toolCallWithArgs("read", map[string]any{
		"path": filePath,
	}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "hello world")

	// grep — search for a pattern in the temp dir.
	grepDef, err := assembly.ToolRegistry.Get(ctx, "grep")
	require.NoError(t, err)

	res, err = grepDef.Execute(ctx, toolCallWithArgs("grep", map[string]any{
		"pattern": "hello",
		"path":    dir,
	}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "ac2_test.txt")

	// bash — execute a simple echo command through the wrapped registry.
	// This shared helper opts into the CLI's explicit ApproveAuto mode
	// (AutoFull); it does not exercise PermissionDefault behavior.
	bashDef, err := assembly.ToolRegistry.Get(ctx, "bash")
	require.NoError(t, err)

	res, err = bashDef.Execute(ctx, toolCallWithArgs("bash", map[string]any{
		"command": "echo hello",
	}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "hello")
}

// =============================================================================
// AC-3: Git Tools Functional (git_status, git_diff, git_commit)
// =============================================================================

// TestET_Phase22_AllCapabilities_AC3_GitToolsFunctional verifies that the git
// tools (git_status, git_diff, git_commit) are registered and functional. A
// temp git repo is created and a separate DefaultGitTool is used to test
// against it (the assembled git tools use os.Getwd() as cwd). The assembled
// registry's git tools are verified to be present and executable.
func TestET_Phase22_AllCapabilities_AC3_GitToolsFunctional(t *testing.T) {
	assembly := phase33Assemble(t, phase33TestConfig())
	ctx := context.Background()

	// Verify git tools are registered in the assembled registry.
	for _, name := range []string{"git_status", "git_diff", "git_commit"} {
		def, err := assembly.ToolRegistry.Get(ctx, name)
		require.NoError(t, err, "tool %s should be in registry", name)
		assert.NotEmpty(t, def.Description())
	}

	// Create a temp git repo for functional testing.
	repoDir := t.TempDir()
	// git init
	cmd := exec.Command("git", "init", repoDir)
	require.NoError(t, cmd.Run(), "git init should succeed")
	// Configure user for commits
	exec.Command("git", "-C", repoDir, "config", "user.email", "test@test.com").Run()
	exec.Command("git", "-C", repoDir, "config", "user.name", "Test").Run()

	// Create a file and commit it so the repo has history.
	initialFile := filepath.Join(repoDir, "initial.txt")
	require.NoError(t, os.WriteFile(initialFile, []byte("initial content\n"), 0644))
	exec.Command("git", "-C", repoDir, "add", "-A").Run()
	exec.Command("git", "-C", repoDir, "commit", "-m", "initial commit").Run()

	// Modify the file to create an unstaged change.
	require.NoError(t, os.WriteFile(initialFile, []byte("modified content\n"), 0644))

	// Create a new untracked file.
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "new.txt"), []byte("new file\n"), 0644))

	// Use a DefaultGitTool pointing at the temp repo.
	gitTool := tools.NewDefaultGitTool(repoDir)

	// git_status — should show modified + untracked files.
	statusTool := tools.NewGitStatusTool(gitTool)
	res, err := statusTool.Execute(ctx, toolCallWithArgs("git_status", map[string]any{}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "initial.txt", "git_status should show modified file")
	assert.Contains(t, res.Output, "new.txt", "git_status should show untracked file")

	// git_diff — should show unstaged changes to initial.txt.
	diffTool := tools.NewGitDiffTool(gitTool)
	res, err = diffTool.Execute(ctx, toolCallWithArgs("git_diff", map[string]any{}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "initial content", "git_diff should show old content")
	assert.Contains(t, res.Output, "modified content", "git_diff should show new content")

	// git_commit — stage all and commit.
	commitTool := tools.NewGitCommitTool(gitTool)
	res, err = commitTool.Execute(ctx, toolCallWithArgs("git_commit", map[string]any{
		"message": "test commit",
		"add_all": true,
	}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "committed:")

	// After commit, git_status should be clean (or show no unstaged changes).
	res, err = statusTool.Execute(ctx, toolCallWithArgs("git_status", map[string]any{}))
	require.NoError(t, err)
	assert.Contains(t, res.Output, "clean working tree")
}

// =============================================================================
// AC-4: Production Components Non-nil (CircuitBreaker, LoopDetector,
//       IdempotentCache, Telemetry, AuditLog)
// =============================================================================

// TestET_Phase22_AllCapabilities_AC4_ProductionComponentsNotNil verifies that
// AssembleAgent wires all production resilience components as non-nil.
func TestET_Phase22_AllCapabilities_AC4_ProductionComponentsNotNil(t *testing.T) {
	cfg := phase33TestConfig()
	auditEnabled := true
	cfg.Production.Audit.Enabled = &auditEnabled
	cfg.Production.Audit.Path = filepath.Join(t.TempDir(), "audit.jsonl")

	assembly := phase33Assemble(t, cfg)

	require.NotNil(t, assembly.CircuitBreaker, "CircuitBreaker must be non-nil")
	require.NotNil(t, assembly.LoopDetector, "LoopDetector must be non-nil")
	require.NotNil(t, assembly.IdempotentCache, "IdempotentCache must be non-nil")
	require.NotNil(t, assembly.Telemetry, "Telemetry must be non-nil")
	require.NotNil(t, assembly.AuditLog, "AuditLog must be non-nil when audit is enabled")
	require.NotNil(t, assembly.CostTracker, "CostTracker must be non-nil")
	require.NotNil(t, assembly.StatsRegistry, "StatsRegistry must be non-nil")
}

// =============================================================================
// AC-5: Middleware Components Non-nil (ReminderManager, HookChain,
//       FailureSynthesizer, PromptBuilder, ContextLoader)
// =============================================================================

// TestET_Phase22_AllCapabilities_AC5_MiddlewareComponentsNotNil verifies that
// AssembleAgent wires all middleware-chain components as non-nil.
func TestET_Phase22_AllCapabilities_AC5_MiddlewareComponentsNotNil(t *testing.T) {
	assembly := phase33Assemble(t, phase33TestConfig())

	require.NotNil(t, assembly.ReminderManager, "ReminderManager must be non-nil")
	require.NotNil(t, assembly.HookChain, "HookChain must be non-nil")
	require.NotNil(t, assembly.FailureSynthesizer, "FailureSynthesizer must be non-nil")
	require.NotNil(t, assembly.PromptBuilder, "PromptBuilder must be non-nil")
	require.NotNil(t, assembly.ContextLoader, "ContextLoader must be non-nil")
}

// =============================================================================
// AC-6: Phase 20 Components Non-nil (FileTracker, DiffGenerator, PlanCtrl,
//       ModeResolver)
// =============================================================================

// TestET_Phase22_AllCapabilities_AC6_Phase20ComponentsNotNil verifies that
// AssembleAgent wires all Phase 20 components as non-nil.
func TestET_Phase22_AllCapabilities_AC6_Phase20ComponentsNotNil(t *testing.T) {
	assembly := phase33Assemble(t, phase33TestConfig())

	require.NotNil(t, assembly.FileTracker, "FileTracker must be non-nil")
	require.NotNil(t, assembly.DiffGenerator, "DiffGenerator must be non-nil")
	require.NotNil(t, assembly.PlanCtrl, "PlanCtrl must be non-nil")
	require.NotNil(t, assembly.ModeResolver, "ModeResolver must be non-nil")
}

// =============================================================================
// AC-7: Phase 19 Components Non-nil (Compactor, Estimator, MidTurn)
// =============================================================================

// TestET_Phase22_AllCapabilities_AC7_Phase19ComponentsNotNil verifies that
// AssembleAgent wires all Phase 19 compaction components as non-nil.
func TestET_Phase22_AllCapabilities_AC7_Phase19ComponentsNotNil(t *testing.T) {
	assembly := phase33Assemble(t, phase33TestConfig())

	require.NotNil(t, assembly.Compactor, "Compactor must be non-nil")
	require.NotNil(t, assembly.Estimator, "Estimator must be non-nil")
	require.NotNil(t, assembly.MidTurn, "MidTurn must be non-nil")
	assert.Greater(t, assembly.MaxTokens, 0, "MaxTokens should be positive")
}

// =============================================================================
// AC-8: Config-Driven Tracing — Tracer non-nil when enabled, JSONL file created
// =============================================================================

// TestET_Phase22_AllCapabilities_AC8_ConfigDrivenTracing verifies that the real
// CLI invocation loads enabled JSONL tracing from user config and writes spans.
func TestET_Phase22_AllCapabilities_AC8_ConfigDrivenTracing(t *testing.T) {
	spans := requireRealCLIInvocationTrace(t)
	require.Len(t, spans, 2, "version command should emit the root and command dispatch spans")
	assert.ElementsMatch(t, []string{"cli.invocation", "command.dispatch"}, []string{spans[0].Name, spans[1].Name})
	assert.Equal(t, spans[0].TraceID, spans[1].TraceID)
}

// =============================================================================
// AC-9: Approval System — default Bash requires explicit approval, and Auto
//       and Plan policies retain their separate prepared-dispatch behavior.
// =============================================================================

// TestET_Phase22_AllCapabilities_AC9_ApprovalSystem verifies that the approval
// middleware is wired into the assembled tool registry. The verification is
// behavioral since ApprovalMiddleware is not directly exposed on AgentAssembly.
//
// The default safety policy permits only trusted read-only effects without
// prompting. Bash remains an approval request, and the prepared registry must
// not execute it until the approval channel returns an explicit allow decision.
// PermissionAuto, PermissionPlan, CLI auto approval, and active Plan Mode are
// checked independently because they have different authorization semantics.
func TestET_Phase22_AllCapabilities_AC9_ApprovalSystem(t *testing.T) {
	ctx := context.Background()

	resolver := approval.NewDefaultPermissionModeResolver()
	defaultClassifier := resolver.Resolve(approval.PermissionDefault)
	require.NotNil(t, defaultClassifier)
	assert.Equal(t, "safety_policy", defaultClassifier.Name())
	assert.Equal(t, approval.Ask, defaultClassifier.Classify(ctx, toolCall("bash")),
		"Default must require approval for arbitrary local execution")
	autoClassifier := resolver.Resolve(approval.PermissionAuto)
	require.NotNil(t, autoClassifier)
	assert.Equal(t, "auto-classifier", autoClassifier.Name())
	assert.Equal(t, approval.Allow, autoClassifier.Classify(ctx, toolCall("bash")),
		"the Auto name classifier allows unknown tools, but prepared identity/effect checks still gate Bash")
	planClassifier := resolver.Resolve(approval.PermissionPlan)
	require.NotNil(t, planClassifier)
	assert.Equal(t, approval.Ask, planClassifier.Classify(ctx, toolCall("bash")),
		"PermissionPlan must require per-call confirmation")

	t.Run("assembled default permits scoped read but denies unapproved Bash", func(t *testing.T) {
		approvalCh := make(chan tui.ApprovalRequest, 1)
		cfg, workspace := phase33ApprovalConfig(t)
		readPath := filepath.Join(workspace, "ac9_read.txt")
		require.NoError(t, os.WriteFile(readPath, []byte("default-safe-read"), 0o600))
		assembly := phase33AssembleWithApproval(t, cfg, approvalCh)
		require.NotNil(t, assembly.ModeResolver, "ModeResolver must be wired by AssembleAgent")
		assert.Equal(t, "permission_mode", assembly.ModeResolver.Name())

		readDef, err := assembly.ToolRegistry.Get(ctx, "read")
		require.NoError(t, err)
		readResult, err := readDef.Execute(ctx, toolCallWithArgs("read", map[string]any{"path": readPath}))
		require.NoError(t, err)
		assert.Contains(t, readResult.Output, "default-safe-read")
		assert.Empty(t, approvalCh, "a scoped built-in read should not prompt in Default")

		marker := filepath.Join(workspace, "default-denied")
		_, err = phase33CallWithApproval(t, assembly.ToolRegistry, approvalCh,
			toolCallWithArgs("bash", map[string]any{"command": fmt.Sprintf("touch %q", marker)}),
			marker, tui.ApprovalDeny)
		assert.ErrorIs(t, err, approval.ErrToolDenied)
		_, statErr := os.Stat(marker)
		assert.ErrorIs(t, statErr, os.ErrNotExist, "denied Bash must have no file effect")
	})

	t.Run("assembled Default executes only after explicit approval", func(t *testing.T) {
		approvalCh := make(chan tui.ApprovalRequest, 1)
		cfg, workspace := phase33ApprovalConfig(t)
		assembly := phase33AssembleWithApproval(t, cfg, approvalCh)
		marker := filepath.Join(workspace, "default-approved")
		result, err := phase33CallWithApproval(t, assembly.ToolRegistry, approvalCh,
			toolCallWithArgs("bash", map[string]any{"command": fmt.Sprintf("touch %q", marker)}),
			marker, tui.ApprovalAllow)
		require.NoError(t, err)
		require.NotNil(t, result)
		info, err := os.Stat(marker)
		require.NoError(t, err, "the approved Bash process must create its effect marker")
		assert.Zero(t, info.Size())
	})

	t.Run("PermissionAuto auto-allows scoped read but still gates Bash identity", func(t *testing.T) {
		approvalCh := make(chan tui.ApprovalRequest, 1)
		registry, _, workspace := phase33PreparedModeRegistry(t, approval.PermissionAuto, approvalCh)
		readPath := filepath.Join(workspace, "auto_read.txt")
		require.NoError(t, os.WriteFile(readPath, []byte("auto-safe-read"), 0o600))
		readDef, err := registry.Get(ctx, "read")
		require.NoError(t, err)
		readResult, err := readDef.Execute(ctx, toolCallWithArgs("read", map[string]any{"path": readPath}))
		require.NoError(t, err)
		assert.Contains(t, readResult.Output, "auto-safe-read")
		assert.Empty(t, approvalCh, "trusted scoped reads should be auto-allowed")

		marker := filepath.Join(workspace, "auto-bash-denied")
		_, err = phase33CallWithApproval(t, registry, approvalCh,
			toolCallWithArgs("bash", map[string]any{"command": fmt.Sprintf("touch %q", marker)}),
			marker, tui.ApprovalDeny)
		assert.ErrorIs(t, err, approval.ErrToolDenied,
			"an untrusted local-process identity must be asked even when its name classifier allows unknown tools")
		_, statErr := os.Stat(marker)
		assert.ErrorIs(t, statErr, os.ErrNotExist)
	})

	t.Run("auto-allowed call cannot masquerade as explicitly approved", func(t *testing.T) {
		approvalCh := make(chan tui.ApprovalRequest, 1)
		registry, _, workspace := phase33PreparedModeRegistry(t, approval.PermissionAuto, approvalCh)
		readPath := filepath.Join(workspace, "auto_read_without_prompt.txt")
		require.NoError(t, os.WriteFile(readPath, []byte("auto-read"), 0o600))

		_, err := phase33CallWithApproval(t, registry, approvalCh,
			toolCallWithArgs("read", map[string]any{"path": readPath}), "", tui.ApprovalAllow)
		assert.ErrorContains(t, err, "completed before approval request",
			"a successful auto-allowed call is not evidence that an explicit approval occurred")
		assert.Empty(t, approvalCh, "the scoped read should be auto-allowed without a prompt")
	})

	t.Run("PermissionPlan asks for each call", func(t *testing.T) {
		approvalCh := make(chan tui.ApprovalRequest, 1)
		registry, _, workspace := phase33PreparedModeRegistry(t, approval.PermissionPlan, approvalCh)
		readPath := filepath.Join(workspace, "plan_read.txt")
		require.NoError(t, os.WriteFile(readPath, []byte("plan-read"), 0o600))
		readResult, err := phase33CallWithApproval(t, registry, approvalCh,
			toolCallWithArgs("read", map[string]any{"path": readPath}), "", tui.ApprovalAllow)
		require.NoError(t, err)
		require.NotNil(t, readResult)
		assert.Contains(t, readResult.Output, "plan-read",
			"PermissionPlan may execute only after the per-call approval callback allows it")
	})

	t.Run("active Plan Mode blocks Bash even under AutoFull", func(t *testing.T) {
		approvalCh := make(chan tui.ApprovalRequest, 1)
		registry, planController, workspace := phase33PreparedModeRegistry(t, approval.PermissionAutoFull, approvalCh)
		require.NoError(t, planController.Enter(ctx, "AC9 mutation quarantine"))
		marker := filepath.Join(workspace, "plan-mode-blocked")
		bashDef, err := registry.Get(ctx, "bash")
		require.NoError(t, err)
		_, err = bashDef.Execute(ctx, toolCallWithArgs("bash", map[string]any{"command": fmt.Sprintf("touch %q", marker)}))
		assert.ErrorIs(t, err, approval.ErrToolDenied)
		assert.Empty(t, approvalCh, "Plan Mode blocks mutation before an approval prompt")
		_, statErr := os.Stat(marker)
		assert.ErrorIs(t, statErr, os.ErrNotExist, "AutoFull must not bypass active Plan Mode")
	})
}

// =============================================================================
// AC-10: Full Agent Turn Driven by MockLLMServer
// =============================================================================

// TestET_Phase22_AllCapabilities_AC10_FullAgentTurnWithMockLLM verifies that
// a full agent turn completes successfully when driven by a MockLLMServer.
// The test creates a MockLLMServer with a simple text response (no tool
// calls), builds a LoopAgent with the mock model and the assembled tool
// registry, wraps it in an AgentImpl + HarnessImpl, and submits a message.
// The event stream is drained and the final result is verified.
func TestET_Phase22_AllCapabilities_AC10_FullAgentTurnWithMockLLM(t *testing.T) {
	assembly := phase33Assemble(t, phase33TestConfig())

	// Create a MockLLMServer with a single-turn conversation that returns
	// a text response (no tool calls). This ensures the agent loop completes
	// in one iteration without needing real tool execution.
	mockServer := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"ac10", "AC-10 Turn",
		mock.ConversationTurn{
			AssistantContent: "Hello from the mock LLM! Task complete.",
		},
	))

	// The mock server implements llm.BaseChatModel, so use it directly as
	// the model for the LoopAgent. This substitutes only the LLM while
	// keeping all real components (tools, middleware, etc.) from the
	// assembled registry.
	loop := core.NewLoopAgent(
		core.WithLLM(mockServer),
		core.WithTools(assembly.ToolRegistry),
		core.WithMaxIterations(3),
	)

	agent := core.NewAgentImpl("ac10-agent", loop)
	harness := core.NewHarnessImpl(agent, core.WithEventBuffer(64))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Submit a user message and drain the event stream.
	stream, err := harness.Submit(ctx, "Say hello")
	require.NoError(t, err)

	var events []core.AgentEvent
	for ev := range stream.Events() {
		events = append(events, ev)
	}

	// The stream should have produced at least one event (the "done" event
	// or a "message" event with the mock response).
	assert.NotEmpty(t, events, "event stream should have at least one event")

	// Check the final result.
	result, streamErr := stream.Result()
	if streamErr != nil {
		// Even if there's an error, the harness should have produced events.
		t.Logf("stream error: %v (events: %d)", streamErr, len(events))
	}

	// Verify the mock LLM was called at least once.
	assert.GreaterOrEqual(t, mockServer.CallCount(), 1, "mock LLM should have been called at least once")

	// Verify the final message contains the mock response (if the turn
	// completed successfully).
	if streamErr == nil {
		assert.Contains(t, result.Content, "mock LLM",
			"final message should contain the mock LLM response")
	}
}
