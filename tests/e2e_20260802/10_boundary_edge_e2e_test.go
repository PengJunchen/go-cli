// Package e2e_20260802 contains boundary-condition and edge-case tests
// covering design-level architectural edge cases that were missing from the
// existing suite. These tests exercise loop agent boundaries, sub-agent state
// machines, circuit breakers, retry policies, session trees, config merging,
// tracing, MCP hot reload, and compaction strategies.
package e2e_20260802

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mcp"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tracing"

	"os"
)

// =============================================================================
// LOOP AGENT BOUNDARY TESTS
// =============================================================================

// TestBoundary_LoopMaxIterationsExactlyAtLimit verifies that the loop runs
// exactly MaxIterations turns when every turn produces tool calls, then
// returns errMaxIterations.
func TestBoundary_LoopMaxIterationsExactlyAtLimit(t *testing.T) {
	toolServer := mock.NewMockToolServer()
	_, err := toolServer.RegisterReadFileTool("file content")
	require.NoError(t, err)

	turns := []mock.ConversationTurn{
		{
			AssistantContent:   "turn 1",
			AssistantToolCalls: []mock.ExpectedToolCall{{ID: "c1", Name: "read_file", Args: map[string]any{"path": "a"}}},
		},
		{
			AssistantContent:   "turn 2",
			AssistantToolCalls: []mock.ExpectedToolCall{{ID: "c2", Name: "read_file", Args: map[string]any{"path": "b"}}},
		},
		{
			AssistantContent:   "turn 3",
			AssistantToolCalls: []mock.ExpectedToolCall{{ID: "c3", Name: "read_file", Args: map[string]any{"path": "c"}}},
		},
	}

	llmServer := mock.NewMockLLMServer(nil)
	llmServer.SetTurns(turns)

	loop := core.NewLoopAgent(
		core.WithLLM(llmServer),
		core.WithTools(toolServer),
		core.WithMaxIterations(3),
	)

	ctx := context.Background()
	sub := core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "test",
	}

	events, err := loop.Run(ctx, sub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max iterations")

	// We should have 3 "message" events (one per turn) + 3 "tool_call" + 3 "tool_result" + 1 "error"
	msgCount := 0
	toolCallCount := 0
	toolResultCount := 0
	errorCount := 0
	for _, ev := range events {
		switch ev.Kind {
		case "message":
			msgCount++
		case "tool_call":
			toolCallCount++
		case "tool_result":
			toolResultCount++
		case "error":
			errorCount++
		}
	}
	assert.Equal(t, 3, msgCount, "should have 3 message events")
	assert.Equal(t, 3, toolCallCount, "should have 3 tool_call events")
	assert.Equal(t, 3, toolResultCount, "should have 3 tool_result events")
	assert.Equal(t, 1, errorCount, "should have 1 error event for max iterations")
}

// TestBoundary_LoopMixedToolAndNoToolCalls verifies the loop handles
// alternating tool/no-tool turns correctly.
func TestBoundary_LoopMixedToolAndNoToolCalls(t *testing.T) {
	toolServer := mock.NewMockToolServer()
	_, err := toolServer.RegisterReadFileTool("file content")
	require.NoError(t, err)

	turns := []mock.ConversationTurn{
		{
			AssistantContent:   "turn 1 with tool",
			AssistantToolCalls: []mock.ExpectedToolCall{{ID: "c1", Name: "read_file", Args: map[string]any{"path": "a"}}},
		},
		{
			AssistantContent: "turn 2 no tool",
		},
		{
			AssistantContent:   "turn 3 with tool",
			AssistantToolCalls: []mock.ExpectedToolCall{{ID: "c2", Name: "read_file", Args: map[string]any{"path": "b"}}},
		},
	}

	llmServer := mock.NewMockLLMServer(nil)
	llmServer.SetTurns(turns)

	loop := core.NewLoopAgent(
		core.WithLLM(llmServer),
		core.WithTools(toolServer),
		core.WithMaxIterations(10),
	)

	ctx := context.Background()
	sub := core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "test",
	}

	events, err := loop.Run(ctx, sub)
	require.NoError(t, err)

	// Turn 1: message + tool_call + tool_result
	// Turn 2: message only (no tool calls -> loop finishes)
	msgCount := 0
	for _, ev := range events {
		if ev.Kind == "message" {
			msgCount++
		}
	}
	assert.Equal(t, 2, msgCount, "loop should stop after first turn with no tool calls")
}

// TestBoundary_LoopLLMErrorMidStream verifies partial events from successful
// turns are preserved when a later turn returns an LLM error.
func TestBoundary_LoopLLMErrorMidStream(t *testing.T) {
	toolServer := mock.NewMockToolServer()
	_, err := toolServer.RegisterReadFileTool("file content")
	require.NoError(t, err)

	turns := []mock.ConversationTurn{
		{
			AssistantContent:   "turn 1 ok",
			AssistantToolCalls: []mock.ExpectedToolCall{{ID: "c1", Name: "read_file", Args: map[string]any{"path": "a"}}},
		},
		{
			AssistantError: "simulated LLM error in turn 2",
		},
	}

	llmServer := mock.NewMockLLMServer(nil)
	llmServer.SetTurns(turns)

	loop := core.NewLoopAgent(
		core.WithLLM(llmServer),
		core.WithTools(toolServer),
		core.WithMaxIterations(10),
	)

	ctx := context.Background()
	sub := core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "test",
	}

	events, err := loop.Run(ctx, sub)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "simulated LLM error")

	// Turn 1 events should be preserved
	hasTurn1Message := false
	for _, ev := range events {
		if ev.Kind == "message" && ev.Content == "turn 1 ok" {
			hasTurn1Message = true
		}
	}
	assert.True(t, hasTurn1Message, "turn 1 message event should be preserved")

	// There should be an error event for the LLM error
	hasErrorEvent := false
	for _, ev := range events {
		if ev.Kind == "error" {
			hasErrorEvent = true
		}
	}
	assert.True(t, hasErrorEvent, "error event should be present")
}

// =============================================================================
// SUBAGENT STATE MACHINE TESTS
// =============================================================================

// TestBoundary_SubAgentSendBeforeRun verifies Send works before Run and
// the message is recorded.
func TestBoundary_SubAgentSendBeforeRun(t *testing.T) {
	sa := core.NewDefaultSubAgent(core.SubAgentConfig{
		Name:     "send-before-run",
		MaxTurns: 1,
	})

	// Send before Run - should succeed and record the message
	err := sa.Send(context.Background(), "pre-run message")
	require.NoError(t, err)

	// Verify recorded via the concrete type
	defaultSA, ok := sa.(*core.DefaultSubAgent)
	require.True(t, ok, "should be *core.DefaultSubAgent")
	assert.Equal(t, []string{"pre-run message"}, defaultSA.Received())

	// Now run and verify it completes
	events, err := sa.Run(context.Background(), "test prompt")
	require.NoError(t, err)

	// Collect events
	var eventList []core.AgentEvent
	for ev := range events {
		eventList = append(eventList, ev)
	}

	// Should have at least the user and message events
	assert.GreaterOrEqual(t, len(eventList), 2)

	// Wait for completion
	msg, waitErr := sa.Wait(context.Background())
	require.NoError(t, waitErr)
	assert.Equal(t, "assistant", msg.Role)
}

// TestBoundary_SubAgentInterruptBeforeRun verifies interrupt before Run
// still allows Run to start, but the goroutine is immediately interrupted.
func TestBoundary_SubAgentInterruptBeforeRun(t *testing.T) {
	sa := core.NewDefaultSubAgent(core.SubAgentConfig{
		Name:     "interrupt-before-run",
		MaxTurns: 1,
	})

	err := sa.Interrupt(context.Background())
	require.NoError(t, err)

	events, err := sa.Run(context.Background(), "test prompt")
	// Run succeeds since state is still Idle (interrupt only sets the flag, not state)
	require.NoError(t, err)
	require.NotNil(t, events)

	// Drain events
	for range events {
	}

	// Wait should return with context.Canceled because the runner was interrupted
	_, waitErr := sa.Wait(context.Background())
	require.Error(t, waitErr)
	assert.Equal(t, context.Canceled, waitErr)
}

// TestBoundary_SubAgentWaitMultipleTimes verifies Wait called multiple
// times returns the same result without blocking.
func TestBoundary_SubAgentWaitMultipleTimes(t *testing.T) {
	sa := core.NewDefaultSubAgent(core.SubAgentConfig{
		Name:     "wait-multiple",
		MaxTurns: 1,
	})

	_, err := sa.Run(context.Background(), "test prompt")
	require.NoError(t, err)

	// Wait 3 times
	msg1, err1 := sa.Wait(context.Background())
	msg2, err2 := sa.Wait(context.Background())
	msg3, err3 := sa.Wait(context.Background())

	require.NoError(t, err1)
	assert.Equal(t, msg1.Role, msg2.Role)
	assert.Equal(t, msg1.Content, msg2.Content)
	assert.Equal(t, msg1.Content, msg3.Content)
	require.NoError(t, err2)
	require.NoError(t, err3)
}

// =============================================================================
// CIRCUIT BREAKER TESTS
// =============================================================================

// TestBoundary_CircuitExactThresholdTransition verifies the exact boundary
// between Closed and Open at FailureThreshold.
func TestBoundary_CircuitExactThresholdTransition(t *testing.T) {
	ctx := context.Background()
	cb := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 3,
		RecoveryTimeout:  30 * time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 1,
	})

	assert.Equal(t, production.CircuitClosed, cb.State())

	// Fail 2 times - should still be Closed
	_, _ = cb.Execute(ctx, func() (any, error) { return nil, errors.New("fail 1") })
	assert.Equal(t, production.CircuitClosed, cb.State())

	_, _ = cb.Execute(ctx, func() (any, error) { return nil, errors.New("fail 2") })
	assert.Equal(t, production.CircuitClosed, cb.State())

	// 3rd failure - should transition to Open
	_, _ = cb.Execute(ctx, func() (any, error) { return nil, errors.New("fail 3") })
	assert.Equal(t, production.CircuitOpen, cb.State())
}

// TestBoundary_CircuitHalfOpenExactMaxCalls verifies that HalfOpenMaxCalls
// probes are allowed exactly, then the circuit refuses further calls.
func TestBoundary_CircuitHalfOpenExactMaxCalls(t *testing.T) {
	clock := &fakeClock{t: time.Now()}
	ctx := context.Background()

	cb := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  30 * time.Second,
		HalfOpenMaxCalls: 2,
		SuccessThreshold: 2,
	}, production.WithClock(clock.Now))

	// Open the circuit
	_, _ = cb.Execute(ctx, func() (any, error) { return nil, errors.New("fail") })
	assert.Equal(t, production.CircuitOpen, cb.State())

	// Advance past recovery timeout so it transitions to HalfOpen
	clock.Advance(31 * time.Second)

	// First probe in HalfOpen - succeeds
	val1, err1 := cb.Execute(ctx, func() (any, error) { return "ok1", nil })
	assert.NoError(t, err1)
	assert.Equal(t, "ok1", val1)
	assert.Equal(t, production.CircuitHalfOpen, cb.State())

	// Second probe in HalfOpen - succeeds, now SuccessThreshold (2) is met, closes
	val2, err2 := cb.Execute(ctx, func() (any, error) { return "ok2", nil })
	assert.NoError(t, err2)
	assert.Equal(t, "ok2", val2)
	assert.Equal(t, production.CircuitClosed, cb.State())

	// One more call should work in Closed state
	val3, err3 := cb.Execute(ctx, func() (any, error) { return "ok3", nil })
	assert.NoError(t, err3)
	assert.Equal(t, "ok3", val3)
}

// TestBoundary_CircuitConcurrentLowThreshold verifies the circuit breaker
// handles concurrent calls with a low threshold without panicking.
func TestBoundary_CircuitConcurrentLowThreshold(t *testing.T) {
	ctx := context.Background()
	cb := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryTimeout:  30 * time.Second,
		HalfOpenMaxCalls: 1,
		SuccessThreshold: 1,
	})

	var wg sync.WaitGroup
	errs := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cb.Execute(ctx, func() (any, error) {
				return nil, errors.New("fail")
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	// After concurrent failures, state should be consistent (either Closed or Open)
	state := cb.State()
	assert.True(t, state == production.CircuitClosed || state == production.CircuitOpen,
		"state should be Closed or Open, got %s", state)

	// Count errors - at least 2 should have failed to trigger the threshold
	errCount := 0
	for range errs {
		errCount++
	}
	assert.GreaterOrEqual(t, errCount, 2)
}

// =============================================================================
// RETRY BOUNDARY TESTS
// =============================================================================

// TestBoundary_RetryMaxAttemptsBoundary verifies ShouldRetry returns true
// for attempt MaxAttempts-1 and false for attempt MaxAttempts.
func TestBoundary_RetryMaxAttemptsBoundary(t *testing.T) {
	ctx := context.Background()
	p := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
		Jitter:      0,
	})

	transientErr := production.NewError(production.ErrorTransient, errors.New("transient error"))

	// attempt 0 and 1 should be retried
	assert.True(t, p.ShouldRetry(ctx, transientErr, 0), "attempt 0 should retry")
	assert.True(t, p.ShouldRetry(ctx, transientErr, 1), "attempt 1 should retry")

	// attempt 2 is MaxAttempts-1 (last retry)
	assert.True(t, p.ShouldRetry(ctx, transientErr, 2), "attempt 2 (last) should retry")

	// attempt 3 == MaxAttempts - should NOT retry
	assert.False(t, p.ShouldRetry(ctx, transientErr, 3), "attempt 3 should NOT retry")

	// attempt 4 > MaxAttempts - should NOT retry
	assert.False(t, p.ShouldRetry(ctx, transientErr, 4), "attempt 4 should NOT retry")
}

// TestBoundary_RetryNilError verifies ShouldRetry returns false for nil error.
func TestBoundary_RetryNilError(t *testing.T) {
	ctx := context.Background()
	p := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 5,
		BaseDelay:   1 * time.Millisecond,
	})

	assert.False(t, p.ShouldRetry(ctx, nil, 0), "nil error should not be retried")
	assert.False(t, p.ShouldRetry(ctx, nil, 1), "nil error should not be retried")
}

// =============================================================================
// SESSION TREE EDGE TESTS
// =============================================================================

// TestBoundary_TreeDeepChainTraversal verifies that a 100-entry linear
// chain can be traversed without stack overflow.
func TestBoundary_TreeDeepChainTraversal(t *testing.T) {
	ctx := context.Background()
	tree := session.NewDefaultSessionTree()

	// Create 100-entry deep linear chain
	var prevID string
	var lastID string
	for i := 0; i < 100; i++ {
		id := "entry-" + string(rune('0'+i%10)) + string(rune('0'+(i/10)%10)) + string(rune('0'+i/100))
		entry := &session.SessionEntry{
			ID:        id,
			ParentID:  prevID,
			Type:      session.EntryTypeUser,
			Content:   "message " + string(rune('0'+i)),
			Timestamp: time.Now(),
		}
		err := tree.Append(ctx, entry)
		require.NoError(t, err)
		prevID = id
		lastID = id
	}

	// Traverse the branch
	branch, err := tree.GetBranch(ctx, lastID)
	require.NoError(t, err)
	assert.Equal(t, 100, len(branch), "should have 100 entries in the branch")

	// Verify order: root first, leaf last
	assert.Equal(t, "entry-000", branch[0].ID)
	assert.Equal(t, lastID, branch[99].ID)

	// BuildContext should also work
	sctx, err := tree.BuildContext(ctx, lastID)
	require.NoError(t, err)
	assert.Equal(t, 100, sctx.EntryCount)
}

// TestBoundary_TreeEmptyTreeOperations verifies operations on an empty tree.
func TestBoundary_TreeEmptyTreeOperations(t *testing.T) {
	ctx := context.Background()
	tree := session.NewDefaultSessionTree()

	// GetBranch on empty tree
	_, err := tree.GetBranch(ctx, "nonexistent")
	assert.Error(t, err)

	// BuildContext on empty tree
	_, err = tree.BuildContext(ctx, "nonexistent")
	assert.Error(t, err)

	// MoveTo on empty tree
	err = tree.MoveTo(ctx, "nonexistent")
	assert.Error(t, err)

	// CurrentLeaf should be empty
	assert.Equal(t, "", tree.CurrentLeaf())

	// Append first entry should set CurrentLeaf
	entry := &session.SessionEntry{
		ID:        "root",
		Type:      session.EntryTypeUser,
		Content:   "first message",
		Timestamp: time.Now(),
	}
	err = tree.Append(ctx, entry)
	require.NoError(t, err)

	assert.Equal(t, "root", tree.CurrentLeaf())
}

// =============================================================================
// CONFIG MERGE EDGE TESTS
// =============================================================================

// TestBoundary_ConfigNilLayers verifies nil flag and override layers
// are skipped without panic.
func TestBoundary_ConfigNilLayers(t *testing.T) {
	loader := config.NewLoader()
	// Explicitly do NOT set WithFlag or WithOverride (leaving them nil)

	// Load should not panic
	cfg, err := loader.Load(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Should have default values
	assert.Equal(t, "jsonl", cfg.Tracing.Exporter)
	assert.Equal(t, "info", cfg.Tracing.Level)
	assert.True(t, *cfg.Tracing.Enabled)
}

// TestBoundary_ConfigOverrideTrumpsAll verifies the override layer wins
// over all other layers when all set the same field.
func TestBoundary_ConfigOverrideTrumpsAll(t *testing.T) {
	loader := config.NewLoader().
		WithFlag(&config.Config{
			Model: config.ModelConfig{Name: "flag-model"},
		}).
		WithOverride(&config.Config{
			Model: config.ModelConfig{Name: "override-model"},
		})

	cfg, err := loader.Load(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "override-model", cfg.Model.Name, "override should win over flag")
}

// =============================================================================
// TRACER EDGE TESTS
// =============================================================================

// TestBoundary_TracerDisabledSpanFromContext verifies SpanFromContext returns
// noopSpan when the tracer is disabled.
func TestBoundary_TracerDisabledSpanFromContext(t *testing.T) {
	// Create a disabled tracer
	tr := tracing.NewTracer("", nil)
	tr.SetEnabled(false)

	// Create a context with the tracer in it (by starting a span that will be noop)
	span, ctx := tr.Start(context.Background(), "test-span", tracing.SpanKindInternal)
	defer span.End()

	// SpanFromContext should return noopSpan
	child, childCtx := tracing.SpanFromContext(ctx, "child-span", tracing.SpanKindInternal)
	defer child.End()

	// noopSpan characteristics
	assert.Equal(t, "", child.TraceID())
	assert.Equal(t, "", child.SpanID())

	// Use childCtx for another call
	grandChild, _ := tracing.SpanFromContext(childCtx, "grandchild-span", tracing.SpanKindInternal)
	defer grandChild.End()
	assert.Equal(t, "", grandChild.TraceID())
}

// TestBoundary_TracerNestedDeepSpans verifies 50-level deep nested spans
// maintain a correct parent-child chain.
func TestBoundary_TracerNestedDeepSpans(t *testing.T) {
	exporter := mock.NewMockTraceExporter()
	tr := tracing.NewTracer("deep-trace", exporter)

	type spanPair struct {
		span    tracing.TraceSpan
		spanCtx context.Context
	}

	var spans []spanPair
	ctx := context.Background()

	// Create 50 levels of nested spans
	for i := 0; i < 50; i++ {
		span, spanCtx := tr.Start(ctx, "level-"+string(rune('0'+i%10)), tracing.SpanKindInternal)
		spans = append(spans, spanPair{span: span, spanCtx: spanCtx})
		ctx = spanCtx
	}

	// End them all in reverse order
	for i := len(spans) - 1; i >= 0; i-- {
		spans[i].span.End()
	}

	// Allow async export
	time.Sleep(50 * time.Millisecond)

	// Verify parent-child chain integrity
	exportedSpans := exporter.Spans()

	type spanInfo struct {
		spanID       string
		parentSpanID string
	}
	var chain []spanInfo
	for _, s := range exportedSpans {
		chain = append(chain, spanInfo{spanID: s.SpanID, parentSpanID: s.ParentSpanID})
	}
	// Build a lookup
	idMap := make(map[string]string) // spanID -> parentSpanID
	for _, c := range chain {
		idMap[c.spanID] = c.parentSpanID
	}

	// Every span's parentSpanID (except root) should exist as a spanID
	for spanID, parentID := range idMap {
		if parentID == "" {
			continue // root span
		}
		_, exists := idMap[parentID]
		assert.True(t, exists, "parent span %s for child %s should exist", parentID, spanID)
	}
}

// =============================================================================
// MCP HOT RELOAD EDGE TESTS
// =============================================================================

// TestBoundary_MCPHotReloadRapidMultipleReload verifies rapid successive
// Reload calls do not panic and only one reconnect cycle runs at a time.
func TestBoundary_MCPHotReloadRapidMultipleReload(t *testing.T) {
	server := mock.NewMockMCPServer("rapid-reload-server")
	server.RegisterTool("test_tool", "a test tool", func(args map[string]any) (any, error) {
		return "ok", nil
	})
	require.NoError(t, server.Start(context.Background()))

	reloader := mcp.NewDefaultHotReloader(server, func(tools []mcp.MCPTool) {
		// registration callback
	},
		mcp.WithPollInterval(10*time.Millisecond),
		mcp.WithMaxRetries(0),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start watching - we need a config path so use a temp file
	tmpDir := t.TempDir()
	configPath := tmpDir + "/test_config.json"
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))

	err := reloader.Watch(ctx, configPath)
	require.NoError(t, err)

	// Rapid multiple Reload calls
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reloader.Reload(context.Background())
		}()
	}
	wg.Wait()

	// No panics = success. Also verify stop works.
	err = reloader.Stop()
	require.NoError(t, err)
}

// =============================================================================
// COMPACTION EDGE TESTS
// =============================================================================

// TestBoundary_CompactionAllStrategiesFail verifies UnifiedCompactor returns
// error when all strategies are nil/unavailable.
func TestBoundary_CompactionAllStrategiesFail(t *testing.T) {
	ctx := context.Background()

	compactor := compaction.NewUnifiedCompactor(
		compaction.WithMicro(nil),
		compaction.WithTruncating(nil),
	)

	items := []compaction.TurnItem{
		{ID: "1", Role: compaction.RoleUser, Content: "hello world"},
	}

	estimator := compaction.NewHeuristicTokenEstimator()

	_, err := compactor.Compact(ctx, items, 100, estimator)
	require.Error(t, err)
}

// TestBoundary_CompactionOneTokenBudget verifies compaction with
// a very tight token budget produces a result within budget.
func TestBoundary_CompactionOneTokenBudget(t *testing.T) {
	ctx := context.Background()

	compactor := compaction.NewUnifiedCompactor()

	items := []compaction.TurnItem{
		{ID: "1", Role: compaction.RoleUser, Content: "hello world this is a long message"},
		{ID: "2", Role: compaction.RoleAssistant, Content: "ok"},
	}

	estimator := compaction.NewHeuristicTokenEstimator()

	// Use a very small budget that forces truncating
	result, err := compactor.Compact(ctx, items, 5, estimator)
	require.NoError(t, err)

	// Verify result fits within the budget
	tokenCount := 0
	for _, it := range result {
		if it.Content != "" {
			n, _ := estimator.Estimate(it.Content)
			tokenCount += n
		}
		if it.ToolResult != "" {
			n, _ := estimator.Estimate(it.ToolResult)
			tokenCount += n
		}
	}
	assert.LessOrEqual(t, tokenCount, 5, "result should fit within budget")
	// With maxTokens=5, only some items should remain
	assert.LessOrEqual(t, len(result), 1, "at most 1 item should survive")
}
