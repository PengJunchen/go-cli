package tools //nolint:scan010 // tool action dispatch uses string-based routing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// TodoWriteTool exposes TodoStore operations through the ToolDefinition
// interface. It dispatches on args["action"] to add, update, list, or remove
// todo items.
type TodoWriteTool struct {
	store *TodoStore
}

var _ ToolDefinition = (*TodoWriteTool)(nil)

// NewTodoWriteTool returns a TodoWriteTool backed by the given TodoStore, which
// must be non-nil.
func NewTodoWriteTool(store *TodoStore) *TodoWriteTool {
	return &TodoWriteTool{store: store}
}

// Name returns the tool name.
func (t *TodoWriteTool) Name() string { return "todo_write" }

// Description returns a brief description of the tool.
func (t *TodoWriteTool) Description() string {
	return "todo_write: manages a todo list. Args: action (string: add|update|list|remove), id (string), content (string), status (string), priority (string)."
}

// Execute dispatches a todo action based on args["action"].
func (t *TodoWriteTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t.store == nil {
		return nil, errors.New("todo_write: nil store")
	}

	action, ok := call.Args["action"].(string)
	if !ok || strings.TrimSpace(action) == "" {
		slog.Debug("todo_write.missing_action")
		return nil, errors.New("todo_write: missing string argument 'action'")
	}

	slog.Debug("todo_write.execute", "action", action)

	switch action {
	case "add":
		return t.doAdd(call)
	case "update":
		return t.doUpdate(call)
	case "list":
		return t.doList(call)
	case "remove":
		return t.doRemove(call)
	default:
		return nil, fmt.Errorf("todo_write: unknown action %q", action)
	}
}

// doAdd creates a new todo item.
func (t *TodoWriteTool) doAdd(call ToolCall) (*ToolResult, error) {
	content, _ := call.Args["content"].(string) //nolint:errcheck
	if strings.TrimSpace(content) == "" {
		return nil, errors.New("todo_write: 'content' is required for add action")
	}
	priority, _ := call.Args["priority"].(string) //nolint:errcheck
	if priority == "" {
		priority = "medium"
	}
	status, _ := call.Args["status"].(string) //nolint:errcheck
	if status == "" {
		status = "pending"
	}

	item := TodoItem{
		Content:  content,
		Status:   status,
		Priority: priority,
	}
	t.store.Add(item)

	id := t.lastAddedID()

	slog.Debug("todo_write.added", "id", id, "content", content)

	return &ToolResult{
		Output:     fmt.Sprintf("added todo %s: %s", id, content),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"action":   "add",
			"id":       id,
			"content":  content,
			"priority": priority,
			"status":   status,
		},
	}, nil
}

// doUpdate changes the status of an existing todo item.
func (t *TodoWriteTool) doUpdate(call ToolCall) (*ToolResult, error) {
	id, _ := call.Args["id"].(string) //nolint:errcheck
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("todo_write: 'id' is required for update action")
	}
	status, _ := call.Args["status"].(string) //nolint:errcheck
	if strings.TrimSpace(status) == "" {
		return nil, errors.New("todo_write: 'status' is required for update action")
	}

	t.store.Update(id, status)

	slog.Debug("todo_write.updated", "id", id, "status", status)

	return &ToolResult{
		Output:     fmt.Sprintf("updated todo %s to %s", id, status),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"action": "update",
			"id":     id,
			"status": status,
		},
	}, nil
}

// doList returns all todo items as a formatted string.
func (t *TodoWriteTool) doList(call ToolCall) (*ToolResult, error) {
	items := t.store.List()

	var sb strings.Builder
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s (%s)\n", item.Status, item.ID, item.Content, item.Priority))
	}

	slog.Debug("todo_write.listed", "count", len(items))

	return &ToolResult{
		Output:     strings.TrimSuffix(sb.String(), "\n"),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"action": "list",
			"count":  len(items),
		},
	}, nil
}

// doRemove deletes a todo item by ID.
func (t *TodoWriteTool) doRemove(call ToolCall) (*ToolResult, error) {
	id, _ := call.Args["id"].(string) //nolint:errcheck
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("todo_write: 'id' is required for remove action")
	}

	t.store.Remove(id)

	slog.Debug("todo_write.removed", "id", id)

	return &ToolResult{
		Output:     fmt.Sprintf("removed todo %s", id),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"action": "remove",
			"id":     id,
		},
	}, nil
}

// lastAddedID returns the ID of the most recently added item. The store is
// expected to have at least one item after doAdd calls Add.
func (t *TodoWriteTool) lastAddedID() string {
	items := t.store.List()
	if len(items) == 0 {
		return ""
	}
	return items[len(items)-1].ID
}
