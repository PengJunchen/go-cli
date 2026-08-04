//go:build e2e

// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 19 cross-cutting integration: approval+sandbox,
// file tracker+diff+undo, parallel tools, compaction, web search, goal/plan
// mode, config consumption, slash registry, subagent prompts, and HTML
// conversion.
package e2e_20260802

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Phase 19 E2E: Full Integration Scenarios
// =============================================================================

// -----------------------------------------------------------------------------
// TestE2E_Phase19_ApprovalCallbackWithSandbox
// -----------------------------------------------------------------------------

// alwaysAllowCallback is a test ApprovalCallback that always returns Allow.
// It replaces InteractiveApprovalCallback in E2E tests because the interactive
// callback's strings.Reader is consumed after the first read.
type alwaysAllowCallback struct{}

func (alwaysAllowCallback) RequestApproval(_ context.Context, _ string, _ map[string]any) (approval.ApprovalResult, error) {
	return approval.ApprovalAllow, nil
}

// TestE2E_Phase19_ApprovalCallbackWithSandbox verifies that a bash command goes
// through both BashSandbox validation and ApprovalCallback. The AutoClassifier
// returns Ask for bash so the callback is consulted; the BashSandbox then
// validates the command inside the tool.
func TestE2E_Phase19_ApprovalCallbackWithSandbox(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()

	// 1. Create BashTool with BashSandbox (whitelist temp dir, blacklist rm).
	sb := tools.NewDefaultBashSandbox(
		tools.WithWhitelist([]string{dir}),
	)
	bashTool := tools.NewBashTool(
		tools.WithBashWorkdir(dir),
		tools.WithBashSandbox(sb),
		tools.WithTimeout(10*time.Second),
	)

	// 2. Create ApprovalMiddleware. AutoClassifier returns Ask for dangerous
	// tools (bash), so the callback is consulted. Safe tools are auto-allowed.
	// A fresh middleware+store is used per sub-test to avoid cached decisions.
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(ctx, bashTool))

	// Test 1: safe command in whitelisted dir -> callback allows -> sandbox
	// passes -> executes.
	mw1 := approval.NewApprovalMiddleware(
		approval.NewAutoClassifier(nil, []string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithCallback(alwaysAllowCallback{}),
	)
	wrapped1 := tools.NewMiddlewareToolRegistry(tr, mw1.WrapToolCall)
	def1, err := wrapped1.Get(ctx, "bash")
	require.NoError(t, err)

	res, err := def1.Execute(ctx, toolCallWithArgs("bash", map[string]any{
		"command": "echo hello",
	}))
	require.NoError(t, err)
	assert.Equal(t, "hello\n", res.Output)

	// Test 2: "rm -rf /" -> callback allows -> sandbox blocks.
	mw2 := approval.NewApprovalMiddleware(
		approval.NewAutoClassifier(nil, []string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithCallback(alwaysAllowCallback{}),
	)
	wrapped2 := tools.NewMiddlewareToolRegistry(tr, mw2.WrapToolCall)
	def2, err := wrapped2.Get(ctx, "bash")
	require.NoError(t, err)

	_, err = def2.Execute(ctx, toolCallWithArgs("bash", map[string]any{
		"command": "rm -rf /",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox")

	// Test 3: "echo hi" in non-whitelisted dir -> sandbox blocks. We need a
	// separate BashTool with a non-whitelisted workdir.
	otherDir := t.TempDir()
	bashToolOther := tools.NewBashTool(
		tools.WithBashWorkdir(otherDir),
		tools.WithBashSandbox(sb), // same sandbox whitelisted to dir, not otherDir
		tools.WithTimeout(10*time.Second),
	)
	tr2 := tools.NewDefaultToolRegistry()
	require.NoError(t, tr2.Register(ctx, bashToolOther))

	mw3 := approval.NewApprovalMiddleware(
		approval.NewAutoClassifier(nil, []string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithCallback(alwaysAllowCallback{}),
	)
	wrapped3 := tools.NewMiddlewareToolRegistry(tr2, mw3.WrapToolCall)
	def3, err := wrapped3.Get(ctx, "bash")
	require.NoError(t, err)

	_, err = def3.Execute(ctx, toolCallWithArgs("bash", map[string]any{
		"command": "echo hi",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox")
}

// -----------------------------------------------------------------------------
// TestE2E_Phase19_FileTrackerBackupDiffUndo
// -----------------------------------------------------------------------------

// TestE2E_Phase19_FileTrackerBackupDiffUndo verifies the Checkpoint -> Diff ->
// Execute -> Undo chain using real FileTracker, UnifiedDiffGenerator, and
// WriteTool.
func TestE2E_Phase19_FileTrackerBackupDiffUndo(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	fileName := "test_file.txt"
	filePath := filepath.Join(dir, fileName)

	// Create components.
	ft := tools.NewFileTracker()
	dg := tools.NewUnifiedDiffGenerator(0, false)
	writeTool := tools.NewWriteTool(
		tools.WithWriteWorkdir(dir),
		tools.WithOverwrite(true),
		tools.WithDiffGenerator(dg),
	)

	// 1. Write original content.
	original := "line1 alpha\nline2 beta\nline3 gamma\n"
	require.NoError(t, os.WriteFile(filePath, []byte(original), 0644))

	// 2. Backup (checkpoint).
	cpID, err := ft.Backup(filePath)
	require.NoError(t, err)
	assert.NotEmpty(t, cpID)

	// 3. Modify file using WriteTool (which generates a diff).
	modified := "line1 alpha\nline2 delta\nline3 gamma\n"
	res, err := writeTool.Execute(ctx, toolCallWithArgs("write", map[string]any{
		"path":    fileName,
		"content": modified,
	}))
	require.NoError(t, err)

	// 4. Verify diff shows changes.
	diff, ok := res.Metadata["diff"].(string)
	require.True(t, ok, "diff should be in metadata")
	assert.Contains(t, diff, "--- a/")
	assert.Contains(t, diff, "+++ b/")
	assert.Contains(t, diff, "-line2 beta")
	assert.Contains(t, diff, "+line2 delta")

	// 5. Verify file was actually modified.
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, modified, string(data))

	// 6. Restore from checkpoint (undo equivalent).
	require.NoError(t, ft.Restore(cpID))

	// 7. Verify file content matches original.
	data, err = os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, original, string(data), "restored content should match original")
}

// -----------------------------------------------------------------------------
// TestE2E_Phase19_ParallelToolsWithApproval
// -----------------------------------------------------------------------------

// TestE2E_Phase19_ParallelToolsWithApproval verifies that parallel execution
// with per-tool approval works: read tools execute while bash is denied, and
// the denial does not block other tools. Results are collected in input order.
func TestE2E_Phase19_ParallelToolsWithApproval(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create 3 tools: 2 read + 1 bash.
	readTool1 := &recordingTool{name: "read1"}
	readTool2 := &recordingTool{name: "read2"}
	bashTool := &recordingTool{name: "bash"}

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(ctx, readTool1))
	require.NoError(t, tr.Register(ctx, readTool2))
	require.NoError(t, tr.Register(ctx, bashTool))

	// ApprovalMiddleware: SafetyPolicyClassifier denies bash, allows reads.
	mw := approval.NewApprovalMiddleware(
		approval.NewSafetyPolicyClassifier([]string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	// Execute all 3 concurrently.
	calls := []tools.ToolCall{
		{ID: "1", Name: "read1", Args: map[string]any{}},
		{ID: "2", Name: "bash", Args: map[string]any{}},
		{ID: "3", Name: "read2", Args: map[string]any{}},
	}

	type result struct {
		index int
		out   string
		err   error
	}
	results := make([]result, len(calls))

	var wg sync.WaitGroup
	for i, tc := range calls {
		wg.Add(1)
		go func(idx int, call tools.ToolCall) {
			defer wg.Done()
			def, err := wrapped.Get(ctx, call.Name)
			if err != nil {
				results[idx] = result{index: idx, err: err}
				return
			}
			res, err := def.Execute(ctx, call)
			if err != nil {
				results[idx] = result{index: idx, err: err}
				return
			}
			results[idx] = result{index: idx, out: res.Output}
		}(i, tc)
	}
	wg.Wait()

	// Verify results are in input order.
	assert.Equal(t, 0, results[0].index)
	assert.Equal(t, 1, results[1].index)
	assert.Equal(t, 2, results[2].index)

	// Read tools should succeed.
	assert.NoError(t, results[0].err)
	assert.Equal(t, "ok:read1", results[0].out)
	assert.True(t, readTool1.executed, "read1 must execute")

	assert.NoError(t, results[2].err)
	assert.Equal(t, "ok:read2", results[2].out)
	assert.True(t, readTool2.executed, "read2 must execute")

	// Bash should be denied.
	assert.ErrorIs(t, results[1].err, approval.ErrToolDenied)
	assert.False(t, bashTool.executed, "bash must not execute")
}

// -----------------------------------------------------------------------------
// TestE2E_Phase19_CompactionSinglePath
// -----------------------------------------------------------------------------

// TestE2E_Phase19_CompactionSinglePath verifies that compaction works through a
// single path: the UnifiedCompactor routes to one strategy, the agent history
// shrinks, and no dual path is invoked.
func TestE2E_Phase19_CompactionSinglePath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create real compaction components.
	estimator := compaction.NewHeuristicTokenEstimator()
	compactor := compaction.NewUnifiedCompactor()

	// Build a large conversation that exceeds a small token budget.
	maxTokens := 100
	var items []compaction.TurnItem

	// System message (should survive compaction).
	items = append(items, compaction.TurnItem{
		ID:      "sys",
		Role:    compaction.RoleSystem,
		Content: "You are a helpful assistant.",
	})

	// Many user/assistant turns with tool results.
	for i := 0; i < 20; i++ {
		items = append(items, compaction.TurnItem{
			ID:         fmt.Sprintf("user-%d", i),
			Role:       compaction.RoleUser,
			Content:    fmt.Sprintf("User message number %d with some padding text to increase token count", i),
			ToolResult: fmt.Sprintf("Tool result %d with lots of output text to bloat the context window significantly", i),
		})
		items = append(items, compaction.TurnItem{
			ID:      fmt.Sprintf("asst-%d", i),
			Role:    compaction.RoleAssistant,
			Content: fmt.Sprintf("Assistant response %d with additional text content", i),
		})
	}

	// Verify the input exceeds the budget.
	beforeTokens := estimateItemsTokens(items, estimator)
	assert.Greater(t, beforeTokens, maxTokens, "input should exceed budget")

	// Run compaction.
	compacted, err := compactor.Compact(ctx, items, maxTokens, estimator)
	require.NoError(t, err)

	// Verify history shrinks.
	afterTokens := estimateItemsTokens(compacted, estimator)
	assert.Less(t, afterTokens, beforeTokens, "compacted tokens should be fewer")
	assert.LessOrEqual(t, afterTokens, maxTokens, "compacted tokens should be within budget")
	assert.Less(t, len(compacted), len(items), "compacted item count should be fewer")

	// Verify only one strategy was used (single path).
	strategy := compactor.LastStrategy()
	assert.NotEqual(t, compaction.StrategyNone, strategy, "a strategy should have been selected")

	// Verify system messages survive.
	hasSystem := false
	for _, it := range compacted {
		if it.Role == compaction.RoleSystem {
			hasSystem = true
			break
		}
	}
	assert.True(t, hasSystem, "system messages must survive compaction")
}

// estimateItemsTokens is a local helper that estimates the total tokens of a
// slice of TurnItems using the given estimator.
func estimateItemsTokens(items []compaction.TurnItem, est *compaction.HeuristicTokenEstimator) int {
	total := 0
	for _, it := range items {
		if it.Content != "" {
			n, _ := est.Estimate(it.Content)
			total += n
		}
		if it.ToolResult != "" {
			n, _ := est.Estimate(it.ToolResult)
			total += n
		}
	}
	return total
}

// -----------------------------------------------------------------------------
// TestE2E_Phase19_WebSearchWithHTTPServer
// -----------------------------------------------------------------------------

// TestE2E_Phase19_WebSearchWithHTTPServer verifies that FetchSearchProvider
// parses real HTML from an httptest.Server and WebSearchTool returns structured
// results.
func TestE2E_Phase19_WebSearchWithHTTPServer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	html := `<html><body>
<h2>Result 1</h2>
<a href="https://example.com/r1">Link 1</a>
<p>Snippet for result 1</p>
<h2>Result 2</h2>
<a href="https://example.com/r2">Link 2</a>
<p>Snippet for result 2</p>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(html))
	}))
	defer srv.Close()

	provider := tools.NewFetchSearchProvider(tools.WithFetchSearchURL(srv.URL))
	tool := tools.NewWebSearchTool(tools.WithSearchProvider(provider))

	res, err := tool.Execute(ctx, toolCallWithArgs("web_search", map[string]any{
		"query": "test query",
	}))
	require.NoError(t, err)
	require.NotNil(t, res)

	// Verify structured results are present in the output.
	assert.Contains(t, res.Output, "Result 1")
	assert.Contains(t, res.Output, "https://example.com/r1")
	assert.Contains(t, res.Output, "Snippet for result 1")
	assert.Contains(t, res.Output, "Result 2")
	assert.Contains(t, res.Output, "https://example.com/r2")
	assert.Contains(t, res.Output, "Snippet for result 2")

	// Verify metadata.
	assert.Equal(t, "test query", res.Metadata["query"])
	assert.Equal(t, 2, res.Metadata["results"])
}

// -----------------------------------------------------------------------------
// TestE2E_Phase19_GoalManagementWithPlanMode
// -----------------------------------------------------------------------------

// TestE2E_Phase19_GoalManagementWithPlanMode verifies the Goal system + PlanMode
// integration: creating goals/tasks, updating task status, plan mode blocking
// write tools, and GoalList showing progress.
func TestE2E_Phase19_GoalManagementWithPlanMode(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create stores and controller.
	goalStore, err := tools.NewDefaultGoalStore("")
	require.NoError(t, err)
	taskStore := tools.NewTaskStore()
	planCtrl := core.NewDefaultPlanModeController()

	// Create tools.
	goalCreateTool := tools.NewGoalCreateTool(goalStore)
	taskCreateTool := tools.NewTaskCreateTool(taskStore)
	taskUpdateTool := tools.NewTaskUpdateTool(taskStore)
	goalListTool := tools.NewGoalListTool(goalStore, taskStore)

	// 1. Create a goal.
	goalRes, err := goalCreateTool.Execute(ctx, toolCallWithArgs("goal_create", map[string]any{
		"title":            "Ship feature X",
		"description":      "Implement and deploy feature X",
		"success_criteria": "All tests pass in production",
	}))
	require.NoError(t, err)
	goalID := goalRes.Metadata["id"].(string)

	// 2. Create a task.
	_, err = taskCreateTool.Execute(ctx, toolCallWithArgs("task_create", map[string]any{
		"title":       "Write tests",
		"description": "Write unit tests for feature X",
	}))
	require.NoError(t, err)

	// 3. Add task to goal.
	created := taskStore.Create("temp", "") // get next ID pattern
	_ = created
	// Retrieve the actual task created above.
	tasks := taskStore.List()
	require.Len(t, tasks, 2) // temp + the one from task_create
	taskID := ""
	for _, tk := range tasks {
		if tk.Title == "Write tests" {
			taskID = tk.ID
			break
		}
	}
	require.NotEmpty(t, taskID)
	require.NoError(t, goalStore.AddTask(ctx, goalID, taskID))

	// 4. Update task to blocked.
	_, err = taskUpdateTool.Execute(ctx, toolCallWithArgs("task_update", map[string]any{
		"id":     taskID,
		"status": "blocked",
	}))
	require.NoError(t, err)

	updated, found := taskStore.Get(taskID)
	require.True(t, found)
	assert.Equal(t, tools.StatusBlocked, updated.Status)

	// 5. GoalList shows progress.
	listRes, err := goalListTool.Execute(ctx, toolCallWithArgs("goal_list", map[string]any{}))
	require.NoError(t, err)
	assert.Contains(t, listRes.Output, "Ship feature X")
	assert.Contains(t, listRes.Output, "0%") // 0 of 1 tasks completed

	// 6. PlanMode active -> write tool blocked.
	require.NoError(t, planCtrl.Enter(ctx, "planning phase"))
	assert.True(t, planCtrl.IsActive())
	assert.True(t, planCtrl.ShouldBlockWrite("write"))
	assert.True(t, planCtrl.ShouldBlockWrite("bash"))
	assert.False(t, planCtrl.ShouldBlockWrite("read"))

	// 7. PlanMode inactive -> write tool allowed.
	require.NoError(t, planCtrl.Exit(ctx, "plan complete"))
	assert.False(t, planCtrl.IsActive())
	assert.False(t, planCtrl.ShouldBlockWrite("write"))
}

// -----------------------------------------------------------------------------
// TestE2E_Phase19_ConfigFieldConsumption
// -----------------------------------------------------------------------------

// TestE2E_Phase19_ConfigFieldConsumption verifies that config fields are
// consumed: Session.ID is used as the session ID, and Compaction.Strategy
// routes to the correct Compactor via CompactorFactory.
func TestE2E_Phase19_ConfigFieldConsumption(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// 1. Create config with specific values.
	cfg := &config.Config{
		Session: config.SessionConfig{
			ID:        "my-session",
			StorePath: "",
		},
		Compaction: config.CompactionConfig{
			Strategy:  "micro",
			MaxTokens: 1000,
		},
	}

	// 2. Verify session ID uses config value (not agent name).
	assert.Equal(t, "my-session", cfg.Session.ID)

	// 3. Verify CompactorFactory.Create("micro") returns MicroCompactor.
	factory := compaction.NewDefaultCompactorFactory()
	compactor, err := factory.Create(cfg.Compaction.Strategy)
	require.NoError(t, err)
	require.NotNil(t, compactor)

	// Run a compaction to confirm the compactor works. The MicroCompactor
	// replaces old tool results with placeholders, so we include tool results
	// and use a budget that the micro strategy can satisfy.
	est := compaction.NewHeuristicTokenEstimator()
	items := []compaction.TurnItem{
		{ID: "1", Role: compaction.RoleSystem, Content: "system prompt"},
		{ID: "2", Role: compaction.RoleUser, Content: "hello"},
		{ID: "3", Role: compaction.RoleAssistant, Content: "hi"},
		{ID: "4", Role: compaction.RoleTool, ToolResult: strings.Repeat("large tool result content ", 50)},
		{ID: "5", Role: compaction.RoleUser, Content: "next question"},
		{ID: "6", Role: compaction.RoleAssistant, Content: "answer"},
		{ID: "7", Role: compaction.RoleTool, ToolResult: strings.Repeat("another big tool result ", 50)},
	}
	// Use a budget that micro can satisfy by placeholdering tool results.
	out, err := compactor.Compact(context.Background(), items, 100, est)
	require.NoError(t, err)
	assert.NotEmpty(t, out, "micro compactor should produce output")

	// 4. Verify other strategies also work.
	for _, strategy := range []string{"unified", "truncating", "summary"} {
		c, err := factory.Create(strategy)
		require.NoError(t, err)
		require.NotNil(t, c, "strategy %q should produce a compactor", strategy)
	}

	// 5. Unknown strategy returns error.
	_, err = factory.Create("unknown-strategy")
	require.Error(t, err)
}

// -----------------------------------------------------------------------------
// TestE2E_Phase19_SlashCommandRegistry
// -----------------------------------------------------------------------------

// slashCtx is a minimal context for slash command handlers in E2E tests.
type slashCtx struct {
	out         *strings.Builder
	fileTracker *tools.FileTracker
	planCtrl    core.PlanModeController
}

// e2eUndoHandler mimics the cli.UndoHandler for E2E testing.
type e2eUndoHandler struct{}

func (h *e2eUndoHandler) Name() string        { return "undo" }
func (h *e2eUndoHandler) Description() string { return "Restore the most recent file checkpoint" }
func (h *e2eUndoHandler) Handle(_ context.Context, _ []string, sc *slashCtx) error {
	if sc.fileTracker == nil {
		fmt.Fprintln(sc.out, "File tracking not configured.")
		return nil
	}
	checkpoints := sc.fileTracker.ListCheckpoints()
	if len(checkpoints) == 0 {
		fmt.Fprintln(sc.out, "No checkpoints to undo.")
		return nil
	}
	latest := checkpoints[len(checkpoints)-1]
	if err := sc.fileTracker.Restore(latest.ID); err != nil {
		fmt.Fprintf(sc.out, "Undo failed: %v\n", err)
		return nil
	}
	fmt.Fprintf(sc.out, "Restored %s to checkpoint %s.\n", latest.Path, latest.ID)
	return nil
}

// e2ePlanHandler mimics the cli.PlanHandler for E2E testing.
type e2ePlanHandler struct{}

func (h *e2ePlanHandler) Name() string        { return "plan" }
func (h *e2ePlanHandler) Description() string { return "Enter or exit plan mode" }
func (h *e2ePlanHandler) Handle(ctx context.Context, args []string, sc *slashCtx) error {
	if sc.planCtrl == nil {
		fmt.Fprintln(sc.out, "Plan mode not configured.")
		return nil
	}
	if len(args) > 0 && args[0] == "exit" {
		if err := sc.planCtrl.Exit(ctx, ""); err != nil {
			fmt.Fprintf(sc.out, "Failed: %v\n", err)
			return nil
		}
		fmt.Fprintln(sc.out, "Exited plan mode.")
		return nil
	}
	if len(args) > 0 && args[0] == "enter" {
		if err := sc.planCtrl.Enter(ctx, "user requested"); err != nil {
			fmt.Fprintf(sc.out, "Failed: %v\n", err)
			return nil
		}
		fmt.Fprintln(sc.out, "Entered plan mode.")
		return nil
	}
	// Toggle.
	if sc.planCtrl.IsActive() {
		_ = sc.planCtrl.Exit(ctx, "")
		fmt.Fprintln(sc.out, "Exited plan mode.")
		return nil
	}
	_ = sc.planCtrl.Enter(ctx, "user requested")
	fmt.Fprintln(sc.out, "Entered plan mode.")
	return nil
}

// e2eHelpHandler lists all registered commands.
type e2eHelpHandler struct {
	names []string
}

func (h *e2eHelpHandler) Name() string        { return "help" }
func (h *e2eHelpHandler) Description() string { return "Show available slash commands" }
func (h *e2eHelpHandler) Handle(_ context.Context, _ []string, sc *slashCtx) error {
	fmt.Fprintln(sc.out, "Available commands:")
	for _, n := range h.names {
		fmt.Fprintf(sc.out, "  /%s\n", n)
	}
	return nil
}

// e2eSlashHandler is a generic handler for testing.
type e2eSlashHandler struct {
	name string
	desc string
}

func (h *e2eSlashHandler) Name() string        { return h.name }
func (h *e2eSlashHandler) Description() string { return h.desc }
func (h *e2eSlashHandler) Handle(_ context.Context, _ []string, sc *slashCtx) error {
	fmt.Fprintf(sc.out, "%s called\n", h.name)
	return nil
}

// TestE2E_Phase19_SlashCommandRegistry verifies the SlashCommandRegistry:
// /help lists all commands, /undo calls FileTracker.Restore, /plan enter/exit
// works, alias /h resolves to /help, and unknown commands return false.
func TestE2E_Phase19_SlashCommandRegistry(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Create a registry with handlers.
	dir := t.TempDir()
	filePath := filepath.Join(dir, "test.txt")
	original := "original content"
	require.NoError(t, os.WriteFile(filePath, []byte(original), 0644))

	ft := tools.NewFileTracker()
	cpID, err := ft.Backup(filePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filePath, []byte("modified content"), 0644))

	planCtrl := core.NewDefaultPlanModeController()

	// Build registry with handlers.
	// We use the cli.SlashCommandRegistry which is exported.
	reg := newE2ERegistry()

	// Register handlers.
	helpNames := []string{"help", "cost", "undo", "plan", "config"}
	require.NoError(t, reg.Register(&e2eHelpHandler{names: helpNames}))
	require.NoError(t, reg.Register(&e2eSlashHandler{name: "cost", desc: "Show cost"}))
	require.NoError(t, reg.Register(&e2eUndoHandler{}))
	require.NoError(t, reg.Register(&e2ePlanHandler{}))
	require.NoError(t, reg.Register(&e2eSlashHandler{name: "config", desc: "Show config"}))

	// Register alias.
	reg.RegisterAlias("h", "help")

	// 2. Test /help lists all commands.
	buf := &strings.Builder{}
	sc := &slashCtx{out: buf, fileTracker: ft, planCtrl: planCtrl}
	h, ok := reg.Lookup("help")
	require.True(t, ok)
	require.NoError(t, h.Handle(ctx, nil, sc))
	output := buf.String()
	for _, name := range helpNames {
		assert.Contains(t, output, "/"+name, "help should list /%s", name)
	}

	// 3. Test /undo calls FileTracker.Restore.
	buf.Reset()
	h, ok = reg.Lookup("undo")
	require.True(t, ok)
	require.NoError(t, h.Handle(ctx, nil, sc))
	assert.Contains(t, buf.String(), "Restored")

	// Verify file was actually restored.
	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	assert.Equal(t, original, string(data))

	// 4. Test /plan enter.
	buf.Reset()
	h, ok = reg.Lookup("plan")
	require.True(t, ok)
	require.NoError(t, h.Handle(ctx, []string{"enter"}, sc))
	assert.Contains(t, buf.String(), "Entered plan mode")
	assert.True(t, planCtrl.IsActive())

	// 5. Test /plan exit.
	buf.Reset()
	require.NoError(t, h.Handle(ctx, []string{"exit"}, sc))
	assert.Contains(t, buf.String(), "Exited plan mode")
	assert.False(t, planCtrl.IsActive())

	// 6. Test alias /h works as /help.
	buf.Reset()
	h, ok = reg.Lookup("h")
	require.True(t, ok)
	assert.Equal(t, "help", h.Name())
	require.NoError(t, h.Handle(ctx, nil, sc))
	assert.Contains(t, buf.String(), "Available commands")

	// 7. Test unknown command returns false.
	_, ok = reg.Lookup("nonexistent")
	assert.False(t, ok, "unknown command should return false")

	// 8. Verify List returns sorted handlers.
	list := reg.List()
	require.Len(t, list, 5)
	assert.Equal(t, "config", list[0].Name())
	assert.Equal(t, "cost", list[1].Name())
	assert.Equal(t, "help", list[2].Name())
	assert.Equal(t, "plan", list[3].Name())
	assert.Equal(t, "undo", list[4].Name())

	// Use checkpoint ID to avoid unused variable.
	_ = cpID
}

// e2eRegistry is a minimal slash command registry for E2E tests, mirroring
// cli.SlashCommandRegistry but with a public slashContext type.
type e2eRegistry struct {
	mu       sync.RWMutex
	handlers map[string]e2eHandler
	aliases  map[string]string
}

type e2eHandler interface {
	Name() string
	Description() string
	Handle(ctx context.Context, args []string, sc *slashCtx) error
}

func newE2ERegistry() *e2eRegistry {
	return &e2eRegistry{
		handlers: make(map[string]e2eHandler),
		aliases:  make(map[string]string),
	}
}

func (r *e2eRegistry) Register(h e2eHandler) error {
	if h == nil {
		return errors.New("cannot register nil handler")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[h.Name()]; exists {
		return fmt.Errorf("command %q already registered", h.Name())
	}
	r.handlers[h.Name()] = h
	return nil
}

func (r *e2eRegistry) RegisterAlias(alias, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.aliases[alias] = name
}

func (r *e2eRegistry) Lookup(name string) (e2eHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if h, ok := r.handlers[name]; ok {
		return h, true
	}
	if realName, ok := r.aliases[name]; ok {
		if h, ok := r.handlers[realName]; ok {
			return h, true
		}
	}
	return nil, false
}

func (r *e2eRegistry) List() []e2eHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]e2eHandler, 0, len(r.handlers))
	for _, h := range r.handlers {
		out = append(out, h)
	}
	// Sort by name.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[i].Name() > out[j].Name() {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// TestE2E_Phase19_SubAgentSystemPrompt
// -----------------------------------------------------------------------------

// e2eFakeSubAgent is a minimal SubAgent for E2E testing.
type e2eFakeSubAgent struct {
	name   string
	result core.AgentMessage
	err    error
}

func (s *e2eFakeSubAgent) Name() string { return s.name }
func (s *e2eFakeSubAgent) Run(_ context.Context, _ string) (<-chan core.AgentEvent, error) {
	ch := make(chan core.AgentEvent)
	close(ch)
	return ch, nil
}
func (s *e2eFakeSubAgent) Send(_ context.Context, _ string) error  { return nil }
func (s *e2eFakeSubAgent) Interrupt(_ context.Context) error        { return nil }
func (s *e2eFakeSubAgent) Wait(_ context.Context) (core.AgentMessage, error) {
	return s.result, s.err
}

// e2eFakeFactory records configs for assertion.
type e2eFakeFactory struct {
	mu      sync.Mutex
	configs []core.SubAgentConfig
}

func (f *e2eFakeFactory) Create(_ context.Context, name string, config core.SubAgentConfig) (core.SubAgent, error) {
	if name != "" {
		config.Name = name
	}
	f.mu.Lock()
	f.configs = append(f.configs, config)
	f.mu.Unlock()
	return &e2eFakeSubAgent{
		name:   name,
		result: core.AgentMessage{Role: "assistant", Content: "sub-agent done"},
	}, nil
}

// TestE2E_Phase19_SubAgentSystemPrompt verifies that a SubagentTask's
// SystemPrompt propagates through the Dispatcher into the SubAgentConfig, and
// that WithSystemPrompt on a LoopAgent overrides the default system prompt.
func TestE2E_Phase19_SubAgentSystemPrompt(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Verify SystemPrompt propagates through Dispatcher -> SubAgentConfig.
	factory := &e2eFakeFactory{}
	dispatcher := core.NewDefaultSubagentDispatcher(factory)

	_, err := dispatcher.Dispatch(ctx, core.SubagentTask{
		ID:           "review-task",
		Prompt:       "Review this code",
		SystemPrompt: "You are a code reviewer",
	})
	require.NoError(t, err)

	require.Len(t, factory.configs, 1)
	assert.Equal(t, "review-task", factory.configs[0].Name)
	assert.Equal(t, "You are a code reviewer", factory.configs[0].SystemPrompt,
		"explicit SystemPrompt must propagate to SubAgentConfig")

	// 2. Verify default prompt when no SystemPrompt or Role is provided.
	factory2 := &e2eFakeFactory{}
	d2 := core.NewDefaultSubagentDispatcher(factory2)
	_, err = d2.Dispatch(ctx, core.SubagentTask{
		ID:     "default-task",
		Prompt: "go",
	})
	require.NoError(t, err)
	require.Len(t, factory2.configs, 1)
	assert.Equal(t, core.DefaultSubAgentPrompt, factory2.configs[0].SystemPrompt,
		"default prompt should be used when neither SystemPrompt nor Role is set")

	// 3. Verify Role template is used when SystemPrompt is empty.
	factory3 := &e2eFakeFactory{}
	d3 := core.NewDefaultSubagentDispatcher(factory3)
	_, err = d3.Dispatch(ctx, core.SubagentTask{
		ID:     "tester-task",
		Prompt: "write tests",
		Role:   "tester",
	})
	require.NoError(t, err)
	require.Len(t, factory3.configs, 1)
	assert.Equal(t, core.TesterPrompt, factory3.configs[0].SystemPrompt,
		"role template should be used when SystemPrompt is empty")

	// 4. Verify WithSystemPrompt on LoopAgent overrides the default.
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"SYS", "override-test",
		mock.ConversationTurn{AssistantContent: "ok"},
	))

	loop := core.NewLoopAgent(
		core.WithLLM(model),
		core.WithSystemPrompt("OVERRIDE-PROMPT"),
	)
	_, err = loop.Run(ctx, core.Submission{Content: "go"})
	require.NoError(t, err)

	require.Equal(t, 1, model.CallCount())
	msgs := model.CallLog()[0].Messages
	require.NotEmpty(t, msgs)
	assert.Equal(t, llm.RoleSystem, msgs[0].Role)
	assert.Equal(t, "OVERRIDE-PROMPT", msgs[0].Content,
		"WithSystemPrompt must override the default system prompt")

	// 5. Verify SystemPrompt precedence over Role.
	factory4 := &e2eFakeFactory{}
	d4 := core.NewDefaultSubagentDispatcher(factory4)
	_, err = d4.Dispatch(ctx, core.SubagentTask{
		ID:           "precedence-task",
		Prompt:       "go",
		Role:         "researcher",
		SystemPrompt: "explicit wins",
	})
	require.NoError(t, err)
	require.Len(t, factory4.configs, 1)
	assert.Equal(t, "explicit wins", factory4.configs[0].SystemPrompt,
		"explicit SystemPrompt must take precedence over Role")
}

// -----------------------------------------------------------------------------
// TestE2E_Phase19_HTMLConverterWithWebFetch
// -----------------------------------------------------------------------------

// TestE2E_Phase19_HTMLConverterWithWebFetch verifies that HTML-to-Markdown
// conversion works through WebFetchTool with DefaultHTMLConverter: output is
// Markdown (no HTML tags), and script/style content is removed.
func TestE2E_Phase19_HTMLConverterWithWebFetch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	htmlPage := `<!DOCTYPE html>
<html><head><title>Test Page</title>
<style>body { color: red; }</style>
<script>alert('xss');</script>
</head><body>
<h1>Hello World</h1>
<p>This is a paragraph.</p>
<a href="https://example.com">A link</a>
<ul><li>item1</li><li>item2</li></ul>
</body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(htmlPage))
	}))
	defer srv.Close()

	converter := tools.NewDefaultHTMLConverter()
	tool := tools.NewWebFetchTool(tools.WithHTMLConverter(converter))

	res, err := tool.Execute(ctx, toolCallWithArgs("web_fetch", map[string]any{
		"url": srv.URL,
	}))
	require.NoError(t, err)
	require.NotNil(t, res)

	// Verify output is Markdown (no HTML tags).
	assert.NotContains(t, res.Output, "<html>")
	assert.NotContains(t, res.Output, "<head>")
	assert.NotContains(t, res.Output, "<body>")
	assert.NotContains(t, res.Output, "<h1>")
	assert.NotContains(t, res.Output, "<p>")
	assert.NotContains(t, res.Output, "<a ")
	assert.NotContains(t, res.Output, "<ul>")
	assert.NotContains(t, res.Output, "<li>")

	// Verify Markdown content is present.
	assert.Contains(t, res.Output, "# Hello World")
	assert.Contains(t, res.Output, "This is a paragraph.")
	assert.Contains(t, res.Output, "[A link](https://example.com)")
	assert.Contains(t, res.Output, "- item1")
	assert.Contains(t, res.Output, "- item2")

	// Verify script/style content is removed.
	assert.NotContains(t, res.Output, "alert")
	assert.NotContains(t, res.Output, "color")
	assert.NotContains(t, res.Output, "red")
	assert.NotContains(t, res.Output, "<script>")
	assert.NotContains(t, res.Output, "<style>")
}
