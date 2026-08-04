package tools

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalStoreSatisfiesInterface(t *testing.T) {
	var _ GoalStore = (*DefaultGoalStore)(nil)
}

func TestGoalStoreCreate(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	goal, err := s.Create(context.Background(), "build feature", "implement the new feature", "tests pass")
	require.NoError(t, err)

	assert.NotEmpty(t, goal.ID)
	assert.Contains(t, goal.ID, "goal_")
	assert.Equal(t, "build feature", goal.Title)
	assert.Equal(t, "implement the new feature", goal.Description)
	assert.Equal(t, "tests pass", goal.SuccessCriteria)
	assert.Equal(t, GoalStatusDraft, goal.Status)
	assert.Equal(t, "medium", goal.Priority)
	assert.Empty(t, goal.TaskIDs)
	assert.False(t, goal.CreatedAt.IsZero())
	assert.False(t, goal.UpdatedAt.IsZero())
	assert.Nil(t, goal.CompletedAt)
}

func TestGoalStoreGet(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	created, err := s.Create(context.Background(), "my goal", "desc", "criteria")
	require.NoError(t, err)

	goal, err := s.Get(context.Background(), created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, goal.ID)
	assert.Equal(t, "my goal", goal.Title)
}

func TestGoalStoreGetNotFound(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	_, err = s.Get(context.Background(), "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGoalStoreUpdateStatusTransition(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	goal, err := s.Create(context.Background(), "goal", "desc", "criteria")
	require.NoError(t, err)

	// Draft -> Active
	err = s.Update(context.Background(), goal.ID, WithGoalStatus(GoalStatusActive))
	require.NoError(t, err)
	g, _ := s.Get(context.Background(), goal.ID)
	assert.Equal(t, GoalStatusActive, g.Status)

	// Active -> InProgress
	err = s.Update(context.Background(), goal.ID, WithGoalStatus(GoalStatusInProgress))
	require.NoError(t, err)
	g, _ = s.Get(context.Background(), goal.ID)
	assert.Equal(t, GoalStatusInProgress, g.Status)

	// InProgress -> Completed (should set CompletedAt)
	err = s.Update(context.Background(), goal.ID, WithGoalStatus(GoalStatusCompleted))
	require.NoError(t, err)
	g, _ = s.Get(context.Background(), goal.ID)
	assert.Equal(t, GoalStatusCompleted, g.Status)
	assert.NotNil(t, g.CompletedAt)
}

func TestGoalStoreUpdateBlocked(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	goal, err := s.Create(context.Background(), "goal", "desc", "criteria")
	require.NoError(t, err)

	err = s.Update(context.Background(), goal.ID, WithGoalStatus(GoalStatusBlocked))
	require.NoError(t, err)
	g, _ := s.Get(context.Background(), goal.ID)
	assert.Equal(t, GoalStatusBlocked, g.Status)
}

func TestGoalStoreUpdateTitleAndPriority(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	goal, err := s.Create(context.Background(), "old title", "desc", "criteria")
	require.NoError(t, err)

	err = s.Update(context.Background(), goal.ID,
		WithGoalTitle("new title"),
		WithGoalPriority("high"),
		WithGoalDescription("new desc"),
		WithGoalSuccessCriteria("new criteria"),
	)
	require.NoError(t, err)

	g, _ := s.Get(context.Background(), goal.ID)
	assert.Equal(t, "new title", g.Title)
	assert.Equal(t, "high", g.Priority)
	assert.Equal(t, "new desc", g.Description)
	assert.Equal(t, "new criteria", g.SuccessCriteria)
}

func TestGoalStoreUpdateNotFound(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	err = s.Update(context.Background(), "nonexistent", WithGoalStatus(GoalStatusActive))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGoalStoreAddTask(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	goal, err := s.Create(context.Background(), "goal", "desc", "criteria")
	require.NoError(t, err)

	err = s.AddTask(context.Background(), goal.ID, "task-1")
	require.NoError(t, err)

	g, _ := s.Get(context.Background(), goal.ID)
	assert.Contains(t, g.TaskIDs, "task-1")

	// Adding the same task again is a no-op.
	err = s.AddTask(context.Background(), goal.ID, "task-1")
	require.NoError(t, err)

	g, _ = s.Get(context.Background(), goal.ID)
	assert.Len(t, g.TaskIDs, 1)
}

func TestGoalStoreRemoveTask(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	goal, err := s.Create(context.Background(), "goal", "desc", "criteria")
	require.NoError(t, err)

	_ = s.AddTask(context.Background(), goal.ID, "task-1")
	_ = s.AddTask(context.Background(), goal.ID, "task-2")

	err = s.RemoveTask(context.Background(), goal.ID, "task-1")
	require.NoError(t, err)

	g, _ := s.Get(context.Background(), goal.ID)
	assert.NotContains(t, g.TaskIDs, "task-1")
	assert.Contains(t, g.TaskIDs, "task-2")

	// Removing a non-associated task is a no-op.
	err = s.RemoveTask(context.Background(), goal.ID, "task-99")
	require.NoError(t, err)
}

func TestGoalStoreAddTaskNotFound(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	err = s.AddTask(context.Background(), "nonexistent", "task-1")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGoalStoreListSortedByCreatedAt(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	g1, _ := s.Create(context.Background(), "first", "", "")
	time.Sleep(1 * time.Millisecond)
	g2, _ := s.Create(context.Background(), "second", "", "")
	time.Sleep(1 * time.Millisecond)
	g3, _ := s.Create(context.Background(), "third", "", "")

	goals, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, goals, 3)

	assert.Equal(t, g1.ID, goals[0].ID)
	assert.Equal(t, g2.ID, goals[1].ID)
	assert.Equal(t, g3.ID, goals[2].ID)
}

func TestGoalStoreListEmpty(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	goals, err := s.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, goals)
}

func TestGoalStoreDelete(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	goal, _ := s.Create(context.Background(), "goal", "desc", "criteria")

	err = s.Delete(context.Background(), goal.ID)
	require.NoError(t, err)

	_, err = s.Get(context.Background(), goal.ID)
	assert.Error(t, err)
}

func TestGoalStoreDeleteNotFound(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	err = s.Delete(context.Background(), "nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGoalStoreJSONLPersistence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/goals.jsonl"

	// Create a store with persistence and add goals.
	s1, err := NewDefaultGoalStore(path)
	require.NoError(t, err)

	g1, err := s1.Create(context.Background(), "goal one", "desc one", "criteria one")
	require.NoError(t, err)

	err = s1.Update(context.Background(), g1.ID, WithGoalStatus(GoalStatusActive))
	require.NoError(t, err)

	err = s1.AddTask(context.Background(), g1.ID, "task-1")
	require.NoError(t, err)

	g2, err := s1.Create(context.Background(), "goal two", "desc two", "criteria two")
	require.NoError(t, err)

	// Reload from the same path and verify goals are restored.
	s2, err := NewDefaultGoalStore(path)
	require.NoError(t, err)

	goals, err := s2.List(context.Background())
	require.NoError(t, err)
	require.Len(t, goals, 2)

	restored1, err := s2.Get(context.Background(), g1.ID)
	require.NoError(t, err)
	assert.Equal(t, "goal one", restored1.Title)
	assert.Equal(t, GoalStatusActive, restored1.Status)
	assert.Contains(t, restored1.TaskIDs, "task-1")

	restored2, err := s2.Get(context.Background(), g2.ID)
	require.NoError(t, err)
	assert.Equal(t, "goal two", restored2.Title)
	assert.Equal(t, GoalStatusDraft, restored2.Status)
}

func TestGoalStoreConcurrent(t *testing.T) {
	s, err := NewDefaultGoalStore("")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Create(context.Background(), "concurrent", "desc", "criteria")
			_, _ = s.List(context.Background())
		}()
	}
	wg.Wait()

	goals, err := s.List(context.Background())
	require.NoError(t, err)
	assert.Len(t, goals, 50)
}

func TestGoalStatusString(t *testing.T) {
	assert.Equal(t, "draft", GoalStatusDraft.String())
	assert.Equal(t, "active", GoalStatusActive.String())
	assert.Equal(t, "in_progress", GoalStatusInProgress.String())
	assert.Equal(t, "completed", GoalStatusCompleted.String())
	assert.Equal(t, "blocked", GoalStatusBlocked.String())
	assert.Equal(t, "abandoned", GoalStatusAbandoned.String())
}
