package tools

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskStoreCreate(t *testing.T) {
	s := NewTaskStore()
	task := s.Create("my task", "do something")

	assert.Equal(t, "task-1", task.ID)
	assert.Equal(t, "my task", task.Title)
	assert.Equal(t, "do something", task.Description)
	assert.Equal(t, StatusPending, task.Status)
	assert.False(t, task.CreatedAt.IsZero())
	assert.False(t, task.UpdatedAt.IsZero())
}

func TestTaskStoreCreateAutoIncrement(t *testing.T) {
	s := NewTaskStore()
	s.Create("a", "")
	s.Create("b", "")
	s.Create("c", "")

	tasks := s.List()
	require.Len(t, tasks, 3)
	assert.Equal(t, "task-1", tasks[0].ID)
	assert.Equal(t, "task-2", tasks[1].ID)
	assert.Equal(t, "task-3", tasks[2].ID)
}

func TestTaskStoreGet(t *testing.T) {
	s := NewTaskStore()
	created := s.Create("my task", "desc")

	task, found := s.Get(created.ID)
	require.True(t, found)
	assert.Equal(t, "my task", task.Title)
	assert.Equal(t, "desc", task.Description)
}

func TestTaskStoreGetNotFound(t *testing.T) {
	s := NewTaskStore()
	_, found := s.Get("nonexistent")
	assert.False(t, found)
}

func TestTaskStoreList(t *testing.T) {
	s := NewTaskStore()
	s.Create("a", "desc-a")
	s.Create("b", "desc-b")

	tasks := s.List()
	require.Len(t, tasks, 2)
	assert.Equal(t, "a", tasks[0].Title)
	assert.Equal(t, "b", tasks[1].Title)
}

func TestTaskStoreListEmpty(t *testing.T) {
	s := NewTaskStore()
	assert.Empty(t, s.List())
}

func TestTaskStoreListPreservesOrder(t *testing.T) {
	s := NewTaskStore()
	s.Create("first", "")
	s.Create("second", "")
	s.Create("third", "")

	tasks := s.List()
	require.Len(t, tasks, 3)
	assert.Equal(t, "first", tasks[0].Title)
	assert.Equal(t, "second", tasks[1].Title)
	assert.Equal(t, "third", tasks[2].Title)
}

func TestTaskStoreUpdateStatus(t *testing.T) {
	s := NewTaskStore()
	task := s.Create("my task", "")

	err := s.Update(task.ID, WithTaskStatus("completed"))
	require.NoError(t, err)

	updated, _ := s.Get(task.ID)
	assert.Equal(t, StatusCompleted, updated.Status)
	assert.True(t, updated.UpdatedAt.After(updated.CreatedAt) || updated.UpdatedAt.Equal(updated.CreatedAt))
}

func TestTaskStoreUpdateTitle(t *testing.T) {
	s := NewTaskStore()
	task := s.Create("old title", "")

	err := s.Update(task.ID, WithTaskTitle("new title"))
	require.NoError(t, err)

	updated, _ := s.Get(task.ID)
	assert.Equal(t, "new title", updated.Title)
}

func TestTaskStoreUpdateDescription(t *testing.T) {
	s := NewTaskStore()
	task := s.Create("title", "old desc")

	err := s.Update(task.ID, WithTaskDescription("new desc"))
	require.NoError(t, err)

	updated, _ := s.Get(task.ID)
	assert.Equal(t, "new desc", updated.Description)
}

func TestTaskStoreUpdateMultipleOptions(t *testing.T) {
	s := NewTaskStore()
	task := s.Create("title", "desc")

	err := s.Update(task.ID,
		WithTaskStatus("in_progress"),
		WithTaskTitle("updated title"),
		WithTaskDescription("updated desc"),
	)
	require.NoError(t, err)

	updated, _ := s.Get(task.ID)
	assert.Equal(t, StatusInProgress, updated.Status)
	assert.Equal(t, "updated title", updated.Title)
	assert.Equal(t, "updated desc", updated.Description)
}

func TestTaskStoreUpdateNotFound(t *testing.T) {
	s := NewTaskStore()
	err := s.Update("nonexistent", WithTaskStatus("completed"))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestTaskStoreConcurrent(t *testing.T) {
	s := NewTaskStore()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Create("concurrent", "")
			_ = s.List()
			_, _ = s.Get("task-1")
		}()
	}
	wg.Wait()

	assert.Len(t, s.List(), 50)
}
