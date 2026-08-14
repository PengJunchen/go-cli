package core

import (
	"errors"
	"fmt"
)

// AgentState represents a point in the AgentImpl lifecycle state machine.
type AgentState string

const (
	// StateCreated is the initial state of a newly allocated AgentImpl before
	// construction is complete.
	StateCreated AgentState = "created"
	// StateInitialized indicates the agent has been fully constructed and is
	// ready to accept Run calls.
	StateInitialized AgentState = "initialized"
	// StateRunning indicates the agent is currently executing a Run.
	StateRunning AgentState = "running"
	// StatePaused indicates the agent's Run has been paused and may be resumed.
	StatePaused AgentState = "paused"
	// StateStopped indicates the agent has completed its Run and is in a
	// terminal state.
	StateStopped AgentState = "stopped"
	// StateError indicates the agent's Run ended in an error and is in a
	// terminal state.
	StateError AgentState = "error"
)

// ErrInvalidTransition is returned when a requested state transition is not
// permitted by the agent lifecycle state machine.
var ErrInvalidTransition = errors.New("core: invalid agent state transition")

// validTransitions enumerates the transitions allowed by the agent lifecycle
// state machine. Terminal states (Stopped, Error) have no outgoing
// transitions. Running may re-enter Running to support multiple runs.
var validTransitions = map[AgentState][]AgentState{
	StateCreated:     {StateInitialized, StateError},
	StateInitialized: {StateRunning, StateStopped, StateError},
	StateRunning:     {StatePaused, StateStopped, StateError, StateRunning},
	StatePaused:      {StateRunning, StateStopped, StateError},
	StateStopped:     {},
	StateError:       {},
}

// canTransition reports whether transitioning from one AgentState to another
// is permitted by the state machine.
func canTransition(from, to AgentState) bool {
	for _, allowed := range validTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// assertTransition returns ErrInvalidTransition when transitioning from one
// AgentState to another is not permitted by the state machine. It returns nil
// when the transition is allowed.
func assertTransition(from, to AgentState) error {
	if !canTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}
