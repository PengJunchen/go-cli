// Package e2e_20260802 contains end-to-end integration tests verifying
// cross-module integration of components built in Phases 11-18.
//
// These tests exercise the wiring between ModelMiddleware chains, PlanMode
// enforcement, SubmissionQueue ordering, CostTracking/Stats accumulation,
// ResultMasker sanitisation, parallel tool execution, FailureTurn synthesis,
// TodoWriteTool CRUD, session migration, model alias resolution, feature flag
// runtime toggling, and AgentHarnessError normalisation.
package e2e_20260802

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Helper types local to this file
// =============================================================================

// fastRetryPolicy is a test RetryPolicy with minimal backoff for fast test
// execution. It retries up to 3 times on any non-nil error.
type fastRetryPolicy struct{}

var _ llm.RetryPolicy = (*fastRetryPolicy)(nil)

func (p *fastRetryPolicy) ShouldRetry(_ context.Context, err error, attempt int) bool {
	return err != nil && attempt < 3
}

func (p *fastRetryPolicy) NextBackoff(_ context.Context, _ int) time.Duration {
	return 1 * time.Millisecond
}

func (p *fastRetryPolicy) Name() string { return "fast-retry" }

// e2eSleepTool is a ToolDefinition that sleeps for a configured duration before
// returning its output. Used to verify parallel execution timing.
type e2eSleepTool struct {
	name   string
	delay  time.Duration
	output string
}

var _ tools.ToolDefinition = (*e2eSleepTool)(nil)

func (t *e2eSleepTool) Name() string        { return t.name }
func (t *e2eSleepTool) Description() string { return "e2e sleep tool: " + t.name }
func (t *e2eSleepTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	time.Sleep(t.delay)
	return &tools.ToolResult{Output: t.output}, nil
}

// =============================================================================
// Test 1: ModelMiddlewareChain – Retry + Validate Integration
// =============================================================================

// TestE2E_ModelMiddlewareChain_RetryAndValidate verifies that a
// ModelMiddlewareChain composed of RetryModelMiddleware (outer) and
// ValidateModelMiddleware (inner) retries a failing first call and ultimately
// returns the validated success response from the second attempt.
func TestE2E_ModelMiddlewareChain_RetryAndValidate(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx := t.Context()

	// Template: turn 0 returns an error, turn 1 returns valid content.
	template := mock.NewConversationTemplate("S-E2E-01", "retry-validate",
		mock.ConversationTurn{AssistantError: "transient failure"},
		mock.ConversationTurn{AssistantContent: "success after retry"},
	)
	mockLLM := mock.NewMockLLMServer(template)

	// Build the middleware chain: retry (outer) -> validate (inner).
	chain := llm.NewModelMiddlewareChain()
	require.NoError(t, chain.Register(llm.NewRetryModelMiddleware(
		llm.WithRetryPolicy(&fastRetryPolicy{}),
	)))
	require.NoError(t, chain.Register(llm.NewValidateModelMiddleware()))

	wrapped := chain.Wrap(mockLLM)

	// Call Generate through the wrapped chain.
	resp, err := wrapped.Generate(ctx, []llm.Message{
		{Role: llm.RoleUser, Content: "hello"},
	})
	require.NoError(t, err, "retry should recover from the first failure")
	require.NotNil(t, resp)
	assert.Equal(t, "success after retry", resp.Content)

	// The mock server should have been called twice: first (error), then retry (success).
	assert.Equal(t, 2, mockLLM.CallCount(),
		"mock LLM should have been called twice: initial failure + retry success")
}

// =============================================================================
// Test 2: PlanMode – Blocks Write Tools
// =============================================================================

// TestE2E_PlanMode_BlocksWriteTools verifies that PlanModeController blocks
// write/edit/bash tools while active, allows read-only tools, and releases all
// blocks once deactivated.
func TestE2E_PlanMode_BlocksWriteTools(t *testing.T) {
	ctx := t.Context()
	plan := core.NewDefaultPlanModeController()

	// Initially plan mode is inactive — writes should be allowed.
	assert.False(t, plan.IsActive())
	assert.False(t, plan.ShouldBlockWrite("write"))

	// Activate plan mode.
	require.NoError(t, plan.Enter(ctx, "user requested plan mode"))
	assert.True(t, plan.IsActive())

	// Write tools should be blocked.
	for _, name := range []string{"write", "edit", "bash"} {
		assert.True(t, plan.ShouldBlockWrite(name),
			"plan mode should block %q", name)
	}

	// Read-only tools should be allowed.
	for _, name := range []string{"read", "grep", "ls"} {
		assert.False(t, plan.ShouldBlockWrite(name),
			"plan mode should NOT block %q", name)
	}

	// Deactivate plan mode.
	require.NoError(t, plan.Exit(ctx, "plan complete"))
	assert.False(t, plan.IsActive())

	// After deactivation, even write tools should be allowed.
	for _, name := range []string{"write", "edit", "bash"} {
		assert.False(t, plan.ShouldBlockWrite(name),
			"after exit, %q should not be blocked", name)
	}
}

// =============================================================================
// Test 3: SubmissionQueue – Steering and FollowUp FIFO
// =============================================================================

// TestE2E_SubmissionQueue_SteeringAndFollowUp verifies FIFO ordering within
// the Steering and FollowUp queues, plus Drain and Abort semantics.
func TestE2E_SubmissionQueue_SteeringAndFollowUp(t *testing.T) {
	q := session.NewDefaultSubmissionQueue()

	// Enqueue 3 steering items.
	for i := 0; i < 3; i++ {
		require.NoError(t, q.Enqueue(session.QueueSteering, session.QueuedSubmission{
			Content: fmt.Sprintf("steer-%d", i),
		}))
	}

	// Enqueue 2 follow-up items.
	for i := 0; i < 2; i++ {
		require.NoError(t, q.Enqueue(session.QueueFollowUp, session.QueuedSubmission{
			Content: fmt.Sprintf("follow-%d", i),
		}))
	}

	// Verify queue lengths.
	assert.Equal(t, 3, q.Len(session.QueueSteering))
	assert.Equal(t, 2, q.Len(session.QueueFollowUp))

	// Dequeue steering items — should be FIFO.
	for i := 0; i < 3; i++ {
		item, ok := q.Dequeue(session.QueueSteering)
		require.True(t, ok)
		assert.Equal(t, fmt.Sprintf("steer-%d", i), item.Content)
	}
	// Steering queue should now be empty.
	_, ok := q.Dequeue(session.QueueSteering)
	assert.False(t, ok)

	// Dequeue follow-up items — should be FIFO.
	for i := 0; i < 2; i++ {
		item, ok := q.Dequeue(session.QueueFollowUp)
		require.True(t, ok)
		assert.Equal(t, fmt.Sprintf("follow-%d", i), item.Content)
	}

	// --- Test Drain ---
	require.NoError(t, q.Enqueue(session.QueueSteering, session.QueuedSubmission{Content: "d-1"}))
	require.NoError(t, q.Enqueue(session.QueueSteering, session.QueuedSubmission{Content: "d-2"}))
	require.NoError(t, q.Enqueue(session.QueueSteering, session.QueuedSubmission{Content: "d-3"}))
	drained := q.Drain(session.QueueSteering)
	require.Len(t, drained, 3)
	assert.Equal(t, "d-1", drained[0].Content)
	assert.Equal(t, "d-3", drained[2].Content)
	assert.Equal(t, 0, q.Len(session.QueueSteering), "queue should be empty after drain")

	// --- Test Abort ---
	require.NoError(t, q.Enqueue(session.QueueFollowUp, session.QueuedSubmission{Content: "a-1"}))
	require.NoError(t, q.Enqueue(session.QueueFollowUp, session.QueuedSubmission{Content: "a-2"}))
	aborted := q.Abort(session.QueueFollowUp)
	assert.Equal(t, 2, aborted)
	assert.Equal(t, 0, q.Len(session.QueueFollowUp), "queue should be empty after abort")
}

// =============================================================================
// Test 4: CostTracking – With Model Call
// =============================================================================

// TestE2E_CostTracking_WithModelCall verifies that CostTracker computes the
// correct cost for a known model call and that StatsRegistry accumulates
// session statistics across multiple records.
func TestE2E_CostTracking_WithModelCall(t *testing.T) {
	tracker := production.NewCostTracker(nil) // use DefaultCostTiers

	// Record a gpt-4o call: 1000 input, 500 output.
	// tier: InputPer1K=0.0025, OutputPer1K=0.01
	// cost = 1000*0.0025/1000 + 500*0.01/1000 = 0.0025 + 0.005 = 0.0075
	cost, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)
	assert.InDelta(t, 0.0075, cost, 1e-9)

	// Total and call count.
	assert.InDelta(t, 0.0075, tracker.Total(), 1e-9)
	assert.Equal(t, 1, tracker.Calls())

	// Record a second call to verify accumulation.
	cost2, err := tracker.Record("gpt-4o", 2000, 1000)
	require.NoError(t, err)
	// cost2 = 2000*0.0025/1000 + 1000*0.01/1000 = 0.005 + 0.01 = 0.015
	assert.InDelta(t, 0.015, cost2, 1e-9)
	assert.InDelta(t, 0.0225, tracker.Total(), 1e-9)
	assert.Equal(t, 2, tracker.Calls())

	// --- StatsRegistry ---
	reg := production.NewStatsRegistry()
	reg.RecordTurn("session-A")
	reg.RecordTurn("session-A")
	reg.RecordToolCall("session-A")
	reg.RecordTokens("session-A", 3000, 1500)

	stats, ok := reg.GetSessionStats("session-A")
	require.True(t, ok)
	assert.Equal(t, 2, stats.Turns)
	assert.Equal(t, 1, stats.ToolCalls)
	assert.Equal(t, 3000, stats.TokensIn)
	assert.Equal(t, 1500, stats.TokensOut)

	// A second session should not interfere.
	reg.RecordTurn("session-B")
	all := reg.GetAll()
	assert.Contains(t, all, "session-A")
	assert.Contains(t, all, "session-B")
	assert.Equal(t, 1, all["session-B"].Turns)
}

// =============================================================================
// Test 5: ResultMasker – Sensitive Data
// =============================================================================

// TestE2E_ResultMasker_SensitiveData verifies that the default ResultMasker
// redacts API keys and GitHub tokens while leaving non-sensitive content
// intact.
func TestE2E_ResultMasker_SensitiveData(t *testing.T) {
	masker := tools.NewResultMasker(nil) // use DefaultMaskPatterns

	apiKey := "sk-abcdefghijklmnopqrstuvwxyz1234567890"
	githubToken := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

	t.Run("API key is redacted", func(t *testing.T) {
		input := fmt.Sprintf("The key is %s and it should be hidden", apiKey)
		masked := masker.Mask(input)
		assert.NotContains(t, masked, apiKey, "API key must not appear in masked output")
		assert.Contains(t, masked, "[REDACTED]")
	})

	t.Run("GitHub token is redacted", func(t *testing.T) {
		input := fmt.Sprintf("Token: %s", githubToken)
		masked := masker.Mask(input)
		assert.NotContains(t, masked, githubToken, "GitHub token must not appear in masked output")
		assert.Contains(t, masked, "[REDACTED]")
	})

	t.Run("non-sensitive content is unchanged", func(t *testing.T) {
		input := "This is a normal file with no secrets. Path: /usr/local/bin/go"
		masked := masker.Mask(input)
		assert.Equal(t, input, masked, "non-sensitive content should be unchanged")
	})

	t.Run("multiple secrets in one string", func(t *testing.T) {
		input := fmt.Sprintf("keys: %s and %s", apiKey, githubToken)
		masked := masker.Mask(input)
		assert.NotContains(t, masked, apiKey)
		assert.NotContains(t, masked, githubToken)
		assert.Equal(t, 2, strings.Count(masked, "[REDACTED]"))
	})
}

// =============================================================================
// Test 6: Parallel Tool Execution
// =============================================================================

// TestE2E_ParallelToolExecution verifies that the LoopAgent in
// ExecutionModeParallel runs multiple tool calls concurrently (verified by
// timing) and returns results in input order. A tracing span wraps the
// execution to verify trace integration.
func TestE2E_ParallelToolExecution(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Build a registry with 3 tools that each sleep 50ms.
	registry := tools.NewDefaultToolRegistry()
	toolNames := []string{"par_a", "par_b", "par_c"}
	for _, name := range toolNames {
		name := name
		require.NoError(t, registry.Register(t.Context(), &e2eSleepTool{
			name:   name,
			delay:  50 * time.Millisecond,
			output: name + "_result",
		}))
	}

	// Mock LLM: turn 0 issues 3 parallel tool calls; turn 1 returns "done".
	mockLLM := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"S-E2E-06", "parallel-tools",
		mock.ConversationTurn{
			AssistantContent: "running tools in parallel",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "c1", Name: "par_a"},
				{ID: "c2", Name: "par_b"},
				{ID: "c3", Name: "par_c"},
			},
		},
		mock.ConversationTurn{AssistantContent: "done"},
	))

	// Tracer to verify span integration.
	exp := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("e2e-parallel", exp)
	rootSpan, traceCtx := tracer.Start(t.Context(), "e2e.parallel.root", tracing.SpanKindInternal)

	loop := core.NewLoopAgent(
		core.WithLLM(mockLLM),
		core.WithTools(registry),
		core.WithMaxIterations(10),
		core.WithExecutionMode(core.ExecutionModeParallel),
	)

	start := time.Now()
	events, err := loop.Run(traceCtx, core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "run parallel tools",
	})
	elapsed := time.Since(start)
	rootSpan.End()

	require.NoError(t, err)
	require.NotEmpty(t, events)

	// If sequential, total would be >= 150ms. Parallel should be ~50ms.
	assert.Less(t, elapsed, 120*time.Millisecond,
		"parallel execution of 3x50ms tools should complete in under 120ms; got %v", elapsed)

	// Collect tool_call and tool_result events.
	var toolCalls, toolResults []string
	for _, ev := range events {
		switch ev.Kind {
		case "tool_call":
			toolCalls = append(toolCalls, ev.Content)
		case "tool_result":
			toolResults = append(toolResults, ev.Content)
		}
	}

	// Verify all 3 tools were called.
	assert.Len(t, toolCalls, 3)
	assert.Contains(t, toolCalls, "par_a")
	assert.Contains(t, toolCalls, "par_b")
	assert.Contains(t, toolCalls, "par_c")

	// Verify all 3 results are returned.
	assert.Len(t, toolResults, 3)
	assert.Contains(t, toolResults, "par_a_result")
	assert.Contains(t, toolResults, "par_b_result")
	assert.Contains(t, toolResults, "par_c_result")

	// Verify tracing spans were exported.
	time.Sleep(150 * time.Millisecond)
	spans := exp.Spans()
	require.NotEmpty(t, spans, "tracing should have exported spans")
}

// =============================================================================
// Test 7: FailureTurnSynthesis
// =============================================================================

// TestE2E_FailureTurnSynthesis verifies that DefaultFailureTurnSynthesizer
// classifies context.DeadlineExceeded as recoverable, produces a system message
// with error context, and rejects non-recoverable errors (IsRecoverable returns
// false).
func TestE2E_FailureTurnSynthesis(t *testing.T) {
	ctx := t.Context()
	synth := core.NewDefaultFailureTurnSynthesizer()

	t.Run("context.DeadlineExceeded is recoverable", func(t *testing.T) {
		assert.True(t, synth.IsRecoverable(context.DeadlineExceeded))
	})

	t.Run("synthesize creates system message for recoverable error", func(t *testing.T) {
		msg, err := synth.Synthesize(ctx, context.DeadlineExceeded)
		require.NoError(t, err)
		assert.Equal(t, "system", msg.Role)
		assert.Contains(t, msg.Content, "recoverable")
		assert.Contains(t, msg.Content, context.DeadlineExceeded.Error())
		assert.Equal(t, context.DeadlineExceeded.Error(), msg.OriginalError)
	})

	t.Run("non-recoverable error is rejected by IsRecoverable", func(t *testing.T) {
		// A generic "invalid argument" error is not recoverable.
		err := errors.New("invalid argument: malformed request")
		assert.False(t, synth.IsRecoverable(err))
	})

	t.Run("synthesize for non-recoverable error still produces message", func(t *testing.T) {
		err := errors.New("fatal: permission denied")
		msg, sErr := synth.Synthesize(ctx, err)
		require.NoError(t, sErr)
		assert.Equal(t, "system", msg.Role)
		assert.Contains(t, msg.Content, "may not be automatically recoverable")
		assert.Contains(t, msg.Content, "permission denied")
	})

	t.Run("nil error is rejected", func(t *testing.T) {
		_, err := synth.Synthesize(ctx, nil)
		require.Error(t, err)
	})
}

// =============================================================================
// Test 8: TodoWriteTool – End-to-End
// =============================================================================

// TestE2E_TodoWriteTool_EndToEnd exercises the full CRUD lifecycle of
// TodoWriteTool: add, list, update, and remove.
func TestE2E_TodoWriteTool_EndToEnd(t *testing.T) {
	ctx := t.Context()
	store := tools.NewTodoStore()
	tool := tools.NewTodoWriteTool(store)

	// --- Add ---
	addResult, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "call-add",
		Name: "todo_write",
		Args: map[string]any{
			"action":   "add",
			"content":  "Write E2E tests",
			"priority": "high",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, addResult.Output, "added todo")
	todoID, _ := addResult.Metadata["id"].(string)
	assert.NotEmpty(t, todoID, "add should return a todo ID")

	// --- List ---
	listResult, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "call-list",
		Name: "todo_write",
		Args: map[string]any{"action": "list"},
	})
	require.NoError(t, err)
	assert.Contains(t, listResult.Output, "Write E2E tests")
	assert.Contains(t, listResult.Output, todoID)

	// --- Update ---
	updateResult, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "call-update",
		Name: "todo_write",
		Args: map[string]any{
			"action": "update",
			"id":     todoID,
			"status": "in_progress",
		},
	})
	require.NoError(t, err)
	assert.Contains(t, updateResult.Output, "updated todo")
	assert.Contains(t, updateResult.Output, "in_progress")

	// Verify the update took effect via List.
	listResult2, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "call-list2",
		Name: "todo_write",
		Args: map[string]any{"action": "list"},
	})
	require.NoError(t, err)
	assert.Contains(t, listResult2.Output, "in_progress")

	// --- Remove ---
	removeResult, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "call-remove",
		Name: "todo_write",
		Args: map[string]any{
			"action": "remove",
			"id":     todoID,
		},
	})
	require.NoError(t, err)
	assert.Contains(t, removeResult.Output, "removed todo")

	// Verify the item is gone via List.
	listResult3, err := tool.Execute(ctx, tools.ToolCall{
		ID:   "call-list3",
		Name: "todo_write",
		Args: map[string]any{"action": "list"},
	})
	require.NoError(t, err)
	assert.NotContains(t, listResult3.Output, "Write E2E tests")
}

// =============================================================================
// Test 9: SessionMigration – Chain
// =============================================================================

// TestE2E_SessionMigration_Chain verifies that the MigrationChain with default
// migrations adds "metadata" and "trace_id" fields when migrating from v1 to
// v3 (CurrentVersion).
func TestE2E_SessionMigration_Chain(t *testing.T) {
	chain := session.NewMigrationChain()

	// v1 data without metadata or trace_id.
	v1Data := map[string]any{
		"id":      "session-001",
		"entries": []any{"entry-1", "entry-2"},
	}

	// Verify preconditions.
	_, hasMetadata := v1Data["metadata"]
	assert.False(t, hasMetadata, "v1 data should not have metadata")
	_, hasTraceID := v1Data["trace_id"]
	assert.False(t, hasTraceID, "v1 data should not have trace_id")

	// Migrate to current version (v3).
	migrated, err := chain.Migrate(v1Data, session.SessionV1)
	require.NoError(t, err)
	require.NotNil(t, migrated)

	// Verify metadata field was added (v1 -> v2).
	md, ok := migrated["metadata"]
	require.True(t, ok, "metadata field should be present after migration")
	assert.NotNil(t, md)

	// Verify trace_id field was added (v2 -> v3).
	traceID, ok := migrated["trace_id"]
	require.True(t, ok, "trace_id field should be present after migration")
	assert.NotNil(t, traceID)

	// Verify original data is preserved.
	assert.Equal(t, "session-001", migrated["id"])
}

// =============================================================================
// Test 10: ModelAlias – Resolution
// =============================================================================

// TestE2E_ModelAlias_Resolution verifies that ModelAliasResolver resolves known
// short aliases to full model identifiers and passes unknown aliases through
// unchanged.
func TestE2E_ModelAlias_Resolution(t *testing.T) {
	resolver := config.NewModelAliasResolver()

	// Known aliases.
	assert.Equal(t, "claude-sonnet-4-20250514", resolver.Resolve("sonnet"))
	assert.Equal(t, "claude-opus-4-20250514", resolver.Resolve("opus"))
	assert.Equal(t, "claude-haiku-3.5-20250315", resolver.Resolve("haiku"))

	// Unknown alias passes through unchanged.
	assert.Equal(t, "some-custom-model", resolver.Resolve("some-custom-model"))
	assert.Equal(t, "gpt-4o", resolver.Resolve("gpt-4o"), "gpt-4o maps to itself")

	// AddAlias and resolve.
	resolver.AddAlias("my-model", "my-full-model-name/v2")
	assert.Equal(t, "my-full-model-name/v2", resolver.Resolve("my-model"))
}

// =============================================================================
// Test 11: FeatureFlag – Runtime
// =============================================================================

// TestE2E_FeatureFlag_Runtime verifies that FeatureFlagRegistry supports
// registering flags, toggling them at runtime, reflecting changes via
// IsEnabled, and bulk-updating via LoadFromConfig.
func TestE2E_FeatureFlag_Runtime(t *testing.T) {
	reg := config.NewFeatureFlagRegistry()

	// Register flags.
	reg.Register(config.FeatureFlag{Name: "parallel_tools", Enabled: false, Description: "enable parallel tool execution"})
	reg.Register(config.FeatureFlag{Name: "cost_tracking", Enabled: true, Description: "enable cost tracking"})
	reg.Register(config.FeatureFlag{Name: "new_compactor", Enabled: false, Description: "use new compaction strategy"})

	// Verify initial states.
	assert.False(t, reg.IsEnabled("parallel_tools"))
	assert.True(t, reg.IsEnabled("cost_tracking"))
	assert.False(t, reg.IsEnabled("new_compactor"))
	assert.False(t, reg.IsEnabled("unregistered"), "unregistered flag should default to false")

	// Toggle at runtime.
	require.NoError(t, reg.Set("parallel_tools", true))
	assert.True(t, reg.IsEnabled("parallel_tools"))

	require.NoError(t, reg.Set("cost_tracking", false))
	assert.False(t, reg.IsEnabled("cost_tracking"))

	// Setting an unregistered flag returns an error.
	err := reg.Set("nonexistent", true)
	assert.ErrorIs(t, err, config.ErrFlagNotFound)

	// LoadFromConfig bulk update — only registered flags are affected.
	reg.LoadFromConfig(map[string]bool{
		"parallel_tools": false,
		"new_compactor":  true,
		"unknown_flag":   true, // should be ignored
	})
	assert.False(t, reg.IsEnabled("parallel_tools"))
	assert.True(t, reg.IsEnabled("new_compactor"))

	// List returns all registered flags sorted by name.
	flags := reg.List()
	require.Len(t, flags, 3)
	assert.Equal(t, "cost_tracking", flags[0].Name)
	assert.Equal(t, "new_compactor", flags[1].Name)
	assert.Equal(t, "parallel_tools", flags[2].Name)
}

// =============================================================================
// Test 12: AgentHarnessError – Normalization
// =============================================================================

// TestE2E_AgentHarnessError_Normalization verifies that NormalizeError assigns
// the correct error code to various error types and that the resulting
// AgentHarnessError wraps the original cause.
func TestE2E_AgentHarnessError_Normalization(t *testing.T) {
	t.Run("timeout error", func(t *testing.T) {
		orig := context.DeadlineExceeded
		norm := core.NormalizeError(orig)
		require.NotNil(t, norm)
		assert.Equal(t, core.ErrCodeTimeout, norm.Code)
		assert.ErrorIs(t, norm, orig, "normalized error should wrap the original")
		assert.Contains(t, norm.Error(), core.ErrCodeTimeout)
		assert.Contains(t, norm.Error(), "timed out")
	})

	t.Run("hook rejection error", func(t *testing.T) {
		orig := errors.New("hook rejected the run: safety policy violation")
		norm := core.NormalizeError(orig)
		require.NotNil(t, norm)
		assert.Equal(t, core.ErrCodeHookRejected, norm.Code)
		assert.ErrorIs(t, norm, orig)
		assert.Contains(t, norm.Error(), core.ErrCodeHookRejected)
	})

	t.Run("busy error", func(t *testing.T) {
		orig := errors.New("harness is busy: already running")
		norm := core.NormalizeError(orig)
		require.NotNil(t, norm)
		assert.Equal(t, core.ErrCodeBusy, norm.Code)
		assert.ErrorIs(t, norm, orig)
	})

	t.Run("tool error", func(t *testing.T) {
		orig := errors.New("tool execution failed: command not found")
		norm := core.NormalizeError(orig)
		require.NotNil(t, norm)
		assert.Equal(t, core.ErrCodeToolError, norm.Code)
		assert.ErrorIs(t, norm, orig)
	})

	t.Run("generic model error", func(t *testing.T) {
		orig := errors.New("model returned invalid JSON")
		norm := core.NormalizeError(orig)
		require.NotNil(t, norm)
		assert.Equal(t, core.ErrCodeModelError, norm.Code)
		assert.ErrorIs(t, norm, orig)
	})

	t.Run("nil error returns nil", func(t *testing.T) {
		assert.Nil(t, core.NormalizeError(nil))
	})

	t.Run("already-normalized error passes through", func(t *testing.T) {
		original := &core.AgentHarnessError{
			Code:    core.ErrCodeTimeout,
			Message: "custom timeout",
			Cause:   context.DeadlineExceeded,
		}
		norm := core.NormalizeError(original)
		require.NotNil(t, norm)
		assert.Equal(t, core.ErrCodeTimeout, norm.Code)
		assert.Equal(t, "custom timeout", norm.Message)
		// Should be the same instance (already normalized).
		assert.Same(t, original, norm)
	})

	t.Run("error message wrapping includes cause", func(t *testing.T) {
		orig := errors.New("connection refused by host")
		norm := core.NormalizeError(orig)
		require.NotNil(t, norm)
		// The Error() string should contain the code, the message, and the cause.
		fullMsg := norm.Error()
		assert.Contains(t, fullMsg, norm.Code)
		assert.Contains(t, fullMsg, orig.Error())
	})
}
