package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTurnRunnerSteerDoesNotRecordWhenQueueFull(t *testing.T) {
	loop := newBlockingTurnLoop()
	runner := NewEinoTurnRunner(loop)
	steer := make(chan string, 1)
	steer <- "already queued"
	runner.SetSteerChannel(steer)

	done := startBlockedRun(t, runner, loop)
	id := runningTurnID(t, runner)

	require.Error(t, runner.Steer(context.Background(), id, "not accepted"))
	turn, err := runner.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Empty(t, turn.Steerings)
	assert.Equal(t, "already queued", <-steer)

	close(loop.release)
	<-done
}

func TestTurnRunnerFollowUpDoesNotRecordWhenQueueFull(t *testing.T) {
	loop := newBlockingTurnLoop()
	runner := NewEinoTurnRunner(loop)
	followUps := make(chan string, 1)
	followUps <- "already queued"
	runner.SetFollowUpChannel(followUps)

	done := startBlockedRun(t, runner, loop)
	id := runningTurnID(t, runner)

	require.Error(t, runner.FollowUp(context.Background(), id, "not accepted"))
	turn, err := runner.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Empty(t, turn.FollowUps)
	assert.Equal(t, "already queued", <-followUps)

	close(loop.release)
	<-done
}

func TestTurnRunnerInputRequiresDeliveryChannel(t *testing.T) {
	loop := newBlockingTurnLoop()
	runner := NewEinoTurnRunner(loop)
	done := startBlockedRun(t, runner, loop)
	id := runningTurnID(t, runner)

	require.Error(t, runner.Steer(context.Background(), id, "no queue"))
	require.Error(t, runner.FollowUp(context.Background(), id, "no queue"))
	turn, err := runner.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Empty(t, turn.Steerings)
	assert.Empty(t, turn.FollowUps)

	close(loop.release)
	<-done
}

func TestTurnRunnerGetDeepCopiesSubmission(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})
	submission := Submission{
		Content:  "hello",
		Metadata: map[string]any{"label": "original"},
		History:  []AgentMessage{{Role: "user", Content: "prior"}},
	}
	_, err := runner.RunTurn(context.Background(), submission)
	require.NoError(t, err)

	first, err := runner.Get(context.Background(), "turn-1")
	require.NoError(t, err)
	first.Submission.Metadata["label"] = "mutated"
	first.Submission.History[0].Content = "mutated"

	again, err := runner.Get(context.Background(), "turn-1")
	require.NoError(t, err)
	assert.Equal(t, "original", again.Submission.Metadata["label"])
	assert.Equal(t, "prior", again.Submission.History[0].Content)
}

func startBlockedRun(t *testing.T, runner *EinoTurnRunner, loop *blockingTurnLoop) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runner.RunTurn(context.Background(), Submission{Content: "start"})
	}()
	select {
	case <-loop.started:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	t.Cleanup(func() {
		select {
		case <-loop.release:
		default:
			close(loop.release)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("turn did not stop")
		}
	})
	return done
}
