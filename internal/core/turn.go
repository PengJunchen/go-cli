package core

import (
	"time"
)

// TurnStatus describes the lifecycle state of a Turn.
type TurnStatus int

const (
	// TurnPending is a turn that has been created but not yet started.
	TurnPending TurnStatus = iota
	// TurnRunning is a turn currently being executed.
	TurnRunning
	// TurnCompleted is a turn that finished successfully.
	TurnCompleted
	// TurnCanceled is a turn that was canceled before completion.
	TurnCanceled
	// TurnFailed is a turn that finished with an error.
	TurnFailed
)

// String returns the textual name of the turn status.
func (s TurnStatus) String() string {
	switch s {
	case TurnRunning:
		return "running"
	case TurnCompleted:
		return "completed"
	case TurnCanceled:
		return "canceled"
	case TurnFailed:
		return "failed"
	default:
		return "pending"
	}
}

// Turn is a single turn execution managed by a TurnRunner. It records the
// submission, lifecycle status, timing, terminal result and any steering /
// follow-up inputs applied while the turn was running.
type Turn struct {
	// ID uniquely identifies the turn within its runner.
	ID string
	// Submission is the request that started the turn.
	Submission Submission
	// Status is the current lifecycle state of the turn.
	Status TurnStatus
	// StartTime records when the turn began running.
	StartTime time.Time
	// EndTime records when the turn finished (zero while running).
	EndTime time.Time
	// Result is the terminal result of the turn.
	Result Result
	// Err is the terminal error of the turn, if any.
	Err error
	// Canceled reports whether the turn was explicitly canceled.
	Canceled bool
	// Steerings holds the steering submissions applied while running.
	Steerings []Submission
	// FollowUps holds the follow-up submissions applied while running.
	FollowUps []Submission
}

// Done reports whether the turn has reached a terminal state.
func (t Turn) Done() bool {
	switch t.Status {
	case TurnCompleted, TurnCanceled, TurnFailed:
		return true
	default:
		return false
	}
}
