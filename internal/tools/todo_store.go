package tools

import (
	"context"
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
	mu       sync.RWMutex
	items    []TodoItem
	counter  int64
	revision uint64
	updates  chan TodoSnapshot
}

// TodoSnapshot is a copied view of the todo store at one revision.
type TodoSnapshot struct {
	Revision uint64
	Items    []TodoItem
}

// NewTodoStore returns an empty TodoStore.
func NewTodoStore() *TodoStore {
	return &TodoStore{
		items:   make([]TodoItem, 0),
		updates: make(chan TodoSnapshot, 1),
	}
}

// Add appends a todo item to the store. When item.ID is empty an auto-incremented
// ID of the form "todo-N" is assigned.
func (s *TodoStore) Add(item TodoItem) TodoSnapshot {
	snapshot, _ := s.add(context.Background(), item)
	return snapshot
}

func (s *TodoStore) add(ctx context.Context, item TodoItem) (TodoSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return TodoSnapshot{}, err
	}

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
	s.revision++
	slog.Debug("todo_store.add", "id", item.ID, "status", item.Status)
	return s.changedSnapshotLocked(), nil
}

// Update sets the status of the todo item with the given ID. It is a no-op when
// the ID is unknown.
func (s *TodoStore) Update(id string, status string) (TodoSnapshot, bool) {
	snapshot, changed, _ := s.update(context.Background(), id, status)
	return snapshot, changed
}

func (s *TodoStore) update(ctx context.Context, id string, status string) (TodoSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return TodoSnapshot{}, false, err
	}

	for i := range s.items {
		if s.items[i].ID == id {
			if s.items[i].Status == status {
				slog.Debug("todo_store.update_unchanged", "id", id, "status", status)
				return s.snapshotLocked(), false, nil
			}
			s.items[i].Status = status
			s.revision++
			slog.Debug("todo_store.update", "id", id, "status", status)
			return s.changedSnapshotLocked(), true, nil
		}
	}
	slog.Debug("todo_store.update_not_found", "id", id)
	return s.snapshotLocked(), false, nil
}

// Snapshot returns the current todo state with an independent item slice.
func (s *TodoStore) Snapshot() TodoSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

func (s *TodoStore) snapshotLocked() TodoSnapshot {
	items := make([]TodoItem, len(s.items))
	copy(items, s.items)
	return TodoSnapshot{Revision: s.revision, Items: items}
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
func (s *TodoStore) Remove(id string) (TodoSnapshot, bool) {
	snapshot, changed, _ := s.remove(context.Background(), id)
	return snapshot, changed
}

func (s *TodoStore) remove(ctx context.Context, id string) (TodoSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return TodoSnapshot{}, false, err
	}

	for i, item := range s.items {
		if item.ID == id {
			s.items = append(s.items[:i], s.items[i+1:]...)
			s.revision++
			slog.Debug("todo_store.remove", "id", id)
			return s.changedSnapshotLocked(), true, nil
		}
	}
	slog.Debug("todo_store.remove_not_found", "id", id)
	return s.snapshotLocked(), false, nil
}

// Updates returns bounded, coalesced copied snapshots for host UI observation.
// Delivery is independent of a tool receipt and never invokes user callbacks.
func (s *TodoStore) Updates() <-chan TodoSnapshot { return s.updates }

func (s *TodoStore) changedSnapshotLocked() TodoSnapshot {
	snapshot := s.snapshotLocked()
	if s.updates != nil {
		select {
		case <-s.updates:
		default:
		}
		update := TodoSnapshot{Revision: snapshot.Revision, Items: append([]TodoItem(nil), snapshot.Items...)}
		select {
		case s.updates <- update:
		default:
		}
	}
	return snapshot
}
