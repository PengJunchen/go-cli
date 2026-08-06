package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

func TestTurnStatusString(t *testing.T) {
	tests := []struct {
		status TurnStatus
		want   string
	}{
		{TurnPending, "pending"},
		{TurnRunning, "running"},
		{TurnCompleted, "completed"},
		{TurnCanceled, "canceled"},
		{TurnFailed, "failed"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.status.String(), "status %d", int(tt.status))
	}
}

func TestTurnStatusInvalidFallsBackToPending(t *testing.T) {
	// An out-of-range status value maps to the default "pending".
	assert.Equal(t, "pending", TurnStatus(99).String())
}

func TestTurnDoneTable(t *testing.T) {
	tests := []struct {
		name   string
		status TurnStatus
		want   bool
	}{
		{"pending", TurnPending, false},
		{"running", TurnRunning, false},
		{"completed", TurnCompleted, true},
		{"canceled", TurnCanceled, true},
		{"failed", TurnFailed, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			turn := Turn{Status: tt.status}
			assert.Equal(t, tt.want, turn.Done())
		})
	}
}

func TestTurnZeroValueNotDone(t *testing.T) {
	var turn Turn
	assert.False(t, turn.Done())
	assert.Equal(t, TurnPending, turn.Status)
}

func TestTurnFullConstruction(t *testing.T) {
	now := time.Now()
	turn := Turn{
		ID:         "t1",
		Submission: Submission{Content: "hello"},
		Status:     TurnCompleted,
		StartTime:  now,
		EndTime:    now,
		Result:     Result{Message: "done", Success: true},
		Steerings:  []Submission{{Type: SubmissionSteering, Content: "steer"}},
		FollowUps:  []Submission{{Type: SubmissionFollowUp, Content: "follow"}},
	}
	assert.True(t, turn.Done())
	assert.Equal(t, "t1", turn.ID)
	assert.Equal(t, "done", turn.Result.Message)
	assert.Len(t, turn.Steerings, 1)
	assert.Len(t, turn.FollowUps, 1)
}

func TestTurnRunnerNewIDIsMonotonic(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})
	seen := map[string]bool{}
	// newID must be unique and distinct across calls.
	for i := 0; i < 50; i++ {
		id := runner.newID()
		assert.False(t, seen[id], "duplicate id %q", id)
		seen[id] = true
		require.NotEmpty(t, id)
	}
}

func TestTurnRunnerGetNotRegisteredReturnsError(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})
	_, err := runner.Get(context.Background(), "ghost")
	require.ErrorIs(t, err, errTurnUnknown)
}

func TestTurnRunnerCancelUnknownTurn(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})
	require.ErrorIs(t, runner.Cancel(context.Background(), "ghost"), errTurnUnknown)
}

func TestTurnRunnerNewLoopReporting(t *testing.T) {
	// Construction with a non-nil loop is a valid, logging no-op path.
	r := NewEinoTurnRunner(&stubLoop{})
	require.NotNil(t, r)
}

// extraErrorLoop returns a fixed error and no events, like errorLoop but for
// result-message derivation edge coverage.
type extraErrorLoop struct{}

func (extraErrorLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return nil, errNilModel
}

func TestTurnRunnerNilEventsYieldEmptyResult(t *testing.T) {
	runner := NewEinoTurnRunner(extraErrorLoop{})
	res, err := runner.RunTurn(context.Background(), Submission{Content: "go"})
	require.ErrorIs(t, err, errNilModel)
	assert.False(t, res.Success)
	assert.Empty(t, res.Message)
}

func TestTurnRunnerCancelEmitsSpan(t *testing.T) {
	exp := &captureExporter{}
	tr := tracing.NewTracer("", exp)
	root, tctx := tr.Start(context.Background(), "root.test", tracing.SpanKindInternal)
	defer root.End()

	runner := NewEinoTurnRunner(&stubLoop{})
	// Canceling an unknown/inactive turn emits a span with an error status.
	require.Error(t, runner.Cancel(tctx, "ghost"))
	errSpan := waitForSpanStatus(t, exp, "turn.cancel", tracing.SpanStatusError)
	assert.NotZero(t, errSpan)

	// A real cancel against a running turn emits a span with an OK status.
	bl := newBlockingTurnLoop()
	runner = NewEinoTurnRunner(bl)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runner.RunTurn(tctx, Submission{Content: "hi"}) //nolint:errcheck // error checked after <-done via Canceled state
	}()

	<-bl.started // loop began => turn is registered and running
	id := runningTurnID(t, runner)
	require.NotEmpty(t, id)

	require.NoError(t, runner.Cancel(tctx, id))
	<-done

	span := waitForSpanStatus(t, exp, "turn.cancel", tracing.SpanStatusOK)
	val, ok := attrValue(span.Attributes, "turn_id")
	assert.True(t, ok)
	assert.Equal(t, id, val)
}

// waitForSpanStatus polls until a span with the given name and status is
// exported. Spans are exported asynchronously.
func waitForSpanStatus(t *testing.T, exp *captureExporter, name string, status tracing.SpanStatus) tracing.SpanData {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, span := range exp.all() {
			if span.Name == name && span.Status == status {
				return span
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("span with name %q and status %q not exported; got %d spans", name, status, len(exp.all()))
	return tracing.SpanData{}
}
