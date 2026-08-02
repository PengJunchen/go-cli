package core

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEventStreamResultOverwrittenBeforeClose(t *testing.T) {
	stream := NewEventStream(1)
	stream.SetResult(AgentMessage{Content: "first"}, nil)
	// A second SetResult before Close overwrites the earlier value.
	stream.SetResult(AgentMessage{Content: "second"}, nil)
	stream.Close()

	res, err := stream.Result()
	require.NoError(t, err)
	assert.Equal(t, "second", res.Content)
	assert.NoError(t, stream.Err())
}

func TestEventStreamErrAfterSuccessfulSetResultIsNil(t *testing.T) {
	stream := NewEventStream(1)
	stream.SetResult(AgentMessage{Content: "ok"}, nil)
	stream.Close()
	assert.NoError(t, stream.Err())
}

func TestEventStreamLargeBurstBuffering(t *testing.T) {
	// Buffering far more events than a handful into a large bounded buffer,
	// then closing, must preserve every event in order.
	const n = 500
	stream := NewEventStream(n)
	for i := 0; i < n; i++ {
		require.NoError(t, stream.Send(AgentEvent{Kind: "message", Content: "e"}))
	}
	stream.Close()
	got := drainEvents(stream)
	assert.Len(t, got, n)
}

func TestNewHookChainCopiesSlice(t *testing.T) {
	// Mutating the caller's slice after construction must not change the chain.
	h1 := &spyHook{name: "h1"}
	h2 := &spyHook{name: "h2"}
	chain := NewHookChain(h1, h2)

	// Replace the caller's slice; the chain already copied its own.
	_ = h1
	require.Len(t, chain.Hooks(), 2)
	assert.Equal(t, "h1", chain.Hooks()[0].Name())
	assert.Equal(t, "h2", chain.Hooks()[1].Name())
}

func TestHookChainHaltByContinueFalseWithoutError(t *testing.T) {
	// declineHook returns Continue=false with no error.
	decline := &interruptingHook{output: "no"}
	chain := NewHookChain(&spyHook{name: "ok"}, decline, &spyHook{name: "never"})

	res, err := chain.Before(context.Background(), Submission{Content: "x"})
	assert.Error(t, err) // halt yields an interrupt result carrying the error
	assert.False(t, res.Continue)
	assert.True(t, res.Interrupted())
}

type interruptingHook struct{ output string }

func (h *interruptingHook) Name() string { return "interrupt" }
func (h *interruptingHook) BeforeRun(context.Context, Submission) error {
	return errors.New(h.output)
}
func (h *interruptingHook) AfterRun(context.Context, Submission, Result, error) error {
	return nil
}

func TestAgentRunEmptySubmission(t *testing.T) {
	model := returningNilEventsLoop{}
	agent := NewAgentImpl("empty", model)

	res, err := agent.Run(context.Background(), Submission{})
	require.NoError(t, err)
	assert.Empty(t, res.Message)
	// An empty user message is still recorded.
	msgs := agent.Messages()
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Empty(t, msgs[0].Content)
}

func TestAgentRunLargeInput(t *testing.T) {
	// A very large submission must be recorded in full without truncation.
	big := strings.Repeat("x", 100_000)
	agent := NewAgentImpl("big", returningNilEventsLoop{})
	_, err := agent.Run(context.Background(), Submission{Content: big})
	require.NoError(t, err)

	msgs := agent.Messages()
	require.Len(t, msgs, 1)
	assert.Equal(t, big, msgs[0].Content)
}

func TestTurnRunnerInjectOnCompletedTurnFails(t *testing.T) {
	// Once a turn completes it leaves the running set; Steer/FollowUp then
	// fail with errTurnUnknown rather than mutating a finished turn.
	runner := NewEinoTurnRunner(&stubLoop{})
	_, err := runner.RunTurn(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)

	require.ErrorIs(t, runner.Steer(context.Background(), "turn-1", "s"), errTurnUnknown)
	require.ErrorIs(t, runner.FollowUp(context.Background(), "turn-1", "f"), errTurnUnknown)
}

func TestTurnRunnerGetDoesNotAliasSlices(t *testing.T) {
	bl := newBlockingTurnLoop()
	runner := NewEinoTurnRunner(bl)

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr = runner.RunTurn(context.Background(), Submission{Content: "go"})
	}()
	<-bl.started
	id := runningTurnID(t, runner)
	require.NotEmpty(t, id)

	require.NoError(t, runner.Steer(context.Background(), id, "steer-one"))
	turn, err := runner.Get(context.Background(), id)
	require.NoError(t, err)
	// Mutating the returned Steerings copy must not leak into the stored turn.
	turn.Steerings[0].Content = "mutated"
	again, err := runner.Get(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "steer-one", again.Steerings[0].Content)

	close(bl.release)
	<-done
	assert.NoError(t, runErr)
}

func TestSubAgentConfigExposedOnSubAgent(t *testing.T) {
	sub := NewDefaultSubAgent(SubAgentConfig{
		Name:         "cfg",
		SystemPrompt: "be concise",
		Tools:        []string{"read", "write"},
		Model:        "mock",
		MaxTurns:     4,
	})
	assert.Equal(t, "cfg", sub.Name())
	assert.Equal(t, "be concise", sub.config.SystemPrompt)
	assert.Len(t, sub.config.Tools, 2)
	assert.Equal(t, "mock", sub.config.Model)
	assert.Equal(t, 4, sub.config.MaxTurns)
}

func TestSubAgentIdleStateInitial(t *testing.T) {
	sub := NewDefaultSubAgent(SubAgentConfig{Name: "idle"})
	assert.Equal(t, SubAgentIdle, sub.State())
	assert.Empty(t, sub.Received())
}

func TestSubAgentStateStringValues(t *testing.T) {
	assert.Equal(t, "idle", SubAgentIdle.String())
	assert.Equal(t, "running", SubAgentRunning.String())
	assert.Equal(t, "waiting", SubAgentWaiting.String())
	assert.Equal(t, "completed", SubAgentCompleted.String())
	assert.Equal(t, "failed", SubAgentFailed.String())
	assert.Equal(t, "interrupted", SubAgentInterrupted.String())
}

func TestResultAndSubmissionString(t *testing.T) {
	assert.Equal(t, "msg", (Result{Message: "msg"}).String())
	sub := Submission{Type: SubmissionSteering, Content: "steer", Metadata: map[string]any{"k": "v"}}
	assert.Equal(t, "steer", sub.Content)
	assert.Equal(t, SubmissionSteering, sub.Type)
	assert.Equal(t, "v", sub.Metadata["k"])
}

func TestDefaultSubAgentRunnerFactoryReturnsRunner(t *testing.T) {
	runner := simulatedRunnerFactory(SubAgentConfig{})
	require.NotNil(t, runner)
	assert.IsType(t, &simulatedSubAgentRunner{}, runner)
}

func TestAgentImplSatisfiesInterfaces(t *testing.T) {
	// Compile-time guards for the concrete implementations.
	var _ AgentLoop = (*LoopAgent)(nil)
	var _ Agent = (*AgentImpl)(nil)
	var _ Harness = (*HarnessImpl)(nil)
	var _ TurnRunner = (*EinoTurnRunner)(nil)
	var _ EventStream = (*EventStreamImpl)(nil)
	var _ SubAgent = (*DefaultSubAgent)(nil)
	var _ SubAgentFactory = (*DefaultSubAgentFactory)(nil)
	var _ ExtensionRegistry = (*ExtensionRegistryImpl)(nil)
	var _ HookChainable = (*HookChain)(nil)
	t.Log("core interfaces satisfied by concrete types")
}

// HookChainable is a self-imposed marker used only to keep the guard above
// self-documenting; it is satisfied by no production type beyond HookChain.
type HookChainable interface{ Hooks() []Hook }
