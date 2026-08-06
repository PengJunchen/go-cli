package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTodoWriteName(t *testing.T) {
	tool := NewTodoWriteTool(NewTodoStore())
	assert.Equal(t, "todo_write", tool.Name())
}

func TestTodoWriteDescription(t *testing.T) {
	tool := NewTodoWriteTool(NewTodoStore())
	assert.Contains(t, tool.Description(), "todo_write")
	assert.Contains(t, tool.Description(), "action")
}

func TestTodoWriteAdd(t *testing.T) {
	store := NewTodoStore()
	tool := NewTodoWriteTool(store)

	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-1",
		Name: "todo_write",
		Args: map[string]any{
			"action":   "add",
			"content":  "write tests",
			"priority": "high",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "added todo")
	assert.Contains(t, res.Output, "write tests")
	assert.Equal(t, "call-1", res.ToolCallID)

	items := store.List()
	require.Len(t, items, 1)
	assert.Equal(t, "write tests", items[0].Content)
	assert.Equal(t, "high", items[0].Priority)
	assert.Equal(t, "pending", items[0].Status)
}

func TestTodoWriteAddMissingContent(t *testing.T) {
	tool := NewTodoWriteTool(NewTodoStore())
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"action": "add"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "'content' is required")
}

func TestTodoWriteUpdate(t *testing.T) {
	store := NewTodoStore()
	store.Add(TodoItem{Content: "task A"})

	tool := NewTodoWriteTool(store)
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-2",
		Name: "todo_write",
		Args: map[string]any{
			"action": "update",
			"id":     "todo-1",
			"status": "completed",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "updated todo todo-1 to completed")

	items := store.List()
	require.Len(t, items, 1)
	assert.Equal(t, "completed", items[0].Status)
}

func TestTodoWriteUpdateMissingID(t *testing.T) {
	tool := NewTodoWriteTool(NewTodoStore())
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"action": "update", "status": "completed"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "'id' is required")
}

func TestTodoWriteUpdateMissingStatus(t *testing.T) {
	tool := NewTodoWriteTool(NewTodoStore())
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"action": "update", "id": "todo-1"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "'status' is required")
}

func TestTodoWriteList(t *testing.T) {
	store := NewTodoStore()
	store.Add(TodoItem{Content: "task A", Priority: "high"})
	store.Add(TodoItem{Content: "task B", Priority: "low"})

	tool := NewTodoWriteTool(store)
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-3",
		Name: "todo_write",
		Args: map[string]any{"action": "list"},
	})
	require.NoError(t, err)
	lines := strings.Split(res.Output, "\n")
	assert.Len(t, lines, 2)
	assert.Contains(t, res.Output, "task A")
	assert.Contains(t, res.Output, "task B")
	assert.Equal(t, 2, res.Metadata["count"])
}

func TestTodoWriteListEmpty(t *testing.T) {
	tool := NewTodoWriteTool(NewTodoStore())
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"action": "list"},
	})
	require.NoError(t, err)
	assert.Empty(t, res.Output)
	assert.Equal(t, 0, res.Metadata["count"])
}

func TestTodoWriteRemove(t *testing.T) {
	store := NewTodoStore()
	store.Add(TodoItem{Content: "task A"})

	tool := NewTodoWriteTool(store)
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-4",
		Name: "todo_write",
		Args: map[string]any{"action": "remove", "id": "todo-1"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "removed todo todo-1")
	assert.Empty(t, store.List())
}

func TestTodoWriteRemoveMissingID(t *testing.T) {
	tool := NewTodoWriteTool(NewTodoStore())
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"action": "remove"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "'id' is required")
}

func TestTodoWriteUnknownAction(t *testing.T) {
	tool := NewTodoWriteTool(NewTodoStore())
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"action": "bogus"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown action")
}

func TestTodoWriteMissingAction(t *testing.T) {
	tool := NewTodoWriteTool(NewTodoStore())
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'action'")
}

func TestTodoWriteNilStore(t *testing.T) {
	tool := &TodoWriteTool{store: nil}
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"action": "list"},
	})
	assert.Error(t, err)
}
