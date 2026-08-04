package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// GoalStatus represents the lifecycle state of a goal.
type GoalStatus int

const (
	// GoalStatusDraft is the initial state of a newly created goal.
	GoalStatusDraft GoalStatus = iota // 0
	// GoalStatusActive indicates the goal has been activated for work.
	GoalStatusActive // 1
	// GoalStatusInProgress indicates work is underway on the goal.
	GoalStatusInProgress // 2
	// GoalStatusCompleted indicates the goal is finished.
	GoalStatusCompleted // 3
	// GoalStatusBlocked indicates the goal cannot proceed.
	GoalStatusBlocked // 4
	// GoalStatusAbandoned indicates the goal has been abandoned.
	GoalStatusAbandoned // 5
)

// String returns the human-readable name of the goal status.
func (s GoalStatus) String() string {
	switch s {
	case GoalStatusDraft:
		return "draft"
	case GoalStatusActive:
		return "active"
	case GoalStatusInProgress:
		return "in_progress"
	case GoalStatusCompleted:
		return "completed"
	case GoalStatusBlocked:
		return "blocked"
	case GoalStatusAbandoned:
		return "abandoned"
	default:
		return "unknown"
	}
}

// Goal represents a high-level objective with associated tasks.
type Goal struct {
	// ID uniquely identifies the goal.
	ID string `json:"id"`
	// Title is the short title of the goal.
	Title string `json:"title"`
	// Description is the longer description of the goal.
	Description string `json:"description"`
	// SuccessCriteria describes how to determine the goal is achieved.
	SuccessCriteria string `json:"success_criteria"`
	// Priority is one of "high", "medium", or "low".
	Priority string `json:"priority"`
	// Status is the current lifecycle state of the goal.
	Status GoalStatus `json:"status"`
	// TaskIDs holds the IDs of associated tasks.
	TaskIDs []string `json:"task_ids"`
	// CreatedAt is when the goal was created.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when the goal was last modified.
	UpdatedAt time.Time `json:"updated_at"`
	// CompletedAt is when the goal was completed, or nil if not completed.
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// GoalUpdateOption configures an incremental update to a Goal.
type GoalUpdateOption func(*Goal)

// WithGoalStatus sets the status of a goal.
func WithGoalStatus(status GoalStatus) GoalUpdateOption {
	return func(g *Goal) { g.Status = status }
}

// WithGoalTitle sets the title of a goal.
func WithGoalTitle(title string) GoalUpdateOption {
	return func(g *Goal) { g.Title = title }
}

// WithGoalDescription sets the description of a goal.
func WithGoalDescription(desc string) GoalUpdateOption {
	return func(g *Goal) { g.Description = desc }
}

// WithGoalPriority sets the priority of a goal.
func WithGoalPriority(priority string) GoalUpdateOption {
	return func(g *Goal) { g.Priority = priority }
}

// WithGoalSuccessCriteria sets the success criteria of a goal.
func WithGoalSuccessCriteria(criteria string) GoalUpdateOption {
	return func(g *Goal) { g.SuccessCriteria = criteria }
}

// GoalStore manages goals.
type GoalStore interface {
	// Create adds a new goal with the given title, description, and success
	// criteria, returning the created Goal.
	Create(ctx context.Context, title, description, criteria string) (*Goal, error)
	// Get returns the goal with the given ID.
	Get(ctx context.Context, id string) (*Goal, error)
	// Update applies the given options to the goal with the specified ID.
	Update(ctx context.Context, id string, opts ...GoalUpdateOption) error
	// List returns all goals sorted by CreatedAt.
	List(ctx context.Context) ([]*Goal, error)
	// Delete removes the goal with the given ID.
	Delete(ctx context.Context, id string) error
	// AddTask associates a task with the goal.
	AddTask(ctx context.Context, goalID, taskID string) error
	// RemoveTask disassociates a task from the goal.
	RemoveTask(ctx context.Context, goalID, taskID string) error
}

// DefaultGoalStore implements GoalStore with in-memory storage and optional
// JSONL persistence. It is concurrency-safe.
type DefaultGoalStore struct {
	mu      sync.RWMutex
	goals   map[string]*Goal
	path    string // JSONL persistence path, empty = memory only
	counter int64
}

var _ GoalStore = (*DefaultGoalStore)(nil)

// NewDefaultGoalStore returns a DefaultGoalStore. When path is non-empty the
// store loads existing goals from the JSONL file and persists all mutations.
func NewDefaultGoalStore(path string) (*DefaultGoalStore, error) {
	s := &DefaultGoalStore{
		goals: make(map[string]*Goal),
		path:  path,
	}
	if path != "" {
		if err := s.load(); err != nil {
			return nil, fmt.Errorf("goal_store: load: %w", err)
		}
	}
	return s, nil
}

// Create adds a new goal with Draft status and returns a copy.
func (s *DefaultGoalStore) Create(_ context.Context, title, description, criteria string) (*Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.counter++
	now := time.Now()
	goal := &Goal{
		ID:              fmt.Sprintf("goal_%d_%d", now.Unix(), s.counter),
		Title:           title,
		Description:     description,
		SuccessCriteria: criteria,
		Priority:        "medium",
		Status:          GoalStatusDraft,
		TaskIDs:         []string{},
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.goals[goal.ID] = goal

	if err := s.saveLocked(); err != nil {
		return nil, fmt.Errorf("goal_store: save: %w", err)
	}

	cp := *goal
	return &cp, nil
}

// Get returns a copy of the goal with the given ID.
func (s *DefaultGoalStore) Get(_ context.Context, id string) (*Goal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	g, ok := s.goals[id]
	if !ok {
		return nil, fmt.Errorf("goal_store: goal %q not found", id)
	}
	cp := *g
	return &cp, nil
}

// Update applies the given options to the goal with the specified ID.
func (s *DefaultGoalStore) Update(_ context.Context, id string, opts ...GoalUpdateOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.goals[id]
	if !ok {
		return fmt.Errorf("goal_store: goal %q not found", id)
	}

	for _, opt := range opts {
		opt(g)
	}
	g.UpdatedAt = time.Now()

	// Set CompletedAt when transitioning to Completed.
	if g.Status == GoalStatusCompleted && g.CompletedAt == nil {
		now := time.Now()
		g.CompletedAt = &now
	}

	return s.saveLocked()
}

// List returns all goals sorted by CreatedAt.
func (s *DefaultGoalStore) List(_ context.Context) ([]*Goal, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Goal, 0, len(s.goals))
	for _, g := range s.goals {
		cp := *g
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the goal with the given ID.
func (s *DefaultGoalStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.goals[id]; !ok {
		return fmt.Errorf("goal_store: goal %q not found", id)
	}
	delete(s.goals, id)
	return s.saveLocked()
}

// AddTask associates a task with the goal. It is a no-op if the task is already
// associated.
func (s *DefaultGoalStore) AddTask(_ context.Context, goalID, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.goals[goalID]
	if !ok {
		return fmt.Errorf("goal_store: goal %q not found", goalID)
	}
	for _, tid := range g.TaskIDs {
		if tid == taskID {
			return nil
		}
	}
	g.TaskIDs = append(g.TaskIDs, taskID)
	g.UpdatedAt = time.Now()
	return s.saveLocked()
}

// RemoveTask disassociates a task from the goal. It is a no-op if the task is
// not associated.
func (s *DefaultGoalStore) RemoveTask(_ context.Context, goalID, taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	g, ok := s.goals[goalID]
	if !ok {
		return fmt.Errorf("goal_store: goal %q not found", goalID)
	}
	for i, tid := range g.TaskIDs {
		if tid == taskID {
			g.TaskIDs = append(g.TaskIDs[:i], g.TaskIDs[i+1:]...)
			g.UpdatedAt = time.Now()
			return s.saveLocked()
		}
	}
	return nil
}

// saveLocked writes all goals to the JSONL file. It must be called while
// holding the write lock.
func (s *DefaultGoalStore) saveLocked() error {
	if s.path == "" {
		return nil
	}
	var buf bytes.Buffer
	for _, g := range s.goals {
		data, err := json.Marshal(g)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	return os.WriteFile(s.path, buf.Bytes(), 0644)
}

// load reads goals from the JSONL file into the store. It must be called before
// the store is used concurrently.
func (s *DefaultGoalStore) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var g Goal
		if err := json.Unmarshal(line, &g); err != nil {
			return err
		}
		s.goals[g.ID] = &g
		s.counter++
	}
	return nil
}
