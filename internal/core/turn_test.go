package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTurnStatusStringTable(t *testing.T) {
	tests := []struct {
		name   string
		status TurnStatus
		want   string
	}{
		{"pending", TurnPending, "pending"},
		{"running", TurnRunning, "running"},
		{"completed", TurnCompleted, "completed"},
		{"canceled", TurnCanceled, "canceled"},
		{"failed", TurnFailed, "failed"},
		{"unknown_defaults_to_pending", TurnStatus(99), "pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.status.String())
		})
	}
}

func TestTurnDone(t *testing.T) {
	tests := []struct {
		name   string
		status TurnStatus
		want   bool
	}{
		{"pending_not_done", TurnPending, false},
		{"running_not_done", TurnRunning, false},
		{"completed_is_done", TurnCompleted, true},
		{"canceled_is_done", TurnCanceled, true},
		{"failed_is_done", TurnFailed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turn := Turn{Status: tt.status}
			assert.Equal(t, tt.want, turn.Done())
		})
	}
}

func TestTurnStatusValues(t *testing.T) {
	assert.True(t, TurnPending < TurnRunning, "TurnPending < TurnRunning")
	assert.True(t, TurnRunning < TurnCompleted, "TurnRunning < TurnCompleted")
	assert.True(t, TurnCompleted < TurnCanceled, "TurnCompleted < TurnCanceled")
	assert.True(t, TurnCanceled < TurnFailed, "TurnCanceled < TurnFailed")
}
