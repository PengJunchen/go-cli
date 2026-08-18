package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestNoopRunSlotGuard_ClaimImmediate verifies that the noop guard's ClaimRun
// returns immediately without blocking.
func TestNoopRunSlotGuard_ClaimImmediate(t *testing.T) {
	g := noopRunSlotGuard{}

	done := make(chan struct{})
	go func() {
		_, _ = g.ClaimRun(context.Background())
		close(done)
	}()

	select {
	case <-done:
		// success: returned immediately
	case <-time.After(time.Second):
		t.Fatal("noopRunSlotGuard.ClaimRun did not return within 1s")
	}
}

// TestNoopRunSlotGuard_ExecutePassthrough verifies that ExecuteClaimedRun
// calls fn directly and returns its result.
func TestNoopRunSlotGuard_ExecutePassthrough(t *testing.T) {
	g := noopRunSlotGuard{}
	claim, err := g.ClaimRun(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "noop", claim.ID)

	sentinel := errors.New("fn executed")
	err = g.ExecuteClaimedRun(claim, func() error {
		return sentinel
	})
	assert.ErrorIs(t, err, sentinel)

	// Release must be a no-op and not panic.
	g.Release(claim)
}

// TestHarness_ZeroConfigCompat verifies that a harness created without
// WithRunSlotGuard (using the noop default) works normally for concurrent
// submissions.
func TestHarness_ZeroConfigCompat(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"ZC-01", "zero-config",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
	loop := NewLoopAgent(WithLLM(model))
	agent := NewAgentImpl("h", loop)
	h := NewHarnessImpl(agent)

	// No runSlot configured; should default to noop and allow concurrent runs.
	stream1, err := h.Submit(context.Background(), "first")
	require.NoError(t, err)
	require.NotNil(t, stream1)

	stream2, err := h.Submit(context.Background(), "second")
	require.NoError(t, err)
	require.NotNil(t, stream2)

	// Both runs should complete normally.
	drainEvents(stream1)
	drainEvents(stream2)
}

// TestHarness_ConcurrentSubmitFailsFast verifies that with a
// DefaultRunSlotGuard, a second concurrent Submit fails within a bounded time
// because the slot is already held.
func TestHarness_ConcurrentSubmitFailsFast(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Use a blocking loop so the first run holds the slot indefinitely.
	bl := newBlockingTurnLoop()

	agent := NewAgentImpl("h", bl)
	guard := NewDefaultRunSlotGuard()
	h := NewHarnessImpl(agent, WithRunSlotGuard(guard))

	// First Submit claims the slot and starts the (blocking) run.
	stream1, err := h.Submit(context.Background(), "first")
	require.NoError(t, err)
	require.NotNil(t, stream1)

	// Wait for the blocking loop to start so the first run is in progress.
	<-bl.started

	// Second Submit should fail because the slot is held by the first run.
	start := time.Now()
	_, err = h.Submit(context.Background(), "second")
	elapsed := time.Since(start)

	require.Error(t, err)
	// Must fail well within 300ms (the 200ms claim timeout plus some slack).
	assert.Less(t, elapsed, 300*time.Millisecond, "second Submit should fail fast")

	// Release the blocking loop so the first run can complete, then drain.
	close(bl.release)
	drainEvents(stream1)
}

// TestHarness_RunCompletesReleasesSlot verifies that after a run completes
// (releasing the slot), a subsequent Submit succeeds immediately.
func TestHarness_RunCompletesReleasesSlot(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"RC-01", "release",
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model))
	agent := NewAgentImpl("h", loop)
	guard := NewDefaultRunSlotGuard()
	h := NewHarnessImpl(agent, WithRunSlotGuard(guard))

	// First run completes normally.
	stream1, err := h.Submit(context.Background(), "first")
	require.NoError(t, err)
	drainEvents(stream1)

	// Second run should succeed immediately because the slot was released.
	start := time.Now()
	stream2, err := h.Submit(context.Background(), "second")
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, 100*time.Millisecond, "second Submit should succeed immediately")

	drainEvents(stream2)
}

// blockingTool blocks until release is closed, then returns a fixed result.
// This allows tests to control timing between LLM iterations.
type blockingTool struct {
	name    string
	release chan struct{}
	started chan struct{}
}

func newBlockingTool(name string) *blockingTool {
	return &blockingTool{
		name:    name,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
}

func (t *blockingTool) Name() string        { return t.name }
func (t *blockingTool) Description() string { return "blocking test tool" }
func (t *blockingTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *blockingTool) Execute(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	select {
	case <-t.started:
	default:
		close(t.started)
	}
	select {
	case <-t.release:
		return &tools.ToolResult{Output: "tool done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var _ tools.ToolDefinition = (*blockingTool)(nil)
var _ tools.Parameterized = (*blockingTool)(nil)

// TestFollowUpInjection_ThroughLoop verifies that a follow-up message sent to
// the followUpCh is injected as a RoleUser message in the next LLM iteration.
func TestFollowUpInjection_ThroughLoop(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("file contents")
	require.NoError(t, err)

	// Use a blocking tool to control timing: the first LLM turn issues a tool
	// call. While the tool blocks, we send a follow-up to followUpCh. The tool
	// then releases, and the loop proceeds to the second iteration where it
	// drains the follow-up channel before the next LLM call.
	bt := newBlockingTool("read_file")
	reg := scriptedRegistry(bt)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"FU-01", "followup",
		mock.ConversationTurn{
			AssistantContent: "let me read",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c1", Name: "read_file", Args: map[string]any{}},
			},
		},
		mock.ConversationTurn{AssistantContent: "final answer"},
	))

	followUpCh := make(chan string, 16)
	loop := NewLoopAgent(WithLLM(model), WithTools(reg), WithFollowUpChannel(followUpCh))

	var runErr error
	var events []AgentEvent
	done := make(chan struct{})
	go func() {
		defer close(done)
		events, runErr = loop.Run(context.Background(), Submission{Content: "start"})
	}()

	// Wait for the tool to start executing (the first LLM call has completed).
	<-bt.started

	// Send a follow-up message while the tool is blocking.
	followUpCh <- "remember to check edge cases"

	// Release the tool so the loop proceeds to the second iteration.
	close(bt.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not complete within 5s")
	}

	require.NoError(t, runErr)
	require.Equal(t, 2, model.CallCount())

	// The second LLM call must contain the follow-up as a user message.
	secondCall := model.CallLog()[1]
	var foundFollowUp bool
	for _, msg := range secondCall.Messages {
		if msg.Role == llm.RoleUser && msg.Content == "remember to check edge cases" {
			foundFollowUp = true
		}
	}
	assert.True(t, foundFollowUp, "follow-up message was not injected as a user message in the second LLM call")

	// The loop still completes with the final answer.
	messages := findEvents(events, "message")
	require.NotEmpty(t, messages)
	assert.Equal(t, "final answer", messages[len(messages)-1])
}

// TestFollowUp_NilChannel_NoOp verifies that calling FollowUp when no
// follow-up channel is set returns nil and does not panic.
func TestFollowUp_NilChannel_NoOp(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	bl := newBlockingTurnLoop()
	runner := NewEinoTurnRunner(bl)
	// No SetFollowUpChannel called, so followUpCh is nil.

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr = runner.RunTurn(context.Background(), Submission{Content: "hi"})
	}()

	<-bl.started
	id := runningTurnID(t, runner)
	require.NotEmpty(t, id)

	// FollowUp with nil channel should not panic and should record on the turn.
	err := runner.FollowUp(context.Background(), id, "are you done?")
	require.NoError(t, err)

	turn, err := runner.Get(context.Background(), id)
	require.NoError(t, err)
	require.Len(t, turn.FollowUps, 1)
	assert.Equal(t, "are you done?", turn.FollowUps[0].Content)

	close(bl.release)
	<-done
	assert.NoError(t, runErr)
}

// TestSteer_ExistingRegression verifies that the steering mechanism still
// works after the follow-up channel changes.
func TestSteer_ExistingRegression(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("content")
	require.NoError(t, err)

	bt := newBlockingTool("read_file")
	reg := scriptedRegistry(bt)

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"ST-01", "steer-regression",
		mock.ConversationTurn{
			AssistantContent: "reading",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c1", Name: "read_file", Args: map[string]any{}},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))

	steerCh := make(chan string, 16)
	followUpCh := make(chan string, 16)
	loop := NewLoopAgent(
		WithLLM(model),
		WithTools(reg),
		WithSteeringChannel(steerCh),
		WithFollowUpChannel(followUpCh),
	)

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr = loop.Run(context.Background(), Submission{Content: "go"})
	}()

	<-bt.started

	// Send a steering instruction while the tool blocks.
	steerCh <- "reassess the approach"
	// Also send a follow-up to verify both channels work together.
	followUpCh <- "check the logs too"

	close(bt.release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not complete within 5s")
	}

	require.NoError(t, runErr)
	require.Equal(t, 2, model.CallCount())

	secondCall := model.CallLog()[1]
	var foundSteer, foundFollowUp bool
	for _, msg := range secondCall.Messages {
		if msg.Role == llm.RoleUser && msg.Content == "reassess the approach" {
			foundSteer = true
		}
		if msg.Role == llm.RoleUser && msg.Content == "check the logs too" {
			foundFollowUp = true
		}
	}
	assert.True(t, foundSteer, "steering message was not injected")
	assert.True(t, foundFollowUp, "follow-up message was not injected")
}

// TestTurnRunner_RunSlotGuard_ConcurrentTurnFails verifies that the
// TurnRunner with a DefaultRunSlotGuard rejects a second concurrent turn.
func TestTurnRunner_RunSlotGuard_ConcurrentTurnFails(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	bl := newBlockingTurnLoop()
	runner := NewEinoTurnRunner(bl)
	runner.SetRunSlotGuard(NewDefaultRunSlotGuard())

	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, firstErr = runner.RunTurn(context.Background(), Submission{Content: "first"})
	}()

	<-bl.started

	// Second turn should fail because the slot is held by the first.
	_, err := runner.RunTurn(context.Background(), Submission{Content: "second"})
	require.Error(t, err)

	close(bl.release)
	<-done
	assert.NoError(t, firstErr)
}
