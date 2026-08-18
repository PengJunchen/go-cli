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
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// Turns map bounded growth
// ---------------------------------------------------------------------------

func TestTurnRunner_TurnsMapBoundedGrowth(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})

	// Submit 200 turns sequentially. After each turn, pruneCompletedTurns
	// caps the map at maxTurnsHistory (100).
	for i := 0; i < 200; i++ {
		_, err := runner.RunTurn(context.Background(), Submission{Content: "turn"})
		require.NoError(t, err, "turn %d", i+1)
	}

	runner.mu.Lock()
	mapSize := len(runner.turns)
	runner.mu.Unlock()

	assert.LessOrEqual(t, mapSize, maxTurnsHistory,
		"turns map must not exceed maxTurnsHistory (%d), got %d", maxTurnsHistory, mapSize)

	// The oldest turns should have been pruned.
	_, err := runner.Get(context.Background(), "turn-1")
	require.Error(t, err)
	assert.ErrorIs(t, err, errTurnUnknown)

	// The most recent turn must still be retrievable.
	turn, err := runner.Get(context.Background(), "turn-200")
	require.NoError(t, err)
	assert.Equal(t, TurnCompleted, turn.Status)
	assert.False(t, turn.StartTime.IsZero())
	assert.False(t, turn.EndTime.IsZero())
}

// ---------------------------------------------------------------------------
// Sequential cancellation synthesizes tool_results
// ---------------------------------------------------------------------------

// cancelBlockingTool blocks its Execute call until ctx is cancelled, then
// returns ctx.Err(). It signals started when Execute is entered so tests can
// synchronize cancellation.
type cancelBlockingTool struct {
	name    string
	started chan struct{}
	once    sync.Once
}

func (b *cancelBlockingTool) Name() string        { return b.name }
func (b *cancelBlockingTool) Description() string { return "blocks until cancelled" }

func (b *cancelBlockingTool) Execute(ctx context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	b.once.Do(func() { close(b.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

var _ tools.ToolDefinition = (*cancelBlockingTool)(nil)

// hasToolResultEvent reports whether events contains a "tool_result" event
// with the given ToolCallID.
func hasToolResultEvent(events []AgentEvent, callID string) bool {
	for _, ev := range events {
		if ev.Kind == "tool_result" && ev.ToolCallID == callID {
			return true
		}
	}
	return false
}

func TestLoop_CancelSynthesizesToolResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	toolSrv := mock.NewMockToolServer()
	bt := &cancelBlockingTool{name: "blocker", started: make(chan struct{})}
	require.NoError(t, toolSrv.Register(context.Background(), bt))

	// Mock LLM: first response issues two tool calls, second response is
	// never reached because the context will be cancelled.
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"C-SEQ", "cancel-seq",
		mock.ConversationTurn{
			AssistantContent: "calling tools",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "tc-1", Name: "blocker"},
				{ID: "tc-2", Name: "blocker"},
			},
		},
	))

	loop := NewLoopAgent(WithLLM(model), WithTools(toolSrv))

	ctx, cancel := context.WithCancel(context.Background())
	var events []AgentEvent
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		events, runErr = loop.Run(ctx, Submission{Content: "run"})
	}()

	// Wait for the first tool to start executing, then cancel.
	select {
	case <-bt.started:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for tool to start")
	}
	cancel()
	<-done

	// The error must be context.Canceled (or a wrapper).
	require.Error(t, runErr)
	assert.True(t, errors.Is(runErr, context.Canceled),
		"expected context.Canceled, got %v", runErr)

	// Collect tool call IDs from "message" events (the assistant response
	// that contains ToolCalls).
	var toolCallIDs []string
	for _, ev := range events {
		if ev.Kind == "message" && len(ev.ToolCalls) > 0 {
			for _, tc := range ev.ToolCalls {
				toolCallIDs = append(toolCallIDs, tc.ID)
			}
		}
	}
	require.Len(t, toolCallIDs, 2, "expected 2 tool calls from the assistant")

	// Every tool call ID must have a matching tool_result event.
	for _, id := range toolCallIDs {
		assert.True(t, hasToolResultEvent(events, id),
			"no tool_result event for tool call %s", id)
		for _, ev := range events {
			if ev.Kind == "tool_result" && ev.ToolCallID == id {
				assert.Contains(t, ev.Content, "canceled",
					"tool_result for %s should mention canceled", id)
				assert.True(t, ev.IsError, "tool_result for %s should be an error", id)
				break
			}
		}
	}

	// Convert events to messages and verify the sequence is complete:
	// every tool_call has a matching tool_result message.
	msgs := eventsToTurnMessages(events)
	for _, id := range toolCallIDs {
		found := false
		for _, m := range msgs {
			if m.Role == "tool" && m.ToolCallID == id {
				found = true
				break
			}
		}
		assert.True(t, found,
			"no tool message for tool call %s in reconstructed messages", id)
	}
}

// ---------------------------------------------------------------------------
// Parallel cancellation synthesizes tool_results
// ---------------------------------------------------------------------------

func TestLoop_CancelParallelModeSynthesizesToolResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	toolSrv := mock.NewMockToolServer()

	// "fast" completes immediately and signals done; "slow" blocks until
	// cancelled. The fastDone channel lets the test wait for the fast tool
	// to finish before canceling, ensuring the parallel snapshot records
	// its real result rather than treating it as still-running.
	fastDone := make(chan struct{})
	fastDef := &testToolDef{
		name: "fast",
		handler: func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
			defer close(fastDone)
			return &tools.ToolResult{Output: "fast-ok"}, nil
		},
	}
	require.NoError(t, toolSrv.Register(context.Background(), fastDef))

	slowTool := &cancelBlockingTool{name: "slow", started: make(chan struct{})}
	require.NoError(t, toolSrv.Register(context.Background(), slowTool))

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"C-PAR", "cancel-par",
		mock.ConversationTurn{
			AssistantContent: "calling tools",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "pc-1", Name: "fast"},
				{ID: "pc-2", Name: "slow"},
			},
		},
	))

	loop := NewLoopAgent(
		WithLLM(model),
		WithTools(toolSrv),
		WithExecutionMode(ExecutionModeParallel),
	)

	ctx, cancel := context.WithCancel(context.Background())
	var events []AgentEvent
	var runErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		events, runErr = loop.Run(ctx, Submission{Content: "run"})
	}()

	// Wait for the slow tool to start, then cancel.
	select {
	case <-slowTool.started:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for slow tool to start")
	}
	// Wait for the fast tool to finish so the parallel snapshot records its
	// real result. A brief sleep lets the goroutine close its done channel
	// in executeToolsParallel before the cancellation snapshot is taken.
	select {
	case <-fastDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for fast tool to complete")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	require.Error(t, runErr)
	assert.True(t, errors.Is(runErr, context.Canceled),
		"expected context.Canceled, got %v", runErr)

	// Collect tool call IDs from the assistant message.
	var toolCallIDs []string
	for _, ev := range events {
		if ev.Kind == "message" && len(ev.ToolCalls) > 0 {
			for _, tc := range ev.ToolCalls {
				toolCallIDs = append(toolCallIDs, tc.ID)
			}
		}
	}
	require.Len(t, toolCallIDs, 2)

	// Every tool call ID must have exactly one tool_result event (no
	// duplicates, no missing).
	for _, id := range toolCallIDs {
		count := 0
		for _, ev := range events {
			if ev.Kind == "tool_result" && ev.ToolCallID == id {
				count++
			}
		}
		assert.Equal(t, 1, count,
			"tool call %s should have exactly one tool_result event, got %d", id, count)
	}

	// The fast tool should have its real result; the slow tool should
	// have a synthetic result containing "canceled".
	for _, ev := range events {
		if ev.Kind == "tool_result" && ev.ToolCallID == "pc-1" {
			assert.Equal(t, "fast-ok", ev.Content,
				"fast tool should have its real result")
			assert.False(t, ev.IsError)
		}
		if ev.Kind == "tool_result" && ev.ToolCallID == "pc-2" {
			assert.Contains(t, ev.Content, "canceled",
				"slow tool should have a synthetic canceled result")
			assert.True(t, ev.IsError)
		}
	}

	// Verify reconstructed messages are complete.
	msgs := eventsToTurnMessages(events)
	for _, id := range toolCallIDs {
		found := false
		for _, m := range msgs {
			if m.Role == "tool" && m.ToolCallID == id {
				found = true
				break
			}
		}
		assert.True(t, found,
			"no tool message for tool call %s in reconstructed messages", id)
	}
}

// ---------------------------------------------------------------------------
// Race detection: concurrent turn submission and Get
// ---------------------------------------------------------------------------

func TestTurnRunner_TurnsMapCleanupRace(t *testing.T) {
	runner := NewEinoTurnRunner(&stubLoop{})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer goroutine: continuously submits turns.
	const writers = 4
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = runner.RunTurn(context.Background(), Submission{Content: "race"})
				}
			}
		}()
	}

	// Reader goroutine: continuously calls Get on various IDs.
	const readers = 4
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// Try to Get a turn that may or may not exist; the
					// call must not race with pruning.
					_, _ = runner.Get(context.Background(), "turn-1")
					// Also read the map size under the lock.
					runner.mu.Lock()
					_ = len(runner.turns)
					runner.mu.Unlock()
				}
			}
		}()
	}

	// Let the goroutines run for a short duration to surface races.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	// After all writers finish, verify the map is bounded.
	runner.mu.Lock()
	mapSize := len(runner.turns)
	runner.mu.Unlock()
	assert.LessOrEqual(t, mapSize, maxTurnsHistory,
		"turns map must stay bounded after concurrent access, got %d", mapSize)
}
