package tools

import (
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// TaskStatus represents the lifecycle state of a task.
type TaskStatus string

const (
	// StatusPending is the initial state of a newly created task.
	StatusPending TaskStatus = "pending"
	// StatusInProgress indicates work has started on the task.
	StatusInProgress TaskStatus = "in_progress"
	// StatusCompleted indicates the task is finished.
	StatusCompleted TaskStatus = "completed"
	// StatusBlocked indicates the task cannot proceed due to a dependency or
	// external issue.
	StatusBlocked TaskStatus = "blocked"
	// StatusCancelled indicates the task has been abandoned.
	StatusCancelled TaskStatus = "cancelled" //nolint:misspell
)

// Task represents a single task managed by TaskStore.
type Task struct {
	// ID uniquely identifies the task.
	ID string `json:"id"`
	// Title is the short title of the task.
	Title string `json:"title"`
	// Description is the longer description of the task.
	Description string `json:"description"`
	// Status is one of "pending", "in_progress", "completed", "blocked", or
	// "canceled".
	Status TaskStatus `json:"status"`
	// CreatedAt is when the task was created.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the task was last modified.
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskUpdateOption configures an incremental update to a Task.
type TaskUpdateOption func(*Task)

// WithTaskStatus sets the status of a task.
func WithTaskStatus(status TaskStatus) TaskUpdateOption {
	return func(t *Task) { t.Status = status }
}

// WithTaskTitle sets the title of a task.
func WithTaskTitle(title string) TaskUpdateOption {
	return func(t *Task) { t.Title = title }
}

// WithTaskDescription sets the description of a task.
func WithTaskDescription(desc string) TaskUpdateOption {
	return func(t *Task) { t.Description = desc }
}

// TaskStore manages a collection of tasks keyed by ID. It is concurrency-safe.
type TaskStore struct {
	mu      sync.RWMutex
	tasks   map[string]Task
	order   []string // preserves insertion order for List
	counter int64
}

// NewTaskStore returns an empty TaskStore.
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks: make(map[string]Task),
	}
}

// Create adds a new task with the given title and description, returning the
// created Task. The ID is auto-generated as "task-N".
func (s *TaskStore) Create(title, description string) Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counter++
	id := fmt.Sprintf("task-%d", s.counter)
	now := time.Now()
	task := Task{
		ID:          id,
		Title:       title,
		Description: description,
		Status:      StatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.tasks[id] = task
	s.order = append(s.order, id)

	slog.Debug("task_store.create", "id", id, "title", title)
	return task
}

// Get returns the task with the given ID and whether it was found.
func (s *TaskStore) Get(id string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[id]
	return task, ok
}

// List returns all tasks in insertion order.
func (s *TaskStore) List() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Task, 0, len(s.order))
	for _, id := range s.order {
		if task, ok := s.tasks[id]; ok {
			out = append(out, task)
		}
	}
	return out
}

// Update applies the given options to the task with the specified ID. It
// returns an error when the ID is unknown.
func (s *TaskStore) Update(id string, opts ...TaskUpdateOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[id]
	if !ok {
		slog.Debug("task_store.update_not_found", "id", id)
		return fmt.Errorf("task_store: task %q not found", id)
	}

	for _, opt := range opts {
		opt(&task)
	}
	task.UpdatedAt = time.Now()
	s.tasks[id] = task

	slog.Debug("task_store.update", "id", id)
	return nil
}
