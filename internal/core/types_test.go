package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestCoreTypesConstruct(t *testing.T) {
	now := time.Now()

	msg := AgentMessage{Role: "user", Content: "hi"}
	assert.Equal(t, "user: hi", msg.String())

	evt := AgentEvent{Kind: "message", Content: "thinking", Timestamp: now}
	assert.Contains(t, evt.String(), "[message] thinking")

	res := Result{Message: "done", Success: true}
	assert.Equal(t, "done", res.String())

	sub := Submission{
		Type:     SubmissionUserMessage,
		Content:  "build it",
		Metadata: map[string]any{"k": "v"},
	}
	assert.Equal(t, SubmissionUserMessage, sub.Type)
	assert.Equal(t, "build it", sub.Content)
	assert.Equal(t, "v", sub.Metadata["k"])

	tool := AgentTool{Name: "read", Description: "read a file"}
	assert.Equal(t, "read", tool.Name)
	assert.Equal(t, "read a file", tool.Description)

	sess := Session{ID: "s1", Messages: []AgentMessage{msg}, CreatedAt: now}
	assert.Len(t, sess.Messages, 1)
	assert.Equal(t, "s1", sess.ID)
	assert.Equal(t, now, sess.CreatedAt)
}

func TestEnumString(t *testing.T) {
	assert.Equal(t, "user", SubmissionUserMessage.String())
	assert.Equal(t, "steering", SubmissionSteering.String())
	assert.Equal(t, "followup", SubmissionFollowUp.String())

	assert.Equal(t, "discard_oldest", DiscardOldest.String())
	assert.Equal(t, "discard_newest", DiscardNewest.String())
	assert.Equal(t, "block", BlockUntilConsumed.String())

	assert.Equal(t, "allow", ClassificationAllow.String())
	assert.Equal(t, "deny", ClassificationDeny.String())
	assert.Equal(t, "require_approval", ClassificationRequireApproval.String())
}

func TestEventStreamContract(t *testing.T) {
	stream := NewEventStream(1)
	sent := AgentEvent{Kind: "status", Content: "started"}

	assert.NoError(t, stream.Send(sent))
	stream.Close()

	assert.Equal(t, sent, <-stream.Events())

	_, err := stream.Result()
	assert.ErrorIs(t, err, errNoResult)
	assert.NoError(t, stream.Err())
}
