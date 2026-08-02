package core

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// nonEventSourceAgent implements Agent but NOT eventSource, exercising the
// harness branch where only the done/result events are produced.
type nonEventSourceAgent struct{}

func (nonEventSourceAgent) Name() string { return "plain-agent" }

func (nonEventSourceAgent) Run(context.Context, Submission) (Result, error) {
	return Result{Message: "from-run", Success: true}, nil
}

func TestHarnessWithNonEventSourceAgent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	h := NewHarnessImpl(nonEventSourceAgent{}, WithEventBuffer(8))
	stream, err := h.Submit(context.Background(), "hi")
	require.NoError(t, err)

	events := drainEvents(stream)
	// No fan-out events; only the terminal "done" event is produced.
	done := findEvents(events, "done")
	require.Len(t, done, 1)
	assert.Equal(t, "from-run", done[0])
	assert.Empty(t, findEvents(events, "message"))

	res, rerr := stream.Result()
	require.NoError(t, rerr)
	assert.Equal(t, "from-run", res.Content)
	assert.NoError(t, stream.Err())
}

// failingEventSourceAgent fails its Run with a fixed error.
type failingEventSourceAgent struct{}

func (failingEventSourceAgent) Name() string { return "fail-agent" }

func (failingEventSourceAgent) Run(context.Context, Submission) (Result, error) {
	return Result{Success: false}, errNilModel
}

func (failingEventSourceAgent) Events() []AgentEvent { return nil }

func TestHarnessErrorPathEmitsErrorEvent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	h := NewHarnessImpl(failingEventSourceAgent{}, WithEventBuffer(8))
	stream, err := h.Submit(context.Background(), "hi")
	require.NoError(t, err)

	events := drainEvents(stream)
	errs := findEvents(events, "error")
	require.NotEmpty(t, errs)

	_, rerr := stream.Result()
	require.ErrorIs(t, rerr, errNilModel)
	assert.ErrorIs(t, stream.Err(), errNilModel)
}

func TestHarnessWithDiscardPolicyOption(t *testing.T) {
	// WithDiscardPolicy is accepted without error for API completeness.
	h := NewHarnessImpl(nonEventSourceAgent{}, WithDiscardPolicy(DiscardNewest))
	require.NotNil(t, h)
	assert.Equal(t, "harness.start", h.startSpanName)
}

func TestHarnessStartSpanOption(t *testing.T) {
	agent := &fakeEventStreamAgent{res: Result{Success: true}}
	h := NewHarnessImpl(agent, WithStartSpanName("custom.submit"))
	assert.Equal(t, "custom.submit", h.startSpanName)
}

func TestHarnessDefaultStartSpanName(t *testing.T) {
	agent := &fakeEventStreamAgent{res: Result{Success: true}}
	h := NewHarnessImpl(agent)
	assert.Equal(t, "harness.start", h.startSpanName)
}

func TestHarnessWithEventBufferNonPositiveIgnored(t *testing.T) {
	agent := &fakeEventStreamAgent{res: Result{Success: true}}
	h := NewHarnessImpl(agent, WithEventBuffer(0))
	assert.Equal(t, 0, h.bufferSize)
	// Negative is also ignored (fall back to default of 0 channels).
	h2 := NewHarnessImpl(agent, WithEventBuffer(-5))
	assert.Equal(t, 0, h2.bufferSize)
}
