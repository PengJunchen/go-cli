package core

import (
	"reflect"
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
	// RunID identifies the run claim owned by this turn.
	RunID string
	// Submission is the request that started the turn.
	Submission Submission
	// Status is the current lifecycle state of the turn.
	Status TurnStatus
	// Outcome records logical completion independently from resource cleanup.
	Outcome RunOutcome
	// ExecutionState reports whether owned execution resources have quiesced.
	ExecutionState RunExecutionState
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

func cloneSubmission(submission Submission) Submission {
	submission.Metadata = cloneStringAnyMap(submission.Metadata)
	submission.History = cloneAgentMessages(submission.History)
	return submission
}

func cloneTurn(turn Turn) Turn {
	turn.Submission = cloneSubmission(turn.Submission)
	if turn.Steerings != nil {
		steerings := make([]Submission, len(turn.Steerings))
		for i := range turn.Steerings {
			steerings[i] = cloneSubmission(turn.Steerings[i])
		}
		turn.Steerings = steerings
	}
	if turn.FollowUps != nil {
		followUps := make([]Submission, len(turn.FollowUps))
		for i := range turn.FollowUps {
			followUps[i] = cloneSubmission(turn.FollowUps[i])
		}
		turn.FollowUps = followUps
	}
	return turn
}

func cloneAgentMessage(message AgentMessage) AgentMessage {
	return cloneAgentMessages([]AgentMessage{message})[0]
}

func cloneStringAnyMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	var cloned map[string]any
	reflect.ValueOf(&cloned).Elem().Set(cloneJSONValue(reflect.ValueOf(input), make(map[jsonCloneIdentity]reflect.Value)))
	return cloned
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
