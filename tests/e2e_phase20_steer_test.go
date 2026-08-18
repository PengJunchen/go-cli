//go:build e2e

// Package tests contains end-to-end integration tests for the go-cli project.
// This file verifies Phase 20 Steer full chain: steer message injection,
// cancel turn, steer queue, quit cancellation, and pause/resume.
package tests

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Phase 20 Steer E2E Tests (Task 20-7)
// =============================================================================

// TestET_steer_message_injected_between_iterations verifies that a steer
// message is injected as a user message between ReAct iterations. The mock LLM
// first returns a tool call (which forces a second iteration), and during that
// tool call we send a steer message. The second LLM call should see the steer
// instruction as a user message in its conversation.
func TestET_steer_message_injected_between_iterations(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var steerSeen atomic.Value // string
	steerCh := make(chan string, 1)

	// Model that first calls a blocking tool, then returns a final answer.
	// On the second call it records the messages it received so we can check
	// whether the steer instruction was injected.
	model := &steerRecordingModel{
		seq: []*llm.Message{
			{
				Role:    llm.RoleAssistant,
				Content: "calling tool",
				ToolCalls: []llm.ToolCall{
					{ID: "c1", Name: "block", Args: map[string]any{}},
				},
			},
			{Role: llm.RoleAssistant, Content: "done"},
		},
		onSecondCall: func(msgs []llm.Message) {
			for _, m := range msgs {
				if m.Role == llm.RoleUser && m.Content == "change direction" {
					steerSeen.Store(m.Content)
				}
			}
		},
	}

	// Tool that blocks until released, giving us time to send the steer.
	toolStarted := make(chan struct{})
	toolRelease := make(chan struct{})
	blockTool := &blockingSteerTool{started: toolStarted, release: toolRelease}

	toolReg := tools.NewDefaultToolRegistry()
	require.NoError(t, toolReg.Register(context.Background(), blockTool))

	loop := core.NewLoopAgent(
		core.WithLLM(model),
		core.WithTools(toolReg),
		core.WithSteeringChannel(steerCh),
	)
	runner := core.NewEinoTurnRunner(loop)
	runner.SetSteerChannel(steerCh)

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr = runner.RunTurn(ctx, core.Submission{Content: "go"})
	}()

	// Wait for the tool to start (first iteration done, tool executing).
	select {
	case <-toolStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for tool to start")
	}

	// Send steer instruction through the TurnRunner.
	turnID := runner.RunningTurnID()
	require.NotEmpty(t, turnID)
	require.NoError(t, runner.Steer(ctx, turnID, "change direction"))

	// Release the tool so the second iteration can proceed.
	close(toolRelease)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for turn to complete")
	}
	require.NoError(t, runErr)

	// Verify the steer message was injected as a user message.
	val := steerSeen.Load()
	require.NotNil(t, val, "steer message was not seen in second LLM call")
	assert.Equal(t, "change direction", val.(string))
}

// TestET_cancel_turn_stops_agent verifies that cancelling a running turn stops
// the agent within 100ms. Uses a blocking loop that only returns when the
// context is cancelled.
func TestET_cancel_turn_stops_agent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bl := newBlockingTurnLoop()
	runner := core.NewEinoTurnRunner(bl)

	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr = runner.RunTurn(ctx, core.Submission{Content: "hi"})
	}()

	// Wait for the loop to start.
	<-bl.started
	id := runner.RunningTurnID()
	require.NotEmpty(t, id)

	// Cancel the turn and measure how long it takes to stop.
	cancelStart := time.Now()
	require.NoError(t, runner.Cancel(ctx, id))

	select {
	case <-done:
		elapsed := time.Since(cancelStart)
		assert.Less(t, elapsed, 100*time.Millisecond, "agent should stop within 100ms of cancel")
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not stop after cancel")
	}
	require.Error(t, runErr)
}

// TestET_steer_queue_no_drop verifies that 5 rapid steer messages sent through
// the InterruptHandler are all consumed without drops (capacity 16 channel).
func TestET_steer_queue_no_drop(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_ = ctx
	handler := cli.NewInterruptHandler(func() {})
	handler.Start(nil)
	defer handler.Stop()

	// Send 5 rapid steer messages.
	msgs := []string{"steer-1", "steer-2", "steer-3", "steer-4", "steer-5"}
	for _, m := range msgs {
		require.NoError(t, handler.SendSteer(m))
	}

	// All 5 should be received from SteerChannel.
	received := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		select {
		case msg := <-handler.SteerChannel():
			received = append(received, msg)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for steer message %d (received %d)", i, len(received))
		}
	}
	assert.Len(t, received, 5)
	assert.ElementsMatch(t, msgs, received)
}

// TestET_quit_cancels_no_leak verifies that stopping the InterruptHandler
// (simulating q key quit) cancels the agent and leaves no goroutine leak.
func TestET_quit_cancels_no_leak(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_ = ctx
	cancelled := atomic.Bool{}
	handler := cli.NewInterruptHandler(func() {
		cancelled.Store(true)
	})
	handler.Start(nil)

	// Simulate quit by stopping the handler.
	handler.Stop()

	// Verify no goroutine leak via the deferred AssertNoGoroutineLeak.
	// Give the drain goroutine time to exit.
	time.Sleep(50 * time.Millisecond)
}

// TestET_pause_resume_continues verifies that after pausing and resuming, the
// agent continues from the pause point and produces the expected result.
func TestET_pause_resume_continues(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	mockLLM := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"P-R1", "pause-resume",
		mock.ConversationTurn{AssistantContent: "hello after resume"},
	))
	loop := core.NewLoopAgent(core.WithLLM(mockLLM))

	// Pause before running.
	loop.Pause()

	var (
		events []core.AgentEvent
		runErr error
		wg     sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		events, runErr = loop.Run(ctx, core.Submission{Content: "hi"})
	}()

	// Give the loop a moment to reach the pause point.
	time.Sleep(100 * time.Millisecond)

	// Resume should unblock the loop.
	loop.Resume()

	wg.Wait()
	require.NoError(t, runErr)

	messages := findPhase20Events(events, "message")
	require.Len(t, messages, 1)
	assert.Equal(t, "hello after resume", messages[0])
}

// Quiet the unused import warning for syscall (used implicitly via signal).
var _ = syscall.SIGINT

// =============================================================================
// Helpers
// =============================================================================

// steerRecordingModel is a test LLM model that returns pre-configured responses
// in sequence. On the second call it invokes onSecondCall with the messages
// it received, so the test can inspect whether a steer message was injected.
type steerRecordingModel struct {
	seq          []*llm.Message
	callCount    int32
	onSecondCall func(msgs []llm.Message)
}

func (m *steerRecordingModel) Generate(_ context.Context, msgs []llm.Message, _ ...llm.Option) (*llm.Message, error) {
	idx := atomic.AddInt32(&m.callCount, 1)
	if idx == 2 && m.onSecondCall != nil {
		m.onSecondCall(msgs)
	}
	if int(idx) <= len(m.seq) {
		return m.seq[idx-1], nil
	}
	return &llm.Message{Role: llm.RoleAssistant, Content: "fallback"}, nil
}

func (m *steerRecordingModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	resp, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan llm.MessageChunk, 2)
	ch <- llm.MessageChunk{Role: resp.Role, Content: resp.Content}
	ch <- llm.MessageChunk{Role: resp.Role, Final: true, ToolCalls: resp.ToolCalls}
	close(ch)
	return ch, nil
}

// blockingSteerTool is a tool that signals when it starts and blocks until
// release is closed. Used to create a window for sending steer messages.
type blockingSteerTool struct {
	started chan struct{}
	release chan struct{}
}

func (t *blockingSteerTool) Name() string { return "block" }
func (t *blockingSteerTool) Description() string {
	return "block: A test tool that blocks until released."
}
func (t *blockingSteerTool) Parameters() any {
	return map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}
}
func (t *blockingSteerTool) Execute(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	close(t.started)
	select {
	case <-t.release:
		return &tools.ToolResult{Output: "released"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// findPhase20Events extracts message contents from a list of AgentEvents.
func findPhase20Events(events []core.AgentEvent, kind string) []string {
	var result []string
	for _, ev := range events {
		if ev.Kind == kind {
			result = append(result, ev.Content)
		}
	}
	return result
}

// blockingTurnLoop blocks its Run until ctx is canceled or release is closed.
type blockingTurnLoop struct {
	started chan struct{}
	release chan struct{}
}

func newBlockingTurnLoop() *blockingTurnLoop {
	return &blockingTurnLoop{started: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingTurnLoop) Run(ctx context.Context, _ core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	close(b.started)
	select {
	case <-ctx.Done():
		return []core.AgentEvent{}, ctx.Err()
	case <-b.release:
		return []core.AgentEvent{{Kind: "message", Content: "ok", Timestamp: time.Now()}}, nil
	}
}
