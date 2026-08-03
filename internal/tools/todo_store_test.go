package tools

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTodoStoreAddAutoID(t *testing.T) {
	s := NewTodoStore()
	s.Add(TodoItem{Content: "task A"})
	s.Add(TodoItem{Content: "task B"})

	items := s.List()
	require.Len(t, items, 2)
	assert.Equal(t, "todo-1", items[0].ID)
	assert.Equal(t, "todo-2", items[1].ID)
	assert.Equal(t, "task A", items[0].Content)
	assert.Equal(t, "task B", items[1].Content)
}

func TestTodoStoreAddDefaults(t *testing.T) {
	s := NewTodoStore()
	s.Add(TodoItem{Content: "task"})

	items := s.List()
	require.Len(t, items, 1)
	assert.Equal(t, "pending", items[0].Status)
	assert.Equal(t, "medium", items[0].Priority)
}

func TestTodoStoreAddExplicitID(t *testing.T) {
	s := NewTodoStore()
	s.Add(TodoItem{ID: "custom-1", Content: "task"})

	items := s.List()
	require.Len(t, items, 1)
	assert.Equal(t, "custom-1", items[0].ID)
}

func TestTodoStoreUpdate(t *testing.T) {
	s := NewTodoStore()
	s.Add(TodoItem{Content: "task A"})
	s.Update("todo-1", "completed")

	items := s.List()
	require.Len(t, items, 1)
	assert.Equal(t, "completed", items[0].Status)
}

func TestTodoStoreUpdateNotFound(t *testing.T) {
	s := NewTodoStore()
	s.Add(TodoItem{Content: "task A"})
	s.Update("nonexistent", "completed")

	items := s.List()
	require.Len(t, items, 1)
	assert.Equal(t, "pending", items[0].Status)
}

func TestTodoStoreRemove(t *testing.T) {
	s := NewTodoStore()
	s.Add(TodoItem{Content: "task A"})
	s.Add(TodoItem{Content: "task B"})
	s.Remove("todo-1")

	items := s.List()
	require.Len(t, items, 1)
	assert.Equal(t, "todo-2", items[0].ID)
}

func TestTodoStoreRemoveNotFound(t *testing.T) {
	s := NewTodoStore()
	s.Add(TodoItem{Content: "task A"})
	s.Remove("nonexistent")

	items := s.List()
	require.Len(t, items, 1)
}

func TestTodoStoreListReturnsCopy(t *testing.T) {
	s := NewTodoStore()
	s.Add(TodoItem{Content: "task A"})

	items := s.List()
	items[0].Content = "mutated"

	original := s.List()
	assert.Equal(t, "task A", original[0].Content)
}

func TestTodoStoreListEmpty(t *testing.T) {
	s := NewTodoStore()
	assert.Empty(t, s.List())
}

func TestTodoStoreConcurrent(t *testing.T) {
	s := NewTodoStore()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s.Add(TodoItem{Content: "concurrent"})
			_ = s.List()
		}(i)
	}
	wg.Wait()

	assert.Len(t, s.List(), 50)
}
