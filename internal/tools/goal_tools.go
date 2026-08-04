package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// parseGoalStatus converts a status string to a GoalStatus. It returns false
// when the string is not a recognized status.
func parseGoalStatus(s string) (GoalStatus, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "draft":
		return GoalStatusDraft, true
	case "active":
		return GoalStatusActive, true
	case "in_progress":
		return GoalStatusInProgress, true
	case "completed":
		return GoalStatusCompleted, true
	case "blocked":
		return GoalStatusBlocked, true
	case "abandoned":
		return GoalStatusAbandoned, true
	default:
		return 0, false
	}
}

// GoalCreateTool creates a new goal in the bound GoalStore. It implements the
// ToolDefinition interface.
type GoalCreateTool struct {
	store GoalStore
}

var _ ToolDefinition = (*GoalCreateTool)(nil)

// NewGoalCreateTool returns a GoalCreateTool backed by the given GoalStore.
func NewGoalCreateTool(store GoalStore) *GoalCreateTool {
	return &GoalCreateTool{store: store}
}

// Name returns the tool name.
func (t *GoalCreateTool) Name() string { return "goal_create" }

// Description returns a brief description of the tool.
func (t *GoalCreateTool) Description() string {
	return "goal_create: creates a new goal. Args: title (string), description (string), success_criteria (string)."
}

// Execute creates a goal using args["title"], args["description"], and
// args["success_criteria"].
func (t *GoalCreateTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t.store == nil {
		return nil, errors.New("goal_create: nil store")
	}

	title, ok := call.Args["title"].(string)
	if !ok || strings.TrimSpace(title) == "" {
		return nil, errors.New("goal_create: missing string argument 'title'")
	}
	description, _ := call.Args["description"].(string)
	criteria, _ := call.Args["success_criteria"].(string)

	goal, err := t.store.Create(ctx, title, description, criteria)
	if err != nil {
		return nil, fmt.Errorf("goal_create: %w", err)
	}

	return &ToolResult{
		Output:     fmt.Sprintf("created goal %s: %s", goal.ID, goal.Title),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"id":               goal.ID,
			"title":            goal.Title,
			"description":      goal.Description,
			"success_criteria": goal.SuccessCriteria,
			"status":           goal.Status.String(),
			"priority":         goal.Priority,
		},
	}, nil
}

// GoalUpdateTool updates a goal in the bound GoalStore. It implements the
// ToolDefinition interface.
type GoalUpdateTool struct {
	store GoalStore
}

var _ ToolDefinition = (*GoalUpdateTool)(nil)

// NewGoalUpdateTool returns a GoalUpdateTool backed by the given GoalStore.
func NewGoalUpdateTool(store GoalStore) *GoalUpdateTool {
	return &GoalUpdateTool{store: store}
}

// Name returns the tool name.
func (t *GoalUpdateTool) Name() string { return "goal_update" }

// Description returns a brief description of the tool.
func (t *GoalUpdateTool) Description() string {
	return "goal_update: updates a goal. Args: id (string), status (string), title (string), description (string), priority (string), success_criteria (string)."
}

// Execute updates the goal identified by args["id"] with any provided fields.
func (t *GoalUpdateTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t.store == nil {
		return nil, errors.New("goal_update: nil store")
	}

	id, ok := call.Args["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return nil, errors.New("goal_update: missing string argument 'id'")
	}

	var opts []GoalUpdateOption
	if v, ok := call.Args["status"].(string); ok && strings.TrimSpace(v) != "" {
		status, parsed := parseGoalStatus(v)
		if !parsed {
			return nil, fmt.Errorf("goal_update: invalid status %q", v)
		}
		opts = append(opts, WithGoalStatus(status))
	}
	if v, ok := call.Args["title"].(string); ok && strings.TrimSpace(v) != "" {
		opts = append(opts, WithGoalTitle(v))
	}
	if v, ok := call.Args["description"].(string); ok {
		opts = append(opts, WithGoalDescription(v))
	}
	if v, ok := call.Args["priority"].(string); ok && strings.TrimSpace(v) != "" {
		opts = append(opts, WithGoalPriority(v))
	}
	if v, ok := call.Args["success_criteria"].(string); ok {
		opts = append(opts, WithGoalSuccessCriteria(v))
	}

	if err := t.store.Update(ctx, id, opts...); err != nil {
		return nil, fmt.Errorf("goal_update: %w", err)
	}

	goal, err := t.store.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("goal_update: %w", err)
	}

	return &ToolResult{
		Output:     fmt.Sprintf("updated goal %s: %s (status=%s)", goal.ID, goal.Title, goal.Status.String()),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"id":     goal.ID,
			"title":  goal.Title,
			"status": goal.Status.String(),
		},
	}, nil
}

// GoalListTool lists all goals in the bound GoalStore with progress. It
// implements the ToolDefinition interface.
type GoalListTool struct {
	store     GoalStore
	taskStore *TaskStore
}

var _ ToolDefinition = (*GoalListTool)(nil)

// NewGoalListTool returns a GoalListTool backed by the given GoalStore and
// TaskStore. The TaskStore is used to compute progress percentages.
func NewGoalListTool(store GoalStore, taskStore *TaskStore) *GoalListTool {
	return &GoalListTool{store: store, taskStore: taskStore}
}

// Name returns the tool name.
func (t *GoalListTool) Name() string { return "goal_list" }

// Description returns a brief description of the tool.
func (t *GoalListTool) Description() string {
	return "goal_list: lists all goals with their ID, title, status, and progress percentage."
}

// Execute returns a formatted list of all goals.
func (t *GoalListTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t.store == nil {
		return nil, errors.New("goal_list: nil store")
	}

	goals, err := t.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("goal_list: %w", err)
	}

	var sb strings.Builder
	for _, g := range goals {
		progress := t.goalProgress(g)
		sb.WriteString(fmt.Sprintf("[%s] %s: %s (%d%%)\n", g.Status.String(), g.ID, g.Title, progress))
	}

	return &ToolResult{
		Output:     strings.TrimSuffix(sb.String(), "\n"),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"count": len(goals),
		},
	}, nil
}

// goalProgress calculates the completion percentage of a goal based on its
// associated tasks.
func (t *GoalListTool) goalProgress(g *Goal) int {
	if len(g.TaskIDs) == 0 || t.taskStore == nil {
		return 0
	}
	completed := 0
	for _, tid := range g.TaskIDs {
		task, ok := t.taskStore.Get(tid)
		if ok && task.Status == StatusCompleted {
			completed++
		}
	}
	return completed * 100 / len(g.TaskIDs)
}

// GoalGetTool retrieves a single goal with its associated tasks. It implements
// the ToolDefinition interface.
type GoalGetTool struct {
	store     GoalStore
	taskStore *TaskStore
}

var _ ToolDefinition = (*GoalGetTool)(nil)

// NewGoalGetTool returns a GoalGetTool backed by the given GoalStore and
// TaskStore.
func NewGoalGetTool(store GoalStore, taskStore *TaskStore) *GoalGetTool {
	return &GoalGetTool{store: store, taskStore: taskStore}
}

// Name returns the tool name.
func (t *GoalGetTool) Name() string { return "goal_get" }

// Description returns a brief description of the tool.
func (t *GoalGetTool) Description() string {
	return "goal_get: retrieves a goal by ID with its associated tasks. Args: id (string)."
}

// Execute fetches the goal identified by args["id"] and its tasks.
func (t *GoalGetTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t.store == nil {
		return nil, errors.New("goal_get: nil store")
	}

	id, ok := call.Args["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return nil, errors.New("goal_get: missing string argument 'id'")
	}

	goal, err := t.store.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("goal_get: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ID: %s\n", goal.ID))
	sb.WriteString(fmt.Sprintf("Title: %s\n", goal.Title))
	sb.WriteString(fmt.Sprintf("Description: %s\n", goal.Description))
	sb.WriteString(fmt.Sprintf("Success Criteria: %s\n", goal.SuccessCriteria))
	sb.WriteString(fmt.Sprintf("Priority: %s\n", goal.Priority))
	sb.WriteString(fmt.Sprintf("Status: %s\n", goal.Status.String()))
	if len(goal.TaskIDs) > 0 {
		sb.WriteString("Tasks:\n")
		for _, tid := range goal.TaskIDs {
			if t.taskStore != nil {
				if task, found := t.taskStore.Get(tid); found {
					sb.WriteString(fmt.Sprintf("  [%s] %s: %s\n", task.Status, task.ID, task.Title))
				} else {
					sb.WriteString(fmt.Sprintf("  [unknown] %s\n", tid))
				}
			} else {
				sb.WriteString(fmt.Sprintf("  %s\n", tid))
			}
		}
	}

	return &ToolResult{
		Output:     strings.TrimSuffix(sb.String(), "\n"),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"id":         goal.ID,
			"title":      goal.Title,
			"status":     goal.Status.String(),
			"priority":   goal.Priority,
			"task_ids":   goal.TaskIDs,
			"task_count": len(goal.TaskIDs),
		},
	}, nil
}

// TaskUpdateTool updates a task in the bound TaskStore. It implements the
// ToolDefinition interface.
type TaskUpdateTool struct {
	store *TaskStore
}

var _ ToolDefinition = (*TaskUpdateTool)(nil)

// NewTaskUpdateTool returns a TaskUpdateTool backed by the given TaskStore.
func NewTaskUpdateTool(store *TaskStore) *TaskUpdateTool {
	return &TaskUpdateTool{store: store}
}

// Name returns the tool name.
func (t *TaskUpdateTool) Name() string { return "task_update" }

// Description returns a brief description of the tool.
func (t *TaskUpdateTool) Description() string {
	return "task_update: updates a task. Args: id (string), status (string), title (string), description (string)."
}

// Execute updates the task identified by args["id"] with any provided fields.
func (t *TaskUpdateTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t.store == nil {
		return nil, errors.New("task_update: nil store")
	}

	id, ok := call.Args["id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return nil, errors.New("task_update: missing string argument 'id'")
	}

	var opts []TaskUpdateOption
	if v, ok := call.Args["status"].(string); ok && strings.TrimSpace(v) != "" {
		opts = append(opts, WithTaskStatus(TaskStatus(v)))
	}
	if v, ok := call.Args["title"].(string); ok && strings.TrimSpace(v) != "" {
		opts = append(opts, WithTaskTitle(v))
	}
	if v, ok := call.Args["description"].(string); ok {
		opts = append(opts, WithTaskDescription(v))
	}

	if err := t.store.Update(id, opts...); err != nil {
		return nil, fmt.Errorf("task_update: %w", err)
	}

	task, found := t.store.Get(id)
	if !found {
		return nil, fmt.Errorf("task_update: task %q not found after update", id)
	}

	return &ToolResult{
		Output:     fmt.Sprintf("updated task %s: %s (status=%s)", task.ID, task.Title, task.Status),
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"id":     task.ID,
			"title":  task.Title,
			"status": string(task.Status),
		},
	}, nil
}
