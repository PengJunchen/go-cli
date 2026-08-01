package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestTurnRunnerRunCompletes(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"TR-01", "single",
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model))
	runner := NewEinoTurnRunner(loop)

	result, err := runner.RunTurn(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "done", result.Message)
	assert.Equal(t, 1, model.CallCount())

	turn, err := runner.Get(context.Background(), "turn-1")
	require.NoError(t, err)
	assert.Equal(t, TurnCompleted, turn.Status)
	assert.False(t, turn.Canceled)
	assert.True(t, turn.Done())
	assert.False(t, turn.StartTime.IsZero())
	assert.False(t, turn.EndTime.IsZero())
}

func TestTurnRunnerNilLoopFails(t *testing.T) {
	runner := NewEinoTurnRunner(nil)
	_, err := runner.RunTurn(context.Background(), Submission{Content: "hi"})
	require.Error(t, err)
	assert.ErrorIs(t, err, errNilModel)
}

func TestTurnRunnerGetUnknown(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})
	_, err := runner.Get(context.Background(), "ghost")
	require.Error(t, err)
	assert.ErrorIs(t, err, errTurnUnknown)
}

func TestTurnRunnerCancelMidRun(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	bl := newBlockingTurnLoop()
	runner := NewEinoTurnRunner(bl)

	var result Result
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		result, runErr = runner.RunTurn(context.Background(), Submission{Content: "hi"})
	}()

	<-bl.started // loop began => turn is registered and running
	id := runningTurnID(t, runner)
	require.NotEmpty(t, id)

	require.NoError(t, runner.Cancel(context.Background(), id))
	<-done

	assert.Error(t, runErr)
	assert.True(t, errors.Is(runErr, context.Canceled))
	assert.False(t, result.Success)

	turn, err := runner.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, TurnCanceled, turn.Status)
	assert.True(t, turn.Canceled)
	assert.True(t, turn.Done())
}

func TestTurnRunnerSteerAndFollowUp(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	bl := newBlockingTurnLoop()
	runner := NewEinoTurnRunner(bl)

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr = runner.RunTurn(context.Background(), Submission{Content: "hi"})
	}()

	<-bl.started
	id := runningTurnID(t, runner)
	require.NotEmpty(t, id)

	require.NoError(t, runner.Steer(context.Background(), id, "reassess the plan"))
	require.NoError(t, runner.FollowUp(context.Background(), id, "clarify the budget"))

	turn, err := runner.Get(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, turn.Steerings, 1)
	assert.Equal(t, SubmissionSteering, turn.Steerings[0].Type)
	assert.Equal(t, "reassess the plan", turn.Steerings[0].Content)
	require.Len(t, turn.FollowUps, 1)
	assert.Equal(t, SubmissionFollowUp, turn.FollowUps[0].Type)
	assert.Equal(t, "clarify the budget", turn.FollowUps[0].Content)

	close(bl.release)
	<-done
	assert.NoError(t, runErr)
}

func TestTurnRunnerActOnInactiveTurnFails(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})
	require.Error(t, runner.Steer(context.Background(), "nope", "x"))
	require.Error(t, runner.FollowUp(context.Background(), "nope", "y"))
	require.Error(t, runner.Cancel(context.Background(), "nope"))
}

func TestTurnRunnerGetReturnsCopy(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})
	_, err := runner.RunTurn(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	first, err := runner.Get(context.Background(), "turn-1")
	require.NoError(t, err)
	// Mutate the returned copy and confirm the stored turn is unchanged.
	first.Steerings = append(first.Steerings, Submission{Content: "mutated"})
	assert.Len(t, first.Steerings, 1)

	again, err := runner.Get(context.Background(), "turn-1")
	require.NoError(t, err)
	assert.False(t, again.Canceled)
	assert.Empty(t, again.Steerings)
}

func TestTurnRunnerConcurrency(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	model := mock.NewMockLLMServer(nil)
	loop := NewLoopAgent(WithLLM(model))
	runner := NewEinoTurnRunner(loop)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			res, err := runner.RunTurn(context.Background(), Submission{Content: "msg"})
			assert.NoError(t, err)
			assert.True(t, res.Success)
		}(i)
	}
	wg.Wait()

	runner.mu.Lock()
	defer runner.mu.Unlock()
	assert.Len(t, runner.turns, n)
	seen := map[string]bool{}
	for id, turn := range runner.turns {
		assert.False(t, seen[id])
		seen[id] = true
		assert.Equal(t, TurnCompleted, turn.Status)
	}
	assert.Empty(t, runner.running) // no turn left running
}

func TestTurnRunnerErrorLoop(t *testing.T) {
	sentinel := errors.New("boom")
	runner := NewEinoTurnRunner(&errorLoop{err: sentinel})
	_, err := runner.RunTurn(context.Background(), Submission{Content: "go"})
	require.ErrorIs(t, err, sentinel)

	turn, err2 := runner.Get(context.Background(), "turn-1")
	require.NoError(t, err2)
	assert.Equal(t, TurnFailed, turn.Status)
	assert.ErrorIs(t, turn.Err, sentinel)
}

// blockingTurnLoop blocks its Run until ctx is canceled or release is closed,
// and signals via started when Run begins.
type blockingTurnLoop struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingTurnLoop() *blockingTurnLoop {
	return &blockingTurnLoop{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingTurnLoop) Run(ctx context.Context, _ Submission) ([]AgentEvent, error) {
	close(b.started)
	select {
	case <-ctx.Done():
		return []AgentEvent{}, ctx.Err()
	case <-b.release:
		return []AgentEvent{{Kind: "message", Content: "ok", Timestamp: time.Now()}}, nil
	}
}

// runningTurnID returns the id of the currently active running turn, or ""
// if none. It reaches into the runner's internal state (same package).
func runningTurnID(t *testing.T, runner *EinoTurnRunner) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runner.mu.Lock()
		for id, turn := range runner.turns {
			if turn.Status == TurnRunning {
				runner.mu.Unlock()
				return id
			}
		}
		runner.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return ""
}

// errorLoop returns a fixed error on every Run.
type errorLoop struct{ err error }

func (l *errorLoop) Run(_ context.Context, _ Submission) ([]AgentEvent, error) {
	return nil, l.err
}
