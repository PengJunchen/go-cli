package tools

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- GoalCreateTool ---

func TestGoalCreateToolName(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalCreateTool(store)
	assert.Equal(t, "goal_create", tool.Name())
}

func TestGoalCreateToolDescription(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalCreateTool(store)
	assert.Contains(t, tool.Description(), "goal_create")
}

func TestGoalCreateToolExecute(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalCreateTool(store)

	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-1",
		Name: "goal_create",
		Args: map[string]any{
			"title":            "ship feature",
			"description":      "implement and deploy",
			"success_criteria": "all tests pass in production",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "created goal")
	assert.Contains(t, res.Output, "ship feature")
	assert.Equal(t, "call-1", res.ToolCallID)
	assert.NotEmpty(t, res.Metadata["id"])
	assert.Equal(t, "ship feature", res.Metadata["title"])
	assert.Equal(t, "draft", res.Metadata["status"])

	// Verify the goal was actually stored.
	goals, _ := store.List(context.Background()) //nolint:errcheck
	require.Len(t, goals, 1)
	assert.Equal(t, "ship feature", goals[0].Title)
}

func TestGoalCreateToolMissingTitle(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalCreateTool(store)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"description": "no title"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'title'")
}

func TestGoalCreateToolNilStore(t *testing.T) {
	tool := &GoalCreateTool{store: nil}
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"title": "test"},
	})
	assert.Error(t, err)
}

// --- GoalUpdateTool ---

func TestGoalUpdateToolName(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalUpdateTool(store)
	assert.Equal(t, "goal_update", tool.Name())
}

func TestGoalUpdateToolExecute(t *testing.T) {
	store, _ := NewDefaultGoalStore("")                                            //nolint:errcheck
	goal, _ := store.Create(context.Background(), "old title", "desc", "criteria") //nolint:errcheck

	tool := NewGoalUpdateTool(store)
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-2",
		Name: "goal_update",
		Args: map[string]any{
			"id":     goal.ID,
			"status": "active",
			"title":  "new title",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "updated goal")
	assert.Contains(t, res.Output, "new title")
	assert.Contains(t, res.Output, "active")
	assert.Equal(t, "call-2", res.ToolCallID)

	g, _ := store.Get(context.Background(), goal.ID) //nolint:errcheck
	assert.Equal(t, GoalStatusActive, g.Status)
	assert.Equal(t, "new title", g.Title)
}

func TestGoalUpdateToolInvalidStatus(t *testing.T) {
	store, _ := NewDefaultGoalStore("")                                        //nolint:errcheck
	goal, _ := store.Create(context.Background(), "title", "desc", "criteria") //nolint:errcheck

	tool := NewGoalUpdateTool(store)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"id":     goal.ID,
			"status": "invalid_status",
		},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid status")
}

func TestGoalUpdateToolMissingID(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalUpdateTool(store)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"status": "active"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'id'")
}

func TestGoalUpdateToolNilStore(t *testing.T) {
	tool := &GoalUpdateTool{store: nil}
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"id": "goal_1"},
	})
	assert.Error(t, err)
}

// --- GoalListTool ---

func TestGoalListToolName(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalListTool(store, NewTaskStore())
	assert.Equal(t, "goal_list", tool.Name())
}

func TestGoalListToolExecute(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	taskStore := NewTaskStore()

	g1, _ := store.Create(context.Background(), "goal A", "", "") //nolint:errcheck
	_, _ = store.Create(context.Background(), "goal B", "", "")   //nolint:errcheck

	// Associate tasks with g1 for progress calculation.
	_ = store.AddTask(context.Background(), g1.ID, "task-1") //nolint:errcheck
	_ = store.AddTask(context.Background(), g1.ID, "task-2") //nolint:errcheck
	taskStore.Create("task 1", "")
	taskStore.Create("task 2", "")
	_ = taskStore.Update("task-1", WithTaskStatus(StatusCompleted)) //nolint:errcheck

	tool := NewGoalListTool(store, taskStore)
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-3",
		Name: "goal_list",
		Args: map[string]any{},
	})
	require.NoError(t, err)
	lines := strings.Split(res.Output, "\n")
	assert.Len(t, lines, 2)
	assert.Contains(t, res.Output, "goal A")
	assert.Contains(t, res.Output, "goal B")
	assert.Contains(t, res.Output, "50%")
	assert.Equal(t, "call-3", res.ToolCallID)
	assert.Equal(t, 2, res.Metadata["count"])
}

func TestGoalListToolEmpty(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalListTool(store, NewTaskStore())

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Empty(t, res.Output)
	assert.Equal(t, 0, res.Metadata["count"])
}

func TestGoalListToolNilStore(t *testing.T) {
	tool := &GoalListTool{store: nil}
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
}

// --- GoalGetTool ---

func TestGoalGetToolName(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalGetTool(store, NewTaskStore())
	assert.Equal(t, "goal_get", tool.Name())
}

func TestGoalGetToolExecute(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	taskStore := NewTaskStore()

	goal, _ := store.Create(context.Background(), "my goal", "a description", "success criteria") //nolint:errcheck
	_ = store.AddTask(context.Background(), goal.ID, "task-1")                                    //nolint:errcheck
	taskStore.Create("task 1", "desc")

	tool := NewGoalGetTool(store, taskStore)
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-4",
		Name: "goal_get",
		Args: map[string]any{"id": goal.ID},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "my goal")
	assert.Contains(t, res.Output, "a description")
	assert.Contains(t, res.Output, "success criteria")
	assert.Contains(t, res.Output, "task 1")
	assert.Equal(t, "call-4", res.ToolCallID)
	assert.Equal(t, goal.ID, res.Metadata["id"])
	assert.Equal(t, 1, res.Metadata["task_count"])
}

func TestGoalGetToolNotFound(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalGetTool(store, NewTaskStore())

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"id": "nonexistent"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGoalGetToolMissingID(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	tool := NewGoalGetTool(store, NewTaskStore())

	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'id'")
}

func TestGoalGetToolNilStore(t *testing.T) {
	tool := &GoalGetTool{store: nil}
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"id": "goal_1"},
	})
	assert.Error(t, err)
}

// --- TaskUpdateTool ---

func TestTaskUpdateToolName(t *testing.T) {
	tool := NewTaskUpdateTool(NewTaskStore())
	assert.Equal(t, "task_update", tool.Name())
}

func TestTaskUpdateToolDescription(t *testing.T) {
	tool := NewTaskUpdateTool(NewTaskStore())
	assert.Contains(t, tool.Description(), "task_update")
}

func TestTaskUpdateToolExecuteStatus(t *testing.T) {
	store := NewTaskStore()
	created := store.Create("my task", "desc")

	tool := NewTaskUpdateTool(store)
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-5",
		Name: "task_update",
		Args: map[string]any{
			"id":     created.ID,
			"status": "in_progress",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "updated task")
	assert.Contains(t, res.Output, "in_progress")
	assert.Equal(t, "call-5", res.ToolCallID)

	task, _ := store.Get(created.ID)
	assert.Equal(t, StatusInProgress, task.Status)
}

func TestTaskUpdateToolExecuteBlocked(t *testing.T) {
	store := NewTaskStore()
	created := store.Create("blocked task", "desc")

	tool := NewTaskUpdateTool(store)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"id":     created.ID,
			"status": "blocked",
		},
	})
	require.NoError(t, err)

	task, _ := store.Get(created.ID)
	assert.Equal(t, StatusBlocked, task.Status)
}

func TestTaskUpdateToolExecuteCancelled(t *testing.T) {
	store := NewTaskStore()
	created := store.Create("canceled task", "desc")

	tool := NewTaskUpdateTool(store)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"id":     created.ID,
			"status": "cancelled", //nolint:misspell
		},
	})
	require.NoError(t, err)

	task, _ := store.Get(created.ID)
	assert.Equal(t, StatusCancelled, task.Status)
}

func TestTaskUpdateToolExecuteTitleAndDescription(t *testing.T) {
	store := NewTaskStore()
	created := store.Create("old title", "old desc")

	tool := NewTaskUpdateTool(store)
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"id":          created.ID,
			"title":       "new title",
			"description": "new desc",
		},
	})
	require.NoError(t, err)

	task, _ := store.Get(created.ID)
	assert.Equal(t, "new title", task.Title)
	assert.Equal(t, "new desc", task.Description)
}

func TestTaskUpdateToolNotFound(t *testing.T) {
	tool := NewTaskUpdateTool(NewTaskStore())
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"id": "nonexistent", "status": "completed"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestTaskUpdateToolMissingID(t *testing.T) {
	tool := NewTaskUpdateTool(NewTaskStore())
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"status": "completed"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'id'")
}

func TestTaskUpdateToolNilStore(t *testing.T) {
	tool := &TaskUpdateTool{store: nil}
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"id": "task-1"},
	})
	assert.Error(t, err)
}

// --- Interface compliance ---

func TestGoalToolsSatisfyInterface(t *testing.T) {
	var _ ToolDefinition = (*GoalCreateTool)(nil)
	var _ ToolDefinition = (*GoalUpdateTool)(nil)
	var _ ToolDefinition = (*GoalListTool)(nil)
	var _ ToolDefinition = (*GoalGetTool)(nil)
	var _ ToolDefinition = (*TaskUpdateTool)(nil)
}

// --- Concurrency ---

func TestGoalToolsConcurrent(t *testing.T) {
	store, _ := NewDefaultGoalStore("") //nolint:errcheck
	taskStore := NewTaskStore()

	createTool := NewGoalCreateTool(store)
	listTool := NewGoalListTool(store, taskStore)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = createTool.Execute(context.Background(), ToolCall{ //nolint:errcheck
				Args: map[string]any{"title": "concurrent goal"},
			})
			_, _ = listTool.Execute(context.Background(), ToolCall{ //nolint:errcheck
				Args: map[string]any{},
			})
		}()
	}
	wg.Wait()

	goals, _ := store.List(context.Background()) //nolint:errcheck
	assert.Len(t, goals, 20)
}
