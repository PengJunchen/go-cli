package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
)

func TestAgentState_NewAgent(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"S-01", "state",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
	loop := NewLoopAgent(WithLLM(model))
	a := NewAgentImpl("state-agent", loop)
	assert.Equal(t, StateInitialized, a.State())
}

func TestAgentState_Transitions(t *testing.T) {
	// Valid transitions through a normal lifecycle.
	require.NoError(t, assertTransition(StateCreated, StateInitialized))
	require.NoError(t, assertTransition(StateInitialized, StateRunning))
	require.NoError(t, assertTransition(StateRunning, StateStopped))
	// Running may re-enter Running to support multiple runs.
	require.NoError(t, assertTransition(StateRunning, StateRunning))
	// Paused can resume to Running or terminate.
	require.NoError(t, assertTransition(StateRunning, StatePaused))
	require.NoError(t, assertTransition(StatePaused, StateRunning))
}

func TestAgentState_InvalidTransition(t *testing.T) {
	// Stopped cannot transition to Initialized or Created.
	err := assertTransition(StateStopped, StateInitialized)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTransition)

	// Error cannot transition to Initialized or Created.
	err = assertTransition(StateError, StateCreated)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTransition)

	// Created cannot jump directly to Running.
	err = assertTransition(StateCreated, StateRunning)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidTransition)
}

func TestAgentState_RunChangesState(t *testing.T) {
	t.Run("success_stops_in_stopped", func(t *testing.T) {
		model := mock.NewMockLLMServer(mock.NewConversationTemplate(
			"S-02", "state",
			mock.ConversationTurn{AssistantContent: "done"},
		))
		loop := NewLoopAgent(WithLLM(model))
		a := NewAgentImpl("ok-agent", loop)

		_, err := a.Run(context.Background(), Submission{Content: "go"})
		require.NoError(t, err)
		assert.Equal(t, StateStopped, a.State())
	})

	t.Run("error_stops_in_error", func(t *testing.T) {
		a := NewAgentImpl("err-agent", &errorLoop{err: errors.New("boom")})
		_, err := a.Run(context.Background(), Submission{Content: "go"})
		require.Error(t, err)
		assert.Equal(t, StateError, a.State())
	})
}

func TestCanTransition(t *testing.T) {
	tests := []struct {
		name string
		from AgentState
		to   AgentState
		want bool
	}{
		// Created -> Initialized, Error
		{"Created->Initialized", StateCreated, StateInitialized, true},
		{"Created->Error", StateCreated, StateError, true},
		{"Created->Running", StateCreated, StateRunning, false},
		{"Created->Stopped", StateCreated, StateStopped, false},
		{"Created->Paused", StateCreated, StatePaused, false},

		// Initialized -> Running, Stopped, Error
		{"Initialized->Running", StateInitialized, StateRunning, true},
		{"Initialized->Stopped", StateInitialized, StateStopped, true},
		{"Initialized->Error", StateInitialized, StateError, true},
		{"Initialized->Paused", StateInitialized, StatePaused, false},
		{"Initialized->Created", StateInitialized, StateCreated, false},

		// Running -> Paused, Stopped, Error, Running
		{"Running->Paused", StateRunning, StatePaused, true},
		{"Running->Stopped", StateRunning, StateStopped, true},
		{"Running->Error", StateRunning, StateError, true},
		{"Running->Running", StateRunning, StateRunning, true},
		{"Running->Initialized", StateRunning, StateInitialized, false},

		// Paused -> Running, Stopped, Error
		{"Paused->Running", StatePaused, StateRunning, true},
		{"Paused->Stopped", StatePaused, StateStopped, true},
		{"Paused->Error", StatePaused, StateError, true},
		{"Paused->Created", StatePaused, StateCreated, false},
		{"Paused->Paused", StatePaused, StatePaused, false},

		// Stopped can transition back to Running or Error for agent reuse.
		{"Stopped->Running", StateStopped, StateRunning, true},
		{"Stopped->Initialized", StateStopped, StateInitialized, false},
		{"Stopped->Error", StateStopped, StateError, true},
		{"Stopped->Stopped", StateStopped, StateStopped, false},

		// Error can transition back to Running or Stopped for agent reuse.
		{"Error->Running", StateError, StateRunning, true},
		{"Error->Initialized", StateError, StateInitialized, false},
		{"Error->Stopped", StateError, StateStopped, true},
		{"Error->Error", StateError, StateError, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, canTransition(tt.from, tt.to))
		})
	}
}
