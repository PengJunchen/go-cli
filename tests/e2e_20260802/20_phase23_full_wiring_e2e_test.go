// Package e2e_20260802 contains end-to-end integration tests.
// This file verifies Phase 23 full runtime wiring across Phases 19-23:
// safety, context management, production resilience, agent capabilities,
// and UX convergence.
package e2e_20260802

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/cli"
	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// =============================================================================
// Phase 23 E2E: Full Runtime Wiring Verification (Phases 19-23)
//
// This file exercises the complete runtime stack wired across Phases 19-23:
//   - Phase 19: Safety (approval gate, mutation serialization)
//   - Phase 20: Context management (compaction, session persistence)
//   - Phase 21: Production resilience (retry, cost tracking)
//   - Phase 22: Agent capabilities (subagent dispatch, todo/task tools)
//   - Phase 23: UX convergence (doctor diagnostics, manual compaction, tracing)
//
// Helpers defined in other files of this package are reused:
//   - recordingTool, toolCall, toolCallWithArgs (03_approval_e2e_test.go, 16)
//   - buildTestCompactionHook, nopLoop (17_phase20_context_e2e_test.go)
//   - flakyModel (18_phase21_production_e2e_test.go)
//   - registerRealSubAgentFactory, mockHITLEmitter (19_phase22_capabilities_e2e_test.go)
// =============================================================================

// --- Shared test helpers ---

// stubDoctorChecker is a test DoctorChecker that returns a fixed result,
// avoiding real system checks (network, disk, etc.) during E2E verification.
type stubDoctorChecker struct {
	name   string
	status string
	msg    string
}

// Check implements cli.DoctorChecker.
func (c *stubDoctorChecker) Check(_ context.Context) cli.DoctorCheck {
	return cli.DoctorCheck{
		Name:    c.name,
		Status:  c.status,
		Message: c.msg,
	}
}

// =============================================================================
// AC-1: Approval gate blocks bash (Phase 19)
// =============================================================================

// TestE2E_Phase23_ApprovalGateBlocksBash verifies that the ApprovalMiddleware
// with SafetyPolicyClassifier denies bash tool calls and prevents the underlying
// tool from executing.
func TestE2E_Phase23_ApprovalGateBlocksBash(t *testing.T) {
	inner := &recordingTool{name: "bash"}
	tr := tools.NewDefaultToolRegistry()
	require.NoError(t, tr.Register(context.Background(), inner))

	mw := approval.NewApprovalMiddleware(
		approval.NewSafetyPolicyClassifier([]string{"bash"}),
		approval.NewInMemoryApprovalStore(),
		approval.WithAutoApprove(false),
	)
	wrapped := tools.NewMiddlewareToolRegistry(tr, mw.WrapToolCall)

	def, err := wrapped.Get(context.Background(), "bash")
	require.NoError(t, err)

	_, execErr := def.Execute(context.Background(), toolCall("bash"))
	require.ErrorIs(t, execErr, approval.ErrToolDenied)
	assert.False(t, inner.executed, "bash must not execute when denied by approval gate")
}

// =============================================================================
// AC-2: Compaction reduces history (Phase 20)
// =============================================================================

// TestE2E_Phase23_CompactionReducesHistory verifies that the compaction hook
// reduces AgentMessage history when the token budget is exceeded.
func TestE2E_Phase23_CompactionReducesHistory(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	hook := buildTestCompactionHook(compactor, estimator, 100)

	messages := make([]core.AgentMessage, 50)
	for i := range messages {
		messages[i] = core.AgentMessage{
			Role:    "user",
			Content: string(make([]byte, 100)),
		}
	}

	compacted, err := hook(context.Background(), messages)
	require.NoError(t, err)
	assert.Less(t, len(compacted), len(messages),
		"compaction must reduce history when token budget is exceeded")
}

// =============================================================================
// AC-3: Session save and restore (Phase 20)
// =============================================================================

// TestE2E_Phase23_SessionSaveAndRestore verifies that session entries persisted
// to a JSONL file can be loaded back by a new store instance opened at the same
// path.
func TestE2E_Phase23_SessionSaveAndRestore(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "session.jsonl")
	ctx := context.Background()

	store := session.NewJSONLSessionStore(storePath)
	require.NoError(t, store.Open(ctx))

	entries := []*session.SessionEntry{
		{ID: "entry-1", Type: session.EntryTypeUser, Content: "Hello", Timestamp: time.Now()},
		{ID: "entry-2", Type: session.EntryTypeAssistant, Content: "Hi there", Timestamp: time.Now()},
		{ID: "entry-3", Type: session.EntryTypeUser, Content: "How are you?", Timestamp: time.Now()},
	}
	for _, e := range entries {
		require.NoError(t, store.Append(ctx, e))
	}
	require.NoError(t, store.Save(ctx))

	// Verify the file was written to disk.
	info, err := os.Stat(storePath)
	require.NoError(t, err)
	assert.Greater(t, info.Size(), int64(0), "session file must have content")

	require.NoError(t, store.Close())

	// Reopen with a brand-new store instance at the same path.
	store2 := session.NewJSONLSessionStore(storePath)
	require.NoError(t, store2.Open(ctx))
	defer store2.Close()

	for _, expected := range entries {
		got, getErr := store2.Get(ctx, expected.ID)
		require.NoError(t, getErr, "entry %s should be restored from disk", expected.ID)
		assert.Equal(t, expected.Content, got.Content)
		assert.Equal(t, expected.Type, got.Type)
	}
}

// =============================================================================
// AC-4: Retry on transient error (Phase 21)
// =============================================================================

// TestE2E_Phase23_RetryOnTransientError verifies that ProductionModelWrapper
// retries failed LLM calls up to MaxAttempts and succeeds after transient
// failures. flakyModel is already defined in 18_phase21_production_e2e_test.go.
func TestE2E_Phase23_RetryOnTransientError(t *testing.T) {
	inner := &flakyModel{
		successAfter: 2, // First 2 calls fail, 3rd succeeds
		resp:         &llm.Message{Role: llm.RoleAssistant, Content: "recovered"},
	}

	retryPolicy := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    100 * time.Millisecond,
	})
	pw := production.NewProductionModelWrapper(
		production.WithWrapperRetryPolicy(retryPolicy),
	)

	wrapped := pw.WrapModel(inner)
	resp, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "recovered", resp.Content)
	assert.Equal(t, int32(3), atomic.LoadInt32(&inner.calls),
		"should take exactly 3 calls: 2 failures + 1 success")
}

// =============================================================================
// AC-5: Cost tracker records usage (Phase 21+23)
// =============================================================================

// TestE2E_Phase23_CostTrackerRecordsUsage verifies that the CostTracker records
// token usage from a successful wrapped model call. The flakyModel is used
// because it can carry Usage data (MockLLMServer does not populate Usage),
// allowing the cost tracker to compute a non-zero total.
func TestE2E_Phase23_CostTrackerRecordsUsage(t *testing.T) {
	inner := &flakyModel{
		successAfter: 0, // Always succeeds on the first call
		resp: &llm.Message{
			Role:    llm.RoleAssistant,
			Content: "hello from model",
			Usage:   &llm.Usage{InputTokens: 100, OutputTokens: 50},
		},
	}

	costTracker := production.NewCostTracker(nil)
	pw := production.NewProductionModelWrapper(
		production.WithWrapperCostTracker(costTracker),
		production.WithWrapperModelName("gpt-4o-mini"),
	)

	wrapped := pw.WrapModel(inner)
	_, err := wrapped.Generate(context.Background(), nil)
	require.NoError(t, err)

	assert.Equal(t, 1, costTracker.Calls(), "cost tracker should record exactly 1 call")
	assert.Greater(t, costTracker.Total(), 0.0, "total cost should be positive for non-zero usage")
}

// =============================================================================
// AC-6: SubAgent real execution (Phase 22)
// =============================================================================

// TestE2E_Phase23_SubAgentRealExecution verifies that dispatch_subagent returns
// a genuine LLM response instead of the simulated "response-1" placeholder,
// confirming the real SubAgent factory wiring.
func TestE2E_Phase23_SubAgentRealExecution(t *testing.T) {
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"sys", "e2e-phase23-subagent",
		mock.ConversationTurn{AssistantContent: "real subagent analysis"},
	))
	registerRealSubAgentFactory(t, model)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dispatcher := core.NewDefaultSubagentDispatcher(nil)
	subTool := core.NewSubagentTool(dispatcher)

	result, err := subTool.Execute(ctx, tools.ToolCall{
		ID:   "tc-1",
		Name: "dispatch_subagent",
		Args: map[string]any{
			"prompt":    "analyze the codebase",
			"id":        "e2e-phase23-sub",
			"max_turns": 3,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "real subagent analysis", result.Output)
	assert.NotEqual(t, "response-1", result.Output)
	assert.False(t, strings.HasPrefix(result.Output, "response-"),
		"output must not be the simulated placeholder")
}

// =============================================================================
// AC-7: TodoWrite tool available (Phase 22)
// =============================================================================

// TestE2E_Phase23_TodoWriteToolAvailable verifies that todo_write can be
// registered in a tool registry, add items, and list them back.
func TestE2E_Phase23_TodoWriteToolAvailable(t *testing.T) {
	tr := tools.NewDefaultToolRegistry()
	todoStore := tools.NewTodoStore()
	require.NoError(t, tr.Register(context.Background(), tools.NewTodoWriteTool(todoStore)))

	todoTool, err := tr.Get(context.Background(), "todo_write")
	require.NoError(t, err)

	// Add a todo item.
	addResult, err := todoTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-add",
		Name: "todo_write",
		Args: map[string]any{"action": "add", "content": "test todo", "priority": "high"},
	})
	require.NoError(t, err)
	assert.Contains(t, addResult.Output, "test todo")

	// List todos — the added item must appear.
	listResult, err := todoTool.Execute(context.Background(), tools.ToolCall{
		ID:   "tc-list",
		Name: "todo_write",
		Args: map[string]any{"action": "list"},
	})
	require.NoError(t, err)
	assert.Contains(t, listResult.Output, "test todo")
}

// =============================================================================
// AC-8: Manual compaction works (Phase 23)
// =============================================================================

// TestE2E_Phase23_ManualCompactionWorks verifies that the compaction hook
// reduces history when the token budget is exceeded and that all compacted
// messages retain valid role values.
func TestE2E_Phase23_ManualCompactionWorks(t *testing.T) {
	compactor := compaction.NewUnifiedCompactor()
	estimator := compaction.NewHeuristicTokenEstimator()
	hook := buildTestCompactionHook(compactor, estimator, 100)

	messages := make([]core.AgentMessage, 30)
	for i := range messages {
		messages[i] = core.AgentMessage{
			Role:    "user",
			Content: string(make([]byte, 100)),
		}
	}

	compacted, err := hook(context.Background(), messages)
	require.NoError(t, err)
	assert.Less(t, len(compacted), len(messages),
		"compaction must reduce history when token budget is exceeded")

	// Verify all compacted messages have valid roles.
	validRoles := map[string]bool{
		"system":    true,
		"user":      true,
		"assistant": true,
		"tool":      true,
	}
	for _, msg := range compacted {
		assert.True(t, validRoles[msg.Role],
			"compacted message must have a valid role, got %q", msg.Role)
	}
}

// =============================================================================
// AC-9: Doctor runner works (Phase 23)
// =============================================================================

// TestE2E_Phase23_DoctorRunnerWorks verifies that DoctorRunner executes
// registered checkers and that Format renders their names in the output.
func TestE2E_Phase23_DoctorRunnerWorks(t *testing.T) {
	runner := cli.NewDoctorRunner().WithCheckers([]cli.DoctorChecker{
		&stubDoctorChecker{name: "test-check-1", status: "pass", msg: "OK"},
		&stubDoctorChecker{name: "test-check-2", status: "pass", msg: "OK"},
	})

	results := runner.Run(context.Background())
	require.Len(t, results, 2, "should have 2 check results")
	for _, r := range results {
		assert.Equal(t, "pass", r.Status, "check %q should pass", r.Name)
	}

	formatted := cli.Format(results)
	assert.Contains(t, formatted, "test-check-1")
	assert.Contains(t, formatted, "test-check-2")
}

// =============================================================================
// AC-10: Trace ID consistency (Phase 19-23)
// =============================================================================

// TestE2E_Phase23_TraceIDConsistency verifies that parent and child spans share
// the same TraceID and that the child's ParentSpanID equals the parent's
// SpanID, confirming context propagation across the tracing pipeline.
func TestE2E_Phase23_TraceIDConsistency(t *testing.T) {
	exp := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("e2e-phase23-trace", exp)

	ctx := context.Background()
	parentSpan, parentCtx := tracer.Start(ctx, "parent.op", tracing.SpanKindInternal)
	parentID := parentSpan.SpanID()
	parentTraceID := parentSpan.TraceID()

	childSpan, _ := tracing.SpanFromContext(parentCtx, "child.op", tracing.SpanKindInternal)
	childID := childSpan.SpanID()

	childSpan.End()
	parentSpan.End()

	// Wait for asynchronous span export.
	require.Eventually(t, func() bool {
		return len(exp.Spans()) == 2
	}, 2*time.Second, 5*time.Millisecond, "should have 2 spans exported")

	spans := exp.Spans()

	// Locate parent and child spans by SpanID.
	var parentData, childData *tracing.SpanData
	for i := range spans {
		switch spans[i].SpanID {
		case parentID:
			parentData = &spans[i]
		case childID:
			childData = &spans[i]
		}
	}
	require.NotNil(t, parentData, "parent span should be exported")
	require.NotNil(t, childData, "child span should be exported")

	// Both spans must share the same TraceID.
	assert.Equal(t, parentTraceID, parentData.TraceID, "parent trace ID should match tracer")
	assert.Equal(t, parentTraceID, childData.TraceID,
		"child and parent should share the same trace ID")

	// Child's ParentSpanID must equal parent's SpanID.
	assert.Equal(t, parentID, childData.ParentSpanID,
		"child parent_span_id should equal parent span_id")
}
