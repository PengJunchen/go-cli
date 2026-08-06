package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskCreateToolName(t *testing.T) {
	tool := NewTaskCreateTool(NewTaskStore())
	assert.Equal(t, "task_create", tool.Name())
}

func TestTaskCreateToolDescription(t *testing.T) {
	tool := NewTaskCreateTool(NewTaskStore())
	assert.Contains(t, tool.Description(), "task_create")
}

func TestTaskCreateToolExecute(t *testing.T) {
	store := NewTaskStore()
	tool := NewTaskCreateTool(store)

	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-1",
		Name: "task_create",
		Args: map[string]any{
			"title":       "build feature",
			"description": "implement the new feature",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "created task task-1")
	assert.Contains(t, res.Output, "build feature")
	assert.Equal(t, "call-1", res.ToolCallID)
	assert.Equal(t, "task-1", res.Metadata["id"])

	tasks := store.List()
	require.Len(t, tasks, 1)
	assert.Equal(t, "build feature", tasks[0].Title)
}

func TestTaskCreateToolMissingTitle(t *testing.T) {
	tool := NewTaskCreateTool(NewTaskStore())
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"description": "no title"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'title'")
}

func TestTaskCreateToolNilStore(t *testing.T) {
	tool := &TaskCreateTool{store: nil}
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"title": "test"},
	})
	assert.Error(t, err)
}

func TestTaskGetToolName(t *testing.T) {
	tool := NewTaskGetTool(NewTaskStore())
	assert.Equal(t, "task_get", tool.Name())
}

func TestTaskGetToolDescription(t *testing.T) {
	tool := NewTaskGetTool(NewTaskStore())
	assert.Contains(t, tool.Description(), "task_get")
}

func TestTaskGetToolExecute(t *testing.T) {
	store := NewTaskStore()
	created := store.Create("my task", "a description")

	tool := NewTaskGetTool(store)
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-2",
		Name: "task_get",
		Args: map[string]any{"id": created.ID},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "my task")
	assert.Contains(t, res.Output, "a description")
	assert.Equal(t, "call-2", res.ToolCallID)
	assert.Equal(t, created.ID, res.Metadata["id"])
}

func TestTaskGetToolNotFound(t *testing.T) {
	tool := NewTaskGetTool(NewTaskStore())
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"id": "nonexistent"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestTaskGetToolMissingID(t *testing.T) {
	tool := NewTaskGetTool(NewTaskStore())
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'id'")
}

func TestTaskGetToolNilStore(t *testing.T) {
	tool := &TaskGetTool{store: nil}
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"id": "task-1"},
	})
	assert.Error(t, err)
}

func TestTaskListToolName(t *testing.T) {
	tool := NewTaskListTool(NewTaskStore())
	assert.Equal(t, "task_list", tool.Name())
}

func TestTaskListToolDescription(t *testing.T) {
	tool := NewTaskListTool(NewTaskStore())
	assert.Contains(t, tool.Description(), "task_list")
}

func TestTaskListToolExecute(t *testing.T) {
	store := NewTaskStore()
	store.Create("task A", "")
	store.Create("task B", "")

	tool := NewTaskListTool(store)
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-3",
		Name: "task_list",
		Args: map[string]any{},
	})
	require.NoError(t, err)
	lines := strings.Split(res.Output, "\n")
	assert.Len(t, lines, 2)
	assert.Contains(t, res.Output, "task A")
	assert.Contains(t, res.Output, "task B")
	assert.Equal(t, "call-3", res.ToolCallID)
	assert.Equal(t, 2, res.Metadata["count"])
}

func TestTaskListToolEmpty(t *testing.T) {
	tool := NewTaskListTool(NewTaskStore())
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Empty(t, res.Output)
	assert.Equal(t, 0, res.Metadata["count"])
}

func TestTaskListToolNilStore(t *testing.T) {
	tool := &TaskListTool{store: nil}
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
}
