package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// TaskCreateTool creates a new task in the bound TaskStore. It implements the
// ToolDefinition interface.
type TaskCreateTool struct {
	store *TaskStore
}

var _ ToolDefinition = (*TaskCreateTool)(nil)

// NewTaskCreateTool returns a TaskCreateTool backed by the given TaskStore.
func NewTaskCreateTool(store *TaskStore) *TaskCreateTool {
	return &TaskCreateTool{store: store}
}

// Name returns the tool name.
func (t *TaskCreateTool) Name() string { return "task_create" }

// Description returns a brief description of the tool.
func (t *TaskCreateTool) Description() string {
	return "task_create: creates a new task. Args: title (string), description (string)."
}

// Execute creates a task using args["title"] and args["description"].
func (t *TaskCreateTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t.store == nil {
		return nil, errors.New("task_create: nil store")
	}

	title, ok := call.Args["title"].(string)
	if !ok || strings.TrimSpace(title) == "" {
		slog.Debug("task_create.missing_title")
		return nil, errors.New("task_create: missing string argument 'title'")
	}
	description, _ := call.Args["description"].(string)

	task := t.store.Create(title, description)

	slog.Debug("task_create.done", "id", task.ID, "title", title)

	return &ToolResult{
		Output:     fmt.Sprintf("created task %s: %s", task.ID, task.Title),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"id":          task.ID,
			"title":       task.Title,
			"description": task.Description,
			"status":      task.Status,
		},
	}, nil
}

// TaskGetTool retrieves a task by ID from the bound TaskStore. It implements
// the ToolDefinition interface.
type TaskGetTool struct {
	store *TaskStore
}

var _ ToolDefinition = (*TaskGetTool)(nil)

// NewTaskGetTool returns a TaskGetTool backed by the given TaskStore.
func NewTaskGetTool(store *TaskStore) *TaskGetTool {
	return &TaskGetTool{store: store}
}

// Name returns the tool name.
func (t *TaskGetTool) Name() string { return "task_get" }

// Description returns a brief description of the tool.
func (t *TaskGetTool) Description() string {
	return "task_get: retrieves a task by ID. Args: id (string)."
}

// Execute fetches the task identified by args["id"].
func (t *TaskGetTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t.store == nil {
		return nil, errors.New("task_get: nil store")
	}

	id, ok := call.Args["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		slog.Debug("task_get.missing_id")
		return nil, errors.New("task_get: missing string argument 'id'")
	}

	task, found := t.store.Get(id)
	if !found {
		slog.Debug("task_get.not_found", "id", id)
		return nil, fmt.Errorf("task_get: task %q not found", id)
	}

	slog.Debug("task_get.done", "id", id)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ID: %s\n", task.ID))
	sb.WriteString(fmt.Sprintf("Title: %s\n", task.Title))
	sb.WriteString(fmt.Sprintf("Description: %s\n", task.Description))
	sb.WriteString(fmt.Sprintf("Status: %s\n", task.Status))

	return &ToolResult{
		Output:     strings.TrimSuffix(sb.String(), "\n"),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"id":     task.ID,
			"title":  task.Title,
			"status": task.Status,
		},
	}, nil
}

// TaskListTool lists all tasks in the bound TaskStore. It implements the
// ToolDefinition interface.
type TaskListTool struct {
	store *TaskStore
}

var _ ToolDefinition = (*TaskListTool)(nil)

// NewTaskListTool returns a TaskListTool backed by the given TaskStore.
func NewTaskListTool(store *TaskStore) *TaskListTool {
	return &TaskListTool{store: store}
}

// Name returns the tool name.
func (t *TaskListTool) Name() string { return "task_list" }

// Description returns a brief description of the tool.
func (t *TaskListTool) Description() string {
	return "task_list: lists all tasks with their ID, title, and status."
}

// Execute returns a formatted list of all tasks.
func (t *TaskListTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t.store == nil {
		return nil, errors.New("task_list: nil store")
	}

	tasks := t.store.List()

	var sb strings.Builder
	for _, task := range tasks {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", task.Status, task.ID, task.Title))
	}

	slog.Debug("task_list.done", "count", len(tasks))

	return &ToolResult{
		Output:     strings.TrimSuffix(sb.String(), "\n"),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"count": len(tasks),
		},
	}, nil
}
