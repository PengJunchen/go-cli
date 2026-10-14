package core

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

var r0PlanModeReadOnlyToolNames = []string{
	"read", "ls", "grep", "find", "web_search", "web_fetch",
	"git_diff", "git_status", "git_log", "git_branch", "git_blame", "git_remote",
	"lsp_query", "tool_search", "goal_get", "goal_list", "task_get", "task_list",
}

var r0PlanModeControlToolNames = []string{"ask_user", "enter_plan_mode", "exit_plan_mode"}

func TestPlanModeBlocksKnownMutatingBuiltins(t *testing.T) {
	ctx := context.Background()
	controller := NewDefaultPlanModeController()
	require.NoError(t, controller.Enter(ctx, "test plan mode guard"))

	registry, registeredNames := newR0PlanModeExecutionRegistry(t, controller)
	allowedNames := make(map[string]struct{}, len(r0PlanModeReadOnlyToolNames)+len(r0PlanModeControlToolNames))
	for _, name := range r0PlanModeReadOnlyToolNames {
		allowedNames[name] = struct{}{}
	}
	for _, name := range r0PlanModeControlToolNames {
		allowedNames[name] = struct{}{}
	}

	var mutatingNames []string
	for name := range registeredNames {
		if _, allowed := allowedNames[name]; !allowed {
			mutatingNames = append(mutatingNames, name)
		}
	}
	for _, name := range []string{"write", "edit", "bash", "dispatch_subagent", "git_commit", "git_checkout", "git_push", "git_create_branch", "git_merge", "git_stash", "git_stash_pop", "git_reset", "git_revert", "git_fetch", "git_pull", "git_pr_create"} {
		require.Contains(t, mutatingNames, name, "the registered defaults must include each known mutator")
	}
	require.NotEmpty(t, mutatingNames)

	events := runR0PlanModeToolCalls(t, controller, registry, mutatingNames)
	canceled := findEvents(events, "tool_canceled")
	assert.ElementsMatch(t, mutatingNames, canceled, "each registered mutator must be canceled before dispatch")
	for _, name := range mutatingNames {
		assert.Zero(t, registry.ExecutionCount(name), "%s must not reach its executor", name)
	}
}

func TestPlanModePreservesApprovedReadOnlyTools(t *testing.T) {
	ctx := context.Background()
	controller := NewDefaultPlanModeController()
	require.NoError(t, controller.Enter(ctx, "test plan mode reads"))

	registry, registeredNames := newR0PlanModeExecutionRegistry(t, controller)
	allowedNames := append(append([]string(nil), r0PlanModeReadOnlyToolNames...), r0PlanModeControlToolNames...)
	for _, name := range allowedNames {
		require.Contains(t, registeredNames, name, "every R0 read/control builtin must be registered for the middleware test")
	}

	events := runR0PlanModeToolCalls(t, controller, registry, allowedNames)
	assert.Empty(t, findEvents(events, "tool_canceled"), "R0 approved read/control tools must pass the name guard")
	for _, name := range allowedNames {
		assert.EqualValues(t, 1, registry.ExecutionCount(name), "%s must reach its executor", name)
	}
}

func newR0PlanModeExecutionRegistry(t *testing.T, controller PlanModeController) (*r0PlanModeExecutionRegistry, map[string]struct{}) {
	t.Helper()
	ctx := context.Background()
	registered := tools.NewDefaultToolRegistry()
	require.NoError(t, tools.RegisterDefaults(ctx, registered,
		tools.WithRegisteredGitTool(tools.NewDefaultGitTool(t.TempDir())),
		tools.WithRegisteredBashSandbox(tools.AllowAllSandbox{}),
	))

	goalStore, err := tools.NewDefaultGoalStore("")
	require.NoError(t, err)
	taskStore := tools.NewTaskStore()
	for _, def := range []tools.ToolDefinition{
		tools.NewTodoWriteTool(tools.NewTodoStore()),
		tools.NewTaskCreateTool(taskStore),
		tools.NewTaskUpdateTool(taskStore),
		tools.NewGoalCreateTool(goalStore),
		tools.NewGoalUpdateTool(goalStore),
		tools.NewSubagentTool(nil),
		tools.NewGitPRCreateTool(t.TempDir()),
		tools.NewTaskGetTool(taskStore),
		tools.NewTaskListTool(taskStore),
		tools.NewGoalListTool(goalStore, taskStore),
		tools.NewGoalGetTool(goalStore, taskStore),
		NewAskUserQuestionTool(nil, time.Second),
		tools.NewToolSearchTool(registered),
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool(tools.WithSearchProvider(tools.NewMockSearchProvider())),
		tools.NewLSPTool(nil),
		tools.NewEnterPlanModeTool(controller),
		tools.NewExitPlanModeTool(controller),
	} {
		require.NoError(t, registered.Register(ctx, def), "register %s", def.Name())
	}

	defs, err := registered.List(ctx)
	require.NoError(t, err)
	names := make(map[string]struct{}, len(defs))
	executions := make(map[string]*atomic.Int32, len(defs))
	for _, def := range defs {
		names[def.Name()] = struct{}{}
		executions[def.Name()] = &atomic.Int32{}
	}
	return &r0PlanModeExecutionRegistry{inner: registered, executions: executions}, names
}

func runR0PlanModeToolCalls(t *testing.T, controller PlanModeController, registry *r0PlanModeExecutionRegistry, names []string) []AgentEvent {
	t.Helper()
	calls := make([]llm.ToolCall, len(names))
	for i, name := range names {
		calls[i] = llm.ToolCall{
			ID:   fmt.Sprintf("plan-mode-%02d", i),
			Name: name,
			Args: map[string]any{},
		}
	}
	model := &scriptedChatModel{seq: []*llm.Message{
		{Role: llm.RoleAssistant, ToolCalls: calls},
		{Role: llm.RoleAssistant, Content: "plan complete"},
	}}
	loop := NewLoopAgent(
		WithLLM(model),
		WithTools(registry),
		WithExecutionMode(ExecutionModeSequential),
		WithToolInterceptor(NewPlanModeToolInterceptor(controller)),
	)
	loop.WithToolSearchThreshold(len(names) + 1)
	events, err := loop.Run(context.Background(), Submission{Content: "research without executing writes"})
	require.NoError(t, err)
	return events
}

type r0PlanModeExecutionRegistry struct {
	inner      tools.ToolRegistry
	executions map[string]*atomic.Int32
}

func (r *r0PlanModeExecutionRegistry) Register(ctx context.Context, def tools.ToolDefinition) error {
	return r.inner.Register(ctx, def)
}

func (r *r0PlanModeExecutionRegistry) Get(ctx context.Context, name string) (tools.ToolDefinition, error) {
	def, err := r.inner.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return &r0PlanModeExecutionTool{ToolDefinition: def, executions: r.executions[name]}, nil
}

func (r *r0PlanModeExecutionRegistry) List(ctx context.Context) ([]tools.ToolDefinition, error) {
	defs, err := r.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := make([]tools.ToolDefinition, 0, len(defs))
	for _, def := range defs {
		wrapped = append(wrapped, &r0PlanModeExecutionTool{ToolDefinition: def, executions: r.executions[def.Name()]})
	}
	return wrapped, nil
}

func (r *r0PlanModeExecutionRegistry) ExecutionCount(name string) int32 {
	if counter := r.executions[name]; counter != nil {
		return counter.Load()
	}
	return 0
}

type r0PlanModeExecutionTool struct {
	tools.ToolDefinition
	executions *atomic.Int32
}

func (t *r0PlanModeExecutionTool) Execute(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	t.executions.Add(1)
	return &tools.ToolResult{Output: "recorded", ToolCallID: call.ID}, nil
}

func (t *r0PlanModeExecutionTool) Parameters() any {
	if parameterized, ok := t.ToolDefinition.(tools.Parameterized); ok {
		return parameterized.Parameters()
	}
	return nil
}
