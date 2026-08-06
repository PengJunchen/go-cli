package tools

import (
	"fmt"
	"log/slog"
	"sync"
)

// TodoItem represents a single todo entry.
type TodoItem struct {
	// ID uniquely identifies the todo item.
	ID string `json:"id"`
	// Content is the human-readable description of the todo.
	Content string `json:"content"`
	// Status is one of "pending", "in_progress", or "completed".
	Status string `json:"status"`
	// Priority is one of "high", "medium", or "low".
	Priority string `json:"priority"`
}

// TodoStore manages an ordered list of TodoItem entries. It is concurrency-safe.
type TodoStore struct {
	mu      sync.RWMutex
	items   []TodoItem
	counter int64
}

// NewTodoStore returns an empty TodoStore.
func NewTodoStore() *TodoStore {
	return &TodoStore{
		items: make([]TodoItem, 0),
	}
}

// Add appends a todo item to the store. When item.ID is empty an auto-incremented
// ID of the form "todo-N" is assigned.
func (s *TodoStore) Add(item TodoItem) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if item.ID == "" {
		s.counter++
		item.ID = fmt.Sprintf("todo-%d", s.counter)
	}
	if item.Status == "" {
		item.Status = "pending"
	}
	if item.Priority == "" {
		item.Priority = "medium"
	}
	s.items = append(s.items, item)
	slog.Debug("todo_store.add", "id", item.ID, "status", item.Status)
}

// Update sets the status of the todo item with the given ID. It is a no-op when
// the ID is unknown.
func (s *TodoStore) Update(id string, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].Status = status
			slog.Debug("todo_store.update", "id", id, "status", status)
			return
		}
	}
	slog.Debug("todo_store.update_not_found", "id", id)
}

// List returns a copy of all todo items in insertion order.
func (s *TodoStore) List() []TodoItem {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]TodoItem, len(s.items))
	copy(out, s.items)
	return out
}

// Remove deletes the todo item with the given ID. It is a no-op when the ID is
// unknown.
func (s *TodoStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, item := range s.items {
		if item.ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			slog.Debug("todo_store.remove", "id", id)
			return
		}
	}
	slog.Debug("todo_store.remove_not_found", "id", id)
}
