// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 22 agent capabilities wiring: real SubAgent
// execution, unconnected tool registration, and plan-mode blocking.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// =============================================================================
// Phase 22 E2E: Agent Capabilities Wiring
// =============================================================================

// registerRealSubAgentFactory registers a real SubAgent factory globally and
// restores the previous factory on test cleanup, preventing global state leak.
func registerRealSubAgentFactory(t *testing.T, model llm.BaseChatModel) {
	t.Helper()
	orig := core.GetSubAgentFactory()
	t.Cleanup(func() { core.RegisterSubAgentFactory(orig) })
	core.RegisterSubAgentFactory(core.NewRealSubAgentFactory(model, nil, tools.NewDefaultToolRegistry()))
}

// --- AC-1: SubAgent real execution ---

// TestE2E_Phase22_SubAgentReturnsRealLLMResponse verifies that dispatch_subagent
// returns a genuine LLM response instead of the simulated "response-1" string.
func TestE2E_Phase22_SubAgentReturnsRealLLMResponse(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"sys", "e2e-subagent-real",
		mock.ConversationTurn{AssistantContent: "real subagent analysis"},
	))
	registerRealSubAgentFactory(t, model)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dispatcher := core.NewDefaultSubagentDispatcher(nil)
	subTool := core.NewSubagentTool(dispatcher)

	result, err := subTool.Execute(ctx, tools.ToolCall{
		ID:   "tc-1",
		Name: "dispatch_subagent",
		Args: map[string]any{
			"prompt":    "analyze the codebase",
			"id":        "e2e-real-test",
			"max_turns": 3,
		},
	})
	require.NoError(t, err)
	assert.NotEqual(t, "response-1", result.Output)
	assert.False(t, strings.HasPrefix(result.Output, "response-"))
	assert.Equal(t, "real subagent analysis", result.Output)
}

// --- AC-2: SubAgent independent context ---

// TestE2E_Phase22_SubAgentIndependentContext verifies that two sub-agent
// dispatches have independent LLM conversation histories. The second dispatch's
// LLM call must NOT contain the first dispatch's prompt in its messages.
func TestE2E_Phase22_SubAgentIndependentContext(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"sys", "e2e-subagent-independent",
		mock.ConversationTurn{AssistantContent: "first independent answer"},
		mock.ConversationTurn{AssistantContent: "second independent answer"},
	))
	registerRealSubAgentFactory(t, model)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dispatcher := core.NewDefaultSubagentDispatcher(nil)
	subTool := core.NewSubagentTool(dispatcher)

	r1, err := subTool.Execute(ctx, tools.ToolCall{
		ID:   "tc-1",
		Name: "dispatch_subagent",
		Args: map[string]any{"prompt": "question one", "id": "sub-1", "max_turns": 3},
	})
	require.NoError(t, err)
	assert.Equal(t, "first independent answer", r1.Output)

	r2, err := subTool.Execute(ctx, tools.ToolCall{
		ID:   "tc-2",
		Name: "dispatch_subagent",
		Args: map[string]any{"prompt": "question two", "id": "sub-2", "max_turns": 3},
	})
	require.NoError(t, err)
	assert.Equal(t, "second independent answer", r2.Output)
	assert.NotEqual(t, r1.Output, r2.Output)

	// Verify context independence: the second LLM call must NOT contain the
	// first dispatch's prompt ("question one") in its message history.
	logs := model.CallLog()
	require.Len(t, logs, 2, "expected exactly 2 LLM calls")
	secondCallMsgs := logs[1].Messages
	for _, msg := range secondCallMsgs {
		assert.NotContains(t, msg.Content, "question one",
			"second dispatch's LLM history must not contain first dispatch's prompt")
	}
}

// --- AC-3: todo/task tools available and working ---

// TestE2E_Phase22_TodoWriteToolWorks verifies that todo_write tool is registered
// and can add/list todo items.
func TestE2E_Phase22_TodoWriteToolWorks(t *testing.T) {
	tr := tools.NewDefaultToolRegistry()
	todoStore := tools.NewTodoStore()
	require.NoError(t, tr.Register(context.Background(), tools.NewTodoWriteTool(todoStore)))

	todoTool, err := tr.Get(context.Background(), "todo_write")
	require.NoError(t, err)

	// Add a todo item.
	addResult, err := todoTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-add",
		Name: "todo_write",
		Args: map[string]any{"action": "add", "content": "write E2E tests", "priority": "high"},
	})
	require.NoError(t, err)
	assert.Contains(t, addResult.Output, "write E2E tests")

	// List todos.
	listResult, err := todoTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-list",
		Name: "todo_write",
		Args: map[string]any{"action": "list"},
	})
	require.NoError(t, err)
	assert.Contains(t, listResult.Output, "write E2E tests")
}

// TestE2E_Phase22_TaskToolsWork verifies that task_create, task_get, and
// task_list tools are registered and functional.
func TestE2E_Phase22_TaskToolsWork(t *testing.T) {
	tr := tools.NewDefaultToolRegistry()
	taskStore := tools.NewTaskStore()
	require.NoError(t, tr.Register(context.Background(), tools.NewTaskCreateTool(taskStore)))
	require.NoError(t, tr.Register(context.Background(), tools.NewTaskGetTool(taskStore)))
	require.NoError(t, tr.Register(context.Background(), tools.NewTaskListTool(taskStore)))

	// Create a task.
	createTool, err := tr.Get(context.Background(), "task_create")
	require.NoError(t, err)
	createResult, err := createTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-create",
		Name: "task_create",
		Args: map[string]any{"title": "E2E verification", "description": "verify task tools"},
	})
	require.NoError(t, err)
	assert.Contains(t, createResult.Output, "E2E verification")
	taskID := createResult.Metadata["id"].(string) //nolint:errcheck
	assert.NotEmpty(t, taskID)

	// Get the task.
	getTool, err := tr.Get(context.Background(), "task_get")
	require.NoError(t, err)
	getResult, err := getTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-get",
		Name: "task_get",
		Args: map[string]any{"id": taskID},
	})
	require.NoError(t, err)
	assert.Contains(t, getResult.Output, "E2E verification")

	// List tasks.
	listTool, err := tr.Get(context.Background(), "task_list")
	require.NoError(t, err)
	listResult, err := listTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-list",
		Name: "task_list",
	})
	require.NoError(t, err)
	assert.Contains(t, listResult.Output, taskID)
}

// --- AC-4: web_fetch tool available ---

// TestE2E_Phase22_WebFetchToolAvailable verifies that web_fetch is registered
// and can be retrieved from the registry.
func TestE2E_Phase22_WebFetchToolAvailable(t *testing.T) {
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), tools.NewWebFetchTool()))

	tool, err := tr.Get(context.Background(), "web_fetch")
	require.NoError(t, err)
	assert.Equal(t, "web_fetch", tool.Name())
	assert.NotEmpty(t, tool.Description())
}

// TestE2E_Phase22_WebSearchToolAvailable verifies that web_search is registered.
func TestE2E_Phase22_WebSearchToolAvailable(t *testing.T) {
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), tools.NewWebSearchTool()))

	tool, err := tr.Get(context.Background(), "web_search")
	require.NoError(t, err)
	assert.Equal(t, "web_search", tool.Name())
}

// --- AC-5: ask_user tool available ---

// mockHITLEmitter auto-responds to questions for testing. It relies on the
// buffered ResponseCh (capacity 1) created by the ask_user tool/adapter, so
// the send is non-blocking.
type mockHITLEmitter struct {
	answer string
}

func (e *mockHITLEmitter) Emit(ctx context.Context, event core.HITLQuestionEvent) error {
	select {
	case event.ResponseCh <- core.HITLAnswer{QuestionID: event.QuestionID, Answer: e.answer}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestE2E_Phase22_AskUserToolWorks verifies that ask_user tool is registered
// and can emit a question + receive an answer.
func TestE2E_Phase22_AskUserToolWorks(t *testing.T) {
	emitter := &mockHITLEmitter{answer: "yes, proceed"}
	askTool := core.NewAskUserQuestionTool(emitter, 5*time.Second)

	result, err := askTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-ask",
		Name: "ask_user",
		Args: map[string]any{"question": "Should I proceed?", "options": []string{"yes, proceed", "no, stop"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "yes, proceed", result.Output)
}

// --- AC-6: plan_mode blocks writes ---

// TestE2E_Phase22_PlanModeBlocksWrites verifies prepared dispatch against
// captured Plan Mode capabilities: trusted controls can enter/exit Plan Mode,
// while ordinary tools cannot gain read exemptions from familiar names.
func TestE2E_Phase22_PlanModeBlocksWrites(t *testing.T) {
	ctx := context.Background()
	queue := tools.NewDefaultFileMutationQueue()
	t.Cleanup(func() { require.NoError(t, queue.(*tools.DefaultFileMutationQueue).Close()) })
	base, registry, planCtrl := newPreparedApprovalRuntime(t, &approval.AllowAllClassifier{}, true, nil, queue)
	registerPreparedPlanControls(t, base, planCtrl)

	unknownTools := map[string]*recordingTool{}
	for _, name := range []string{"read", "grep", "ls", "write", "edit", "bash"} {
		tool := &recordingTool{name: name}
		unknownTools[name] = tool
		require.NoError(t, base.Register(ctx, tool))
	}

	writeCall := tools.ToolCall{
		ID:   "write-before-plan",
		Name: "write",
		Args: map[string]any{"path": "/tmp/phase22-plan.txt", "content": "before"},
	}
	writeProxy, err := registry.Get(ctx, "write")
	require.NoError(t, err)
	_, err = writeProxy.Execute(ctx, writeCall)
	require.NoError(t, err, "an explicit auto-approve mode should resolve Ask before entering Plan Mode")
	assert.EqualValues(t, 1, unknownTools["write"].calls.Load())

	enterTool, err := registry.Get(ctx, "enter_plan_mode")
	require.NoError(t, err)
	enterResult, err := enterTool.Execute(ctx, tools.ToolCall{
		ID:   "tc-enter",
		Name: "enter_plan_mode",
		Args: map[string]any{"reason": "researching before changes"},
	})
	require.NoError(t, err)
	assert.Contains(t, enterResult.Output, "plan mode activated")
	assert.True(t, planCtrl.IsActive())

	for _, name := range []string{"read", "grep", "ls", "write", "edit", "bash"} {
		call := planModeFixtureCall(name)
		toolProxy, getErr := registry.Get(ctx, name)
		require.NoError(t, getErr)
		callsBeforeDeny := unknownTools[name].calls.Load()
		_, executeErr := toolProxy.Execute(ctx, call)
		require.ErrorIs(t, executeErr, approval.ErrToolDenied,
			"ordinary %q registration must remain unknown in Plan Mode", name)
		assert.Equal(t, callsBeforeDeny, unknownTools[name].calls.Load(), "%q must be denied before its executor has side effects", name)
	}
	assert.EqualValues(t, 1, unknownTools["write"].calls.Load(), "denied Plan Mode call must not add a write execution")

	exitTool, err := registry.Get(ctx, "exit_plan_mode")
	require.NoError(t, err)
	exitResult, err := exitTool.Execute(ctx, tools.ToolCall{
		ID:   "tc-exit",
		Name: "exit_plan_mode",
		Args: map[string]any{"summary": "plan complete"},
	})
	require.NoError(t, err)
	assert.Contains(t, exitResult.Output, "plan mode deactivated")
	assert.False(t, planCtrl.IsActive())

	writeCall.ID = "write-after-plan"
	_, err = writeProxy.Execute(ctx, writeCall)
	require.NoError(t, err, "the same ordinary tool remains available after leaving Plan Mode")
	assert.EqualValues(t, 2, unknownTools["write"].calls.Load())
}

func planModeFixtureCall(name string) tools.ToolCall {
	args := map[string]any{}
	switch name {
	case "write":
		args["path"] = "/tmp/phase22-plan-denied.txt"
		args["content"] = "must not execute"
	case "edit":
		args["file_path"] = "/tmp/phase22-plan-denied.txt"
		args["old_string"] = "before"
		args["new_string"] = "after"
	}
	return tools.ToolCall{ID: "plan-denied-" + name, Name: name, Args: args}
}

// --- tool_search can find registered tools ---

// TestE2E_Phase22_ToolSearchFindsTools verifies that tool_search can locate
// tools registered in the registry, including the newly wired tools.
func TestE2E_Phase22_ToolSearchFindsTools(t *testing.T) {
	tr := tools.NewDefaultToolRegistry()
	todoStore := tools.NewTodoStore()
	taskStore := tools.NewTaskStore()
	planCtrl := core.NewDefaultPlanModeController()

	registrations := []tools.ToolDefinition{
		tools.NewTodoWriteTool(todoStore),
		tools.NewTaskCreateTool(taskStore),
		tools.NewTaskGetTool(taskStore),
		tools.NewTaskListTool(taskStore),
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool(),
		tools.NewEnterPlanModeTool(planCtrl),
		tools.NewExitPlanModeTool(planCtrl),
	}
	for _, def := range registrations {
		require.NoError(t, tr.Register(context.Background(), def))
	}
	// tool_search registered last so it sees all tools.
	require.NoError(t, tr.Register(context.Background(), tools.NewToolSearchTool(tr)))

	searchTool, err := tr.Get(context.Background(), "tool_search")
	require.NoError(t, err)

	result, err := searchTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-search",
		Name: "tool_search",
		Args: map[string]any{"query": "todo"},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "todo_write")

	// Search for task tools.
	taskResult, err := searchTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-search-task",
		Name: "tool_search",
		Args: map[string]any{"query": "task"},
	})
	require.NoError(t, err)
	assert.Contains(t, taskResult.Output, "task_create")
	assert.Contains(t, taskResult.Output, "task_get")
	assert.Contains(t, taskResult.Output, "task_list")
}

// TestE2E_Phase22_AllToolsRegistered verifies that a registry populated the
// same way interactive.go wires tools contains all expected tool names.
func TestE2E_Phase22_AllToolsRegistered(t *testing.T) {
	tr := tools.NewDefaultToolRegistry()
	todoStore := tools.NewTodoStore()
	taskStore := tools.NewTaskStore()
	planCtrl := core.NewDefaultPlanModeController()
	emitter := &mockHITLEmitter{answer: "ok"}

	extraTools := []tools.ToolDefinition{
		tools.NewTodoWriteTool(todoStore),
		tools.NewTaskCreateTool(taskStore),
		tools.NewTaskGetTool(taskStore),
		tools.NewTaskListTool(taskStore),
		tools.NewWebFetchTool(),
		tools.NewWebSearchTool(),
		core.NewAskUserQuestionTool(emitter, 30*time.Second),
		tools.NewEnterPlanModeTool(planCtrl),
		tools.NewExitPlanModeTool(planCtrl),
	}
	for _, def := range extraTools {
		require.NoError(t, tr.Register(context.Background(), def))
	}
	require.NoError(t, tr.Register(context.Background(), tools.NewToolSearchTool(tr)))

	// Verify all expected tools are present.
	expectedNames := []string{
		"todo_write", "task_create", "task_get", "task_list",
		"web_fetch", "web_search", "ask_user",
		"enter_plan_mode", "exit_plan_mode", "tool_search",
	}
	defs, err := tr.List(context.Background())
	require.NoError(t, err)

	registeredNames := make(map[string]bool)
	for _, d := range defs {
		registeredNames[d.Name()] = true
	}
	for _, name := range expectedNames {
		assert.True(t, registeredNames[name], "tool %q should be registered", name)
	}
}

// TestE2E_Phase22_DispatchSubagentToolRegistered verifies that dispatch_subagent
// can be registered alongside other tools in a single registry and returns a
// real LLM response.
func TestE2E_Phase22_DispatchSubagentToolRegistered(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"sys", "e2e-dispatch-registered",
		mock.ConversationTurn{AssistantContent: "dispatched result"},
	))
	registerRealSubAgentFactory(t, model)
	dispatcher := core.NewDefaultSubagentDispatcher(nil)

	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), core.NewSubagentTool(dispatcher)))

	tool, err := tr.Get(context.Background(), "dispatch_subagent")
	require.NoError(t, err)
	assert.Equal(t, "dispatch_subagent", tool.Name())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "tc-dispatch",
		Name: "dispatch_subagent",
		Args: map[string]any{"prompt": "hello subagent", "id": "e2e-reg", "max_turns": 2},
	})
	require.NoError(t, err)
	assert.Equal(t, "dispatched result", result.Output)
	assert.NotEqual(t, "response-1", result.Output)
}
