package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEnumStringInvalidFallbacks(t *testing.T) {
	// Out-of-range enum values fall back to the documented default in each
	// type's String method.
	assert.Equal(t, "user", SubmissionType(99).String())
	assert.Equal(t, "discard_oldest", DiscardPolicy(99).String())
	assert.Equal(t, "allow", Classification(99).String())
}

func TestAgentEventStringRoundTrip(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	ev := AgentEvent{Kind: "message", Content: "hello", Timestamp: now}
	want := "2025-01-02T03:04:05Z [message] hello"
	assert.Equal(t, want, ev.String())
}

func TestAgentEventStringZeroTimestamp(t *testing.T) {
	ev := AgentEvent{Kind: "status", Content: "x"}
	// Zero time formats to an RFC3339 zero value without panicking.
	s := ev.String()
	assert.Contains(t, s, "[status] x")
}

func TestSubmissionMetadataNilIsSafe(t *testing.T) {
	sub := Submission{Type: SubmissionUserMessage, Content: "hi"}
	assert.Equal(t, SubmissionUserMessage, sub.Type)
	assert.Equal(t, "hi", sub.Content)
	assert.Nil(t, sub.Metadata)
	// A populated metadata round-trips.
	sub.Metadata = map[string]any{"k": "v"}
	assert.Equal(t, "v", sub.Metadata["k"])
}

func TestSubmissionTypeConstants(t *testing.T) {
	// Constants must remain ordered as documented.
	assert.Equal(t, 0, int(SubmissionUserMessage))
	assert.Equal(t, 1, int(SubmissionSteering))
	assert.Equal(t, 2, int(SubmissionFollowUp))
}

func TestDiscardPolicyConstants(t *testing.T) {
	assert.Equal(t, 0, int(DiscardOldest))
	assert.Equal(t, 1, int(DiscardNewest))
	assert.Equal(t, 2, int(BlockUntilConsumed))
}

func TestClassificationConstants(t *testing.T) {
	assert.Equal(t, 0, int(ClassificationAllow))
	assert.Equal(t, 1, int(ClassificationDeny))
	assert.Equal(t, 2, int(ClassificationRequireApproval))
}

func TestTurnStatusDoneEdge(t *testing.T) {
	// A fully zero Turn is not done; a completed one is.
	assert.False(t, (Turn{}).Done())
	assert.True(t, (Turn{Status: TurnCompleted}).Done())
	assert.True(t, (Turn{Status: TurnFailed}).Done())
	assert.True(t, (Turn{Status: TurnCanceled}).Done())
}
