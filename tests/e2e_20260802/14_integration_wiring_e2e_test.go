// Package e2e_20260802 contains rigorous, traceable integration/wiring tests
// (WT prefix) that verify modules are properly connected to the runtime. Each
// test follows the standards in integration_test_standards.md.
package e2e_20260802 //nolint:staticcheck

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
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
// Helper types
// =============================================================================

// resultModifyingToolMW is a custom ToolMiddleware that appends a suffix to
// every tool result output. Used by WT-MIDDLEWARE-001.
type resultModifyingToolMW struct {
	suffix string
}

var _ core.ToolMiddleware = (*resultModifyingToolMW)(nil)

func (m *resultModifyingToolMW) Name() string { return "result-modifying" }
func (m *resultModifyingToolMW) WrapToolCall(next func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)) func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	return func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		result, err := next(ctx, call)
		if err != nil || result == nil {
			return result, err
		}
		result.Output = result.Output + m.suffix
		return result, nil
	}
}

// loggingModelMiddleware is a custom ModelMiddleware that records when its
// WrapModel is called and when Generate/Stream is invoked through the wrapper.
// Used by WT-MIDDLEWARE-002.
type loggingModelMiddleware struct {
	mu       sync.Mutex
	wrapped  bool
	genCount int
}

var _ core.ModelMiddleware = (*loggingModelMiddleware)(nil)

func (m *loggingModelMiddleware) Name() string { return "logging-model-mw" }
func (m *loggingModelMiddleware) WrapModel(next llm.BaseChatModel) llm.BaseChatModel {
	m.mu.Lock()
	m.wrapped = true
	m.mu.Unlock()
	return &wrappedChatModel{inner: next, mw: m}
}

// wrappedChatModel is the BaseChatModel wrapper produced by
// loggingModelMiddleware.
type wrappedChatModel struct {
	inner llm.BaseChatModel
	mw    *loggingModelMiddleware
}

var _ llm.BaseChatModel = (*wrappedChatModel)(nil)

func (w *wrappedChatModel) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (*llm.Message, error) {
	w.mw.mu.Lock()
	w.mw.genCount++
	w.mw.mu.Unlock()
	return w.inner.Generate(ctx, msgs, opts...)
}

func (w *wrappedChatModel) Stream(ctx context.Context, msgs []llm.Message, opts ...llm.Option) (<-chan llm.MessageChunk, error) {
	w.mw.mu.Lock()
	w.mw.genCount++
	w.mw.mu.Unlock()
	return w.inner.Stream(ctx, msgs, opts...)
}

// gatedToolDef wraps a ToolDefinition and routes its Execute through an
// approval-gated executor function. Used by ET-PIPELINE-001.
type gatedToolDef struct {
	def  tools.ToolDefinition
	exec func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)
}

var _ tools.ToolDefinition = (*gatedToolDef)(nil)

func (d *gatedToolDef) Name() string        { return d.def.Name() }
func (d *gatedToolDef) Description() string { return d.def.Description() }
func (d *gatedToolDef) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	return d.exec(ctx, call)
}

// approvalGatedRegistry wraps a ToolRegistry so that every Get returns a
// gatedToolDef whose Execute is wrapped by an ApprovalMiddleware. List and
// Register delegate to the inner registry. Used by ET-PIPELINE-001.
type approvalGatedRegistry struct {
	inner tools.ToolRegistry
	mw    *approval.ApprovalMiddleware
}

var _ tools.ToolRegistry = (*approvalGatedRegistry)(nil)

func (r *approvalGatedRegistry) Register(ctx context.Context, def tools.ToolDefinition) error {
	return r.inner.Register(ctx, def)
}
func (r *approvalGatedRegistry) Get(ctx context.Context, name string) (tools.ToolDefinition, error) {
	def, err := r.inner.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return &gatedToolDef{def: def, exec: r.mw.WrapToolCall(def.Execute)}, nil
}
func (r *approvalGatedRegistry) List(ctx context.Context) ([]tools.ToolDefinition, error) {
	return r.inner.List(ctx)
}

// =============================================================================
// WT-APPROVAL: Approval Gate Wiring
// =============================================================================

// Test ID:   WT-APPROVAL-001
// Task ref:  Approval Gate blocks dangerous tool execution
// Gap ref:   LoopAgent does not currently wire approval middleware into its
//
//	tool execution path; this test proves the gate works when applied.
//
// Feature:   ApprovalMiddleware.WrapToolCall deny-first enforcement
func TestWT_Approval_GateBlocksBash(t *testing.T) {
	ctx := context.Background()

	// Create MockToolServer with bash tool.
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterBashTool("should not execute", 0)
	require.NoError(t, err)

	// Create DenyAllClassifier + ApprovalMiddleware.
	mw := approval.NewApprovalMiddleware(
		approval.DenyAllClassifier{},
		approval.NewInMemoryApprovalStore(),
	)

	// Base executor delegates to the MockToolServer's Execute.
	baseExec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return toolSrv.Execute(ctx, call)
	}

	// Wrap the executor with the approval gate.
	gatedExec := mw.WrapToolCall(baseExec)

	// Manually call the wrapped executor and verify ErrToolDenied.
	call := tools.ToolCall{ID: "call-1", Name: "bash", Args: map[string]any{"command": "rm -rf /"}}
	_, err = gatedExec(ctx, call)
	assert.ErrorIs(t, err, approval.ErrToolDenied)

	// Verify that without the approval gate, bash executes (proving the gap).
	result, err := baseExec(ctx, call)
	require.NoError(t, err)
	assert.Equal(t, "should not execute", result.Output)
}

// Test ID:   WT-APPROVAL-002
// Task ref:  Approval Gate allows safe tool execution
// Gap ref:   Verify AllowAll classifier permits tool calls through the gate.
// Feature:   ApprovalMiddleware.WrapToolCall allow path
func TestWT_Approval_GateAllowsReadFile(t *testing.T) {
	ctx := context.Background()

	// Create MockToolServer with read_file tool.
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("file content here")
	require.NoError(t, err)

	// Create AllowAllClassifier + ApprovalMiddleware.
	mw := approval.NewApprovalMiddleware(
		approval.AllowAllClassifier{},
		approval.NewInMemoryApprovalStore(),
	)

	// Base executor delegates to the MockToolServer's Execute.
	baseExec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return toolSrv.Execute(ctx, call)
	}

	// Wrap the executor with the approval gate.
	gatedExec := mw.WrapToolCall(baseExec)

	// Call with read_file and verify the result is returned (not denied).
	call := tools.ToolCall{ID: "call-1", Name: "read_file", Args: map[string]any{"path": "test.go"}}
	result, err := gatedExec(ctx, call)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "file content here", result.Output)

	// Verify the tool was actually executed.
	log := toolSrv.CallLog()
	require.Len(t, log, 1)
	assert.Equal(t, "read_file", log[0].ToolName)
}

// Test ID:   WT-APPROVAL-003
// Task ref:  Approval decision cache is reused across calls
// Gap ref:   Verify that repeated identical tool calls hit the session cache
//
//	and do not re-invoke the classifier.
//
// Feature:   ApprovalMiddleware in-session decision cache
func TestWT_Approval_DecisionCacheReused(t *testing.T) {
	ctx := context.Background()

	// Create a counting classifier wrapping a StaticClassifier.
	static := approval.NewStaticClassifier([]string{"read_file"}, []string{})
	counter := &countingClassifier{inner: static}

	// Create ApprovalMiddleware with the counting classifier.
	mw := approval.NewApprovalMiddleware(counter, approval.NewInMemoryApprovalStore())

	// Base executor returns a dummy result.
	baseExec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: "ok"}, nil
	}

	gatedExec := mw.WrapToolCall(baseExec)

	// Call with the same args twice.
	call := tools.ToolCall{ID: "call-1", Name: "read_file", Args: map[string]any{"path": "test.go"}}

	result1, err := gatedExec(ctx, call)
	require.NoError(t, err)
	assert.Equal(t, "ok", result1.Output)

	result2, err := gatedExec(ctx, call)
	require.NoError(t, err)
	assert.Equal(t, "ok", result2.Output)

	// Verify the classifier was only called once (cache hit on second call).
	assert.Equal(t, 1, counter.count(), "classifier should be called exactly once; second call should hit cache")
}

// =============================================================================
// WT-PRODUCTION: Resilience Component Wiring
// =============================================================================

// Test ID:   WT-PRODUCTION-001
// Task ref:  RetryPolicy error classification
// Gap ref:   Verify transient errors are retryable and fatal errors are not.
// Feature:   DefaultRetryPolicy.Classify + ShouldRetry
func TestWT_Production_RetryClassifiesErrors(t *testing.T) {
	ctx := context.Background()

	policy := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
	})
	dp, ok := policy.(*production.DefaultRetryPolicy)
	require.True(t, ok)

	t.Run("transient error is retryable", func(t *testing.T) {
		transientErr := production.NewError(production.ErrorTransient, errors.New("connection reset"))
		assert.Equal(t, production.ErrorTransient, dp.Classify(transientErr))
		assert.True(t, policy.ShouldRetry(ctx, transientErr, 0))
	})

	t.Run("timeout error is retryable", func(t *testing.T) {
		timeoutErr := errors.New("context deadline exceeded")
		assert.Equal(t, production.ErrorTimeout, dp.Classify(timeoutErr))
		assert.True(t, policy.ShouldRetry(ctx, timeoutErr, 0))
	})

	t.Run("rate limit error is retryable", func(t *testing.T) {
		rlErr := errors.New("rate limit exceeded")
		assert.Equal(t, production.ErrorRateLimit, dp.Classify(rlErr))
		assert.True(t, policy.ShouldRetry(ctx, rlErr, 0))
	})

	t.Run("fatal error is not retryable", func(t *testing.T) {
		fatalErr := errors.New("invalid argument")
		assert.Equal(t, production.ErrorFatal, dp.Classify(fatalErr))
		assert.False(t, policy.ShouldRetry(ctx, fatalErr, 0))
	})

	t.Run("categorized fatal error is not retryable", func(t *testing.T) {
		fatalErr := production.NewError(production.ErrorFatal, errors.New("bad request"))
		assert.Equal(t, production.ErrorFatal, dp.Classify(fatalErr))
		assert.False(t, policy.ShouldRetry(ctx, fatalErr, 0))
	})

	t.Run("max attempts reached blocks retry", func(t *testing.T) {
		transientErr := production.NewError(production.ErrorTransient, errors.New("connection reset"))
		assert.False(t, policy.ShouldRetry(ctx, transientErr, 3), "should not retry after max attempts")
	})
}

// Test ID:   WT-PRODUCTION-002
// Task ref:  CircuitBreaker three-state machine
// Gap ref:   Verify Closed -> Open transition and that Open state refuses
//
//	calls without invoking the wrapped function.
//
// Feature:   DefaultCircuitBreaker state machine
func TestWT_Production_CircuitBreakerStates(t *testing.T) {
	ctx := context.Background()

	cb := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 3,
		RecoveryTimeout:  30 * time.Second,
	})

	assert.Equal(t, production.CircuitClosed, cb.State(), "breaker should start closed")

	// Execute failing functions 3 times to trigger the threshold.
	callCount := 0
	failFn := func() (any, error) {
		callCount++
		return nil, errors.New("dependency down")
	}

	for i := 0; i < 3; i++ {
		_, err := cb.Execute(ctx, failFn)
		assert.Error(t, err, "call %d should fail", i)
	}
	assert.Equal(t, 3, callCount, "fn should have been called 3 times")

	// After 3 consecutive failures, breaker should be Open.
	assert.Equal(t, production.CircuitOpen, cb.State(), "breaker should be open after 3 failures")

	// Execute once more — verify it fails immediately with ErrCircuitOpen
	// and fn is NOT called.
	before := callCount
	_, err := cb.Execute(ctx, func() (any, error) {
		callCount++
		return "ok", nil
	})
	assert.ErrorIs(t, err, production.ErrCircuitOpen)
	assert.Equal(t, before, callCount, "fn should not be called when circuit is open")
}

// Test ID:   WT-PRODUCTION-003
// Task ref:  LoopDetector three-dimension detection
// Gap ref:   Verify each detection dimension (edit_count, test_failure,
//
//	same_tool_call) triggers independently.
//
// Feature:   DefaultLoopDetector multi-dimension observation
func TestWT_Production_LoopDetectorAllDimensions(t *testing.T) {
	ctx := context.Background()

	t.Run("edit_count dimension", func(t *testing.T) {
		ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
			EditThreshold: 3,
			Disposition:   production.DispositionTerminate,
		})
		for i := 0; i < 3; i++ {
			require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindEdit, Content: "main.go"}))
		}
		res := ld.Check(ctx)
		assert.True(t, res.Detected)
		assert.Equal(t, production.DimensionEditCount, res.Dimension)
		assert.Equal(t, production.DispositionTerminate, res.Disposition)
	})

	t.Run("test_failure dimension", func(t *testing.T) {
		ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
			TestFailureThreshold: 3,
			Disposition:          production.DispositionTerminate,
		})
		for i := 0; i < 3; i++ {
			require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindTestFailure, Content: "test failed"}))
		}
		res := ld.Check(ctx)
		assert.True(t, res.Detected)
		assert.Equal(t, production.DimensionTestFailure, res.Dimension)
	})

	t.Run("same_tool_call dimension", func(t *testing.T) {
		ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
			SameToolCallThreshold: 3,
			Disposition:           production.DispositionSteer,
		})
		payload := `{"tool":"read","args":{"path":"a.go"}}`
		for i := 0; i < 3; i++ {
			require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: payload}))
		}
		res := ld.Check(ctx)
		assert.True(t, res.Detected)
		assert.Equal(t, production.DimensionSameToolCall, res.Dimension)
		assert.Equal(t, production.DispositionSteer, res.Disposition)
	})
}

// =============================================================================
// WT-SESSION: Session Storage Wiring
// =============================================================================

// Test ID:   WT-SESSION-001
// Task ref:  JSONL session save and restore
// Gap ref:   Verify file-backed session store persists entries across
//
//	store instances.
//
// Feature:   JSONLSessionStore append + flush + reload
func TestWT_Session_JSONLSaveAndRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "session.jsonl")

	// Create store, append 3 entries, Save, Close.
	store1 := session.NewJSONLSessionStore(path)
	require.NoError(t, store1.Open(ctx))

	entries := []*session.SessionEntry{
		{ID: "e-1", Type: session.EntryTypeUser, Content: "hello", Timestamp: time.Now()},
		{ID: "e-2", Type: session.EntryTypeAssistant, Content: "hi there", Timestamp: time.Now().Add(time.Second)},
		{ID: "e-3", Type: session.EntryTypeTool, Content: "tool output", Timestamp: time.Now().Add(2 * time.Second)},
	}
	for _, e := range entries {
		require.NoError(t, store1.Append(ctx, e))
	}
	require.NoError(t, store1.Save(ctx))
	require.NoError(t, store1.Close())

	// Create new store with same path, Open/Load, verify entries restored.
	store2 := session.NewJSONLSessionStore(path)
	require.NoError(t, store2.Open(ctx))
	defer store2.Close() //nolint:errcheck,gosec

	for _, expected := range entries {
		got, err := store2.Get(ctx, expected.ID)
		require.NoError(t, err, "entry %s should be restored", expected.ID)
		assert.Equal(t, expected.ID, got.ID)
		assert.Equal(t, expected.Type, got.Type)
		assert.Equal(t, expected.Content, got.Content)
	}
}

// Test ID:   WT-SESSION-002
// Task ref:  SessionTree branch and context rebuild
// Gap ref:   Verify branching produces correct context for the branched leaf.
// Feature:   DefaultSessionTree Branch + BuildContext
func TestWT_Session_TreeBranchAndContext(t *testing.T) {
	ctx := context.Background()
	tree := session.NewDefaultSessionTree()

	// Append 5 entries linearly.
	entryIDs := make([]string, 5)
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("e-%d", i)
		entryIDs[i] = id
		parentID := ""
		if i > 0 {
			parentID = entryIDs[i-1] //nolint:gosec // bounded by i > 0 check
		}
		require.NoError(t, tree.Append(ctx, &session.SessionEntry{
			ID:        id,
			ParentID:  parentID,
			Type:      session.EntryTypeUser,
			Content:   fmt.Sprintf("content-%d", i),
			Timestamp: time.Now().Add(time.Duration(i) * time.Minute),
		}))
	}

	// MoveTo entry-3 and Branch.
	require.NoError(t, tree.MoveTo(ctx, "e-3"))
	require.NoError(t, tree.Branch(ctx, "e-3"))

	// Append 2 more entries on the branch.
	require.NoError(t, tree.Append(ctx, &session.SessionEntry{
		ID:        "b-0",
		ParentID:  "e-3",
		Type:      session.EntryTypeAssistant,
		Content:   "branch-0",
		Timestamp: time.Now().Add(10 * time.Minute),
	}))
	require.NoError(t, tree.Append(ctx, &session.SessionEntry{
		ID:        "b-1",
		ParentID:  "b-0",
		Type:      session.EntryTypeAssistant,
		Content:   "branch-1",
		Timestamp: time.Now().Add(11 * time.Minute),
	}))

	// BuildContext for the branch leaf.
	sc, err := tree.BuildContext(ctx, "b-1")
	require.NoError(t, err)
	require.NotNil(t, sc)

	// Verify context contains correct entries: e-0..e-3, b-0, b-1 (6 total).
	assert.Equal(t, 6, sc.EntryCount)

	contents := make([]string, len(sc.Messages))
	for i, m := range sc.Messages {
		contents[i] = m.Content
	}
	assert.Contains(t, contents, "content-0")
	assert.Contains(t, contents, "content-3")
	assert.Contains(t, contents, "branch-0")
	assert.Contains(t, contents, "branch-1")
	assert.NotContains(t, contents, "content-4", "e-4 is on the other branch and should not appear")
}

// Test ID:   WT-SESSION-003
// Task ref:  MemoryStore append and query
// Gap ref:   Verify in-memory store append-only semantics and duplicate
//
//	rejection.
//
// Feature:   MemoryStore Append + Get
func TestWT_Session_MemoryStoreAppendAndGet(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()

	// Append entries.
	e1 := &session.SessionEntry{ID: "m-1", Type: session.EntryTypeUser, Content: "first", Timestamp: time.Now()}
	e2 := &session.SessionEntry{ID: "m-2", Type: session.EntryTypeAssistant, Content: "second", Timestamp: time.Now()}

	require.NoError(t, store.Append(ctx, e1))
	require.NoError(t, store.Append(ctx, e2))

	// Get by ID, verify content.
	got1, err := store.Get(ctx, "m-1")
	require.NoError(t, err)
	assert.Equal(t, "first", got1.Content)
	assert.Equal(t, session.EntryTypeUser, got1.Type)

	got2, err := store.Get(ctx, "m-2")
	require.NoError(t, err)
	assert.Equal(t, "second", got2.Content)

	// Append duplicate ID, verify error.
	dup := &session.SessionEntry{ID: "m-1", Type: session.EntryTypeSystem, Content: "duplicate", Timestamp: time.Now()}
	err = store.Append(ctx, dup)
	assert.Error(t, err, "duplicate ID should be rejected")
}

// =============================================================================
// WT-COMPACTION: Compaction Wiring
// =============================================================================

// Test ID:   WT-COMPACTION-001
// Task ref:  MidTurnCompact threshold trigger
// Gap ref:   Verify compaction fires when token estimate exceeds the
//
//	configured threshold fraction of the budget.
//
// Feature:   MidTurnCompact.CompactIfNeeded
func TestWT_Compaction_MidTurnThresholdTrigger(t *testing.T) {
	ctx := context.Background()
	est := compaction.NewHeuristicTokenEstimator()

	// Create MidTurnCompact with threshold=0.5.
	mtc := compaction.NewMidTurnCompact(compaction.WithThresholdRatio(0.5))

	// Items that exceed the threshold (budget=50, items consume much more).
	items := []compaction.TurnItem{
		{Role: compaction.RoleUser, Content: strings.Repeat("u", 200)},
		{Role: compaction.RoleAssistant, Content: strings.Repeat("a", 200)},
	}

	// Use a recording compactor to capture whether compaction was triggered.
	rc := &recordingCompactor{
		name: "micro",
		out:  []compaction.TurnItem{{Role: compaction.RoleSystem, Content: "compacted", IsCompaction: true}},
	}

	out, res, err := mtc.CompactIfNeeded(ctx, items, 50, est, rc)
	require.NoError(t, err)
	assert.True(t, res.Triggered, "compaction should be triggered when items exceed threshold")
	assert.Equal(t, compaction.TriggerThreshold, res.Reason)
	assert.Len(t, out, 1)
}

// Test ID:   WT-COMPACTION-002
// Task ref:  UnifiedCompactor routing degradation
// Gap ref:   Verify the unified router uses Micro when budget is generous and
//
//	falls through to Truncating when Micro cannot fit.
//
// Feature:   UnifiedCompactor strategy routing
func TestWT_Compaction_UnifiedRouterDegrades(t *testing.T) {
	ctx := context.Background()
	est := compaction.NewHeuristicTokenEstimator()

	uni := compaction.NewUnifiedCompactor(
		compaction.WithMicro(compaction.NewMicroCompactor()),
		compaction.WithTruncating(compaction.NewTruncatingCompactor()),
	)

	t.Run("micro strategy used when budget is generous", func(t *testing.T) {
		items := []compaction.TurnItem{
			{Role: compaction.RoleSystem, Content: "sys"},
			{Role: compaction.RoleUser, Content: "hello"},
			{Role: compaction.RoleTool, ToolName: "read", ToolResult: strings.Repeat("t", 200)},
			{Role: compaction.RoleAssistant, Content: "hi"},
		}

		out, err := uni.Compact(ctx, items, 100, est)
		require.NoError(t, err)
		assert.Equal(t, compaction.StrategyMicro, uni.LastStrategy())
		assert.LessOrEqual(t, tokenSum(out, est), 100)
	})

	t.Run("truncating strategy used when micro cannot fit", func(t *testing.T) {
		items := make([]compaction.TurnItem, 0, 40)
		for i := 0; i < 40; i++ {
			items = append(items, compaction.TurnItem{
				Role:    compaction.RoleUser,
				Content: strings.Repeat("x", 100),
			})
		}

		out, err := uni.Compact(ctx, items, 20, est)
		require.NoError(t, err)
		assert.Equal(t, compaction.StrategyTruncating, uni.LastStrategy())
		assert.LessOrEqual(t, tokenSum(out, est), 20)
	})
}

// Test ID:   WT-COMPACTION-003
// Task ref:  Long conversation compaction stability
// Gap ref:   Verify compaction handles a 50-turn conversation without panic
//
//	and produces a reasonable output.
//
// Feature:   UnifiedCompactor + LongConversationGenerator integration
func TestWT_Compaction_LongConversationStability(t *testing.T) {
	ctx := context.Background()
	est := compaction.NewHeuristicTokenEstimator()

	// Generate a 50-turn conversation.
	gen := mock.NewLongConversationGenerator(50, 5, 3)
	template := gen.Generate()

	// Convert template turns to compaction TurnItems.
	items := make([]compaction.TurnItem, 0, len(template.Turns)*2)
	for i, turn := range template.Turns {
		items = append(items, compaction.TurnItem{
			Role:    compaction.RoleUser,
			Content: fmt.Sprintf("user message %d", i),
		})
		if turn.AssistantContent != "" {
			items = append(items, compaction.TurnItem{
				Role:    compaction.RoleAssistant,
				Content: turn.AssistantContent,
			})
		}
		for _, tc := range turn.AssistantToolCalls {
			items = append(items, compaction.TurnItem{
				Role:       compaction.RoleTool,
				ToolName:   tc.Name,
				ToolResult: fmt.Sprintf("result for %s at iteration %d", tc.Name, i),
			})
		}
	}

	require.NotEmpty(t, items, "should have generated items")

	// Run through compaction with a constrained budget.
	uni := compaction.NewUnifiedCompactor()
	out, err := uni.Compact(ctx, items, 500, est)
	require.NoError(t, err, "compaction should not panic or error")
	assert.NotEmpty(t, out, "output should not be empty")
	assert.LessOrEqual(t, tokenSum(out, est), 500, "output should fit within budget")
}

// =============================================================================
// WT-MIDDLEWARE: Middleware Wiring
// =============================================================================

// Test ID:   WT-MIDDLEWARE-001
// Task ref:  ToolMiddleware intercepts tool calls
// Gap ref:   Verify a custom ToolMiddleware can modify tool results.
// Feature:   core.ToolMiddleware WrapToolCall
func TestWT_Middleware_ToolMiddlewareIntercepts(t *testing.T) {
	ctx := context.Background()

	// Create a custom ToolMiddleware that appends a suffix to results.
	mw := &resultModifyingToolMW{suffix: " [intercepted]"}

	// Base executor returns a plain result.
	baseExec := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: "original"}, nil
	}

	// Wrap the executor.
	wrapped := mw.WrapToolCall(baseExec)

	// Verify the result is modified.
	result, err := wrapped(ctx, tools.ToolCall{ID: "c1", Name: "read_file", Args: map[string]any{}})
	require.NoError(t, err)
	assert.Equal(t, "original [intercepted]", result.Output)
}

// Test ID:   WT-MIDDLEWARE-002
// Task ref:  ModelMiddleware wraps model
// Gap ref:   Verify a custom ModelMiddleware wraps a BaseChatModel and is
//
//	invoked on Generate/Stream calls.
//
// Feature:   core.ModelMiddleware WrapModel
func TestWT_Middleware_ModelMiddlewareWraps(t *testing.T) {
	ctx := context.Background()

	// Create a MockLLMServer with a simple template.
	template := mock.NewConversationTemplate("S-MW", "model-mw-test",
		mock.ConversationTurn{AssistantContent: "hello from mock"},
	)
	mockServer := mock.NewMockLLMServer(template)

	// Create a custom ModelMiddleware.
	mw := &loggingModelMiddleware{}

	// Wrap the MockLLMServer.
	wrapped := mw.WrapModel(mockServer)

	// Verify the middleware was invoked during wrapping.
	assert.True(t, mw.wrapped, "WrapModel should set wrapped flag")

	// Call Generate through the wrapped model.
	resp, err := wrapped.Generate(ctx, []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
	})
	require.NoError(t, err)
	assert.Equal(t, "hello from mock", resp.Content)

	// Verify the middleware recorded the call.
	mw.mu.Lock()
	assert.Equal(t, 1, mw.genCount, "Generate should have been intercepted")
	mw.mu.Unlock()

	// Verify the underlying mock server also recorded the call.
	assert.Equal(t, 1, mockServer.CallCount())
}

// =============================================================================
// WT-SUBAGENT: SubAgent Lifecycle Wiring
// =============================================================================

// Test ID:   WT-SUBAGENT-001
// Task ref:  SubAgent state machine lifecycle
// Gap ref:   Verify SubAgent transitions from Idle -> Running -> Completed.
// Feature:   DefaultSubAgent lifecycle states
func TestWT_SubAgent_LifecycleStates(t *testing.T) {
	conf := core.SubAgentConfig{Name: "lifecycle-worker", MaxTurns: 3}
	sub := core.NewDefaultSubAgent(conf)
	sa := sub.(*core.DefaultSubAgent) //nolint:errcheck

	// Verify initial state is Idle.
	assert.Equal(t, core.SubAgentIdle, sa.State())

	// Run: state transitions to Running.
	ch, err := sub.Run(context.Background(), "do something")
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.Equal(t, core.SubAgentRunning, sa.State())

	// Drain events.
	for range ch {
	}

	// Wait for final result.
	msg, werr := sub.Wait(context.Background())
	require.NoError(t, werr)
	assert.Equal(t, "assistant", string(msg.Role))

	// Verify final state is Completed.
	assert.Equal(t, core.SubAgentCompleted, sa.State())
}

// Test ID:   WT-SUBAGENT-002
// Task ref:  SubAgent interrupt transitions
// Gap ref:   Verify SubAgent transitions to Interrupted on Interrupt.
// Feature:   DefaultSubAgent interrupt lifecycle
func TestWT_SubAgent_InterruptTransitions(t *testing.T) {
	conf := core.SubAgentConfig{Name: "interrupt-worker", MaxTurns: 50}
	sub := core.NewDefaultSubAgent(conf)
	sa := sub.(*core.DefaultSubAgent) //nolint:errcheck

	ch, err := sub.Run(context.Background(), "long task")
	require.NoError(t, err)

	// Interrupt the running sub-agent.
	require.NoError(t, sub.Interrupt(context.Background()))

	// Drain channel.
	for range ch {
	}

	// Verify state is Interrupted.
	_, werr := sub.Wait(context.Background())
	require.Error(t, werr)
	assert.Equal(t, core.SubAgentInterrupted, sa.State())
}

// =============================================================================
// WT-TRACE: Tracing Wiring
// =============================================================================

// Test ID:   WT-TRACE-001
// Task ref:  Trace span parent-child chain integrity
// Gap ref:   Verify child span inherits parent SpanID and both share TraceID.
// Feature:   Tracer.Start + SpanFromContext parent-child propagation
func TestWT_Trace_SpanParentChainComplete(t *testing.T) {
	exp := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("wt-trace-001", exp)
	ctx := context.Background()

	// Create parent span.
	parentSpan, parentCtx := tracer.Start(ctx, "parent.op", tracing.SpanKindInternal)
	parentID := parentSpan.SpanID()
	parentTraceID := parentSpan.TraceID()

	// Create child span from parent context.
	childSpan, _ := tracing.SpanFromContext(parentCtx, "child.op", tracing.SpanKindInternal)
	childID := childSpan.SpanID()

	// End both spans.
	childSpan.End()
	parentSpan.End()

	// Wait for async export.
	time.Sleep(200 * time.Millisecond)

	spans := exp.Spans()
	require.Len(t, spans, 2, "should have 2 spans exported")

	// Find parent and child by SpanID.
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

	// Verify child.ParentSpanID == parent.SpanID.
	assert.Equal(t, parentID, childData.ParentSpanID, "child parent_span_id should equal parent span_id")

	// Verify both have the same TraceID.
	assert.Equal(t, parentTraceID, parentData.TraceID)
	assert.Equal(t, parentTraceID, childData.TraceID, "child and parent should share trace_id")
}

// Test ID:   WT-TRACE-002
// Task ref:  Trace multi-exporter fan-out
// Gap ref:   Verify MultiExporter delivers each span to every constituent
//
//	exporter.
//
// Feature:   tracing.NewMultiExporter fan-out
func TestWT_Trace_MultiExporterFanOut(t *testing.T) {
	exp1 := mock.NewMockTraceExporter()
	exp2 := mock.NewMockTraceExporter()

	multi := tracing.NewMultiExporter(exp1, exp2)
	tracer := tracing.NewTracer("wt-trace-002", multi)

	span, _ := tracer.Start(context.Background(), "fan.out.op", tracing.SpanKindInternal)
	span.SetAttributes(tracing.Attribute{Key: "key1", Value: "val1"})
	span.SetStatus(tracing.SpanStatusOK, "")
	span.End()

	// Wait for async export.
	time.Sleep(200 * time.Millisecond)

	assert.GreaterOrEqual(t, exp1.SpanCount(), 1, "exporter 1 should receive the span")
	assert.GreaterOrEqual(t, exp2.SpanCount(), 1, "exporter 2 should receive the span")

	// Verify both received the same span name.
	spans1 := exp1.Spans()
	spans2 := exp2.Spans()
	require.NotEmpty(t, spans1)
	require.NotEmpty(t, spans2)
	assert.Equal(t, "fan.out.op", spans1[0].Name)
	assert.Equal(t, "fan.out.op", spans2[0].Name)
}

// =============================================================================
// ET-PIPELINE: End-to-End Pipeline Integration
// =============================================================================

// Test ID:   ET-PIPELINE-001
// Task ref:  Full pipeline integration (Approval + Tool + Trace)
// Gap ref:   LoopAgent does not wire approval; this test manually wraps the
//
//	tool registry with an ApprovalMiddleware to prove the gate works
//	end-to-end with LLM-driven tool calls.
//
// Feature:   MockLLMServer -> LoopAgent -> MockToolServer (approval-gated) + Trace
func TestET_Pipeline_ApprovalToolTrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- Mock LLM Server ---
	// Turn 0: bash tool call (denied by approval)
	// Turn 1: read_file tool call (allowed by approval)
	// Turn 2: final text response (no tool calls, ends the loop)
	template := mock.NewConversationTemplate("S-ET-001", "approval-pipeline",
		mock.ConversationTurn{
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-bash", Name: "bash", Args: map[string]any{"command": "rm -rf /"}},
			},
		},
		mock.ConversationTurn{
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-read", Name: "read_file", Args: map[string]any{"path": "test.go"}},
			},
		},
		mock.ConversationTurn{
			AssistantContent: "Done reading the file.",
		},
	)
	mockLLM := mock.NewMockLLMServer(template)

	// --- Mock Tool Server ---
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterBashTool("should not execute", 0)
	require.NoError(t, err)
	_, err = toolSrv.RegisterReadFileTool("file content")
	require.NoError(t, err)

	// --- Approval Middleware ---
	// StaticClassifier allows read_file, denies bash (deny-by-default for
	// tools not in the allowlist).
	classifier := approval.NewStaticClassifier([]string{"read_file"}, []string{"bash"})
	mw := approval.NewApprovalMiddleware(classifier, approval.NewInMemoryApprovalStore())

	// Wrap the tool registry with the approval gate.
	gatedRegistry := &approvalGatedRegistry{inner: toolSrv, mw: mw}

	// --- Tracing ---
	mockExp := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("et-pipeline-001", mockExp)
	rootSpan, traceCtx := tracer.Start(ctx, "test-root", tracing.SpanKindInternal)

	// --- LoopAgent + AgentImpl ---
	loop := core.NewLoopAgent(
		core.WithLLM(mockLLM),
		core.WithTools(gatedRegistry),
		core.WithMaxIterations(10),
	)
	agent := core.NewAgentImpl("et-pipeline-001", loop)

	// Run the agent.
	result, err := agent.Run(traceCtx, core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "run the pipeline",
	})
	rootSpan.End()

	require.NoError(t, err)
	assert.True(t, result.Success)
	assert.Equal(t, "Done reading the file.", result.Message)

	// --- Verify events ---
	events := agent.Events()
	require.NotEmpty(t, events)

	// Collect non-incremental event kinds and contents.
	var toolCallIdx, toolResultIdx int
	var bashDenied bool
	var readFileAllowed bool
	for _, ev := range events {
		switch ev.Kind {
		case "tool_call":
			toolCallIdx++
			// When Content == "bash", the next tool_result should contain the denial error.
		case "tool_result":
			toolResultIdx++
			if strings.Contains(ev.Content, "denied") {
				bashDenied = true
			}
			if ev.Content == "mock" || ev.Content == "file content" {
				readFileAllowed = true
			}
		}
	}

	assert.GreaterOrEqual(t, toolCallIdx, 2, "should have at least 2 tool_call events")
	assert.GreaterOrEqual(t, toolResultIdx, 2, "should have at least 2 tool_result events")
	assert.True(t, bashDenied, "bash tool call should have been denied")
	assert.True(t, readFileAllowed, "read_file tool call should have been allowed")

	// --- Verify trace spans ---
	time.Sleep(200 * time.Millisecond)
	spans := mockExp.Spans()
	require.NotEmpty(t, spans, "trace should have exported spans")

	// Verify a "loop.run" span exists.
	hasLoopSpan := false
	for _, s := range spans {
		if s.Name == "loop.run" {
			hasLoopSpan = true
			break
		}
	}
	assert.True(t, hasLoopSpan, "should have a loop.run span")
}

// Test ID:   ET-PIPELINE-002
// Task ref:  Multi-turn conversation event sequence verification
// Gap ref:   Verify the full event sequence (message, tool_call, tool_result,
//
//	message, ...) is emitted correctly across multiple turns.
//
// Feature:   HarnessImpl EventStream multi-turn event flow
func TestET_Pipeline_MultiTurnEventSequence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 3-turn conversation template:
	// Turn 1: content + read_file tool call
	// Turn 2: content + bash tool call
	// Turn 3: content only (ends the loop)
	template := mock.NewConversationTemplate("S-ET-002", "multi-turn",
		mock.ConversationTurn{
			AssistantContent: "Let me read the file.",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-1", Name: "read_file", Args: map[string]any{"path": "test.go"}},
			},
		},
		mock.ConversationTurn{
			AssistantContent: "Now let me run the tests.",
			AssistantToolCalls: []mock.ExpectedToolCall{
				{ID: "call-2", Name: "bash", Args: map[string]any{"command": "go test"}},
			},
		},
		mock.ConversationTurn{
			AssistantContent: "All done!",
		},
	)
	mockLLM := mock.NewMockLLMServer(template)

	// Mock Tool Server with read_file and bash.
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("file content")
	require.NoError(t, err)
	_, err = toolSrv.RegisterBashTool("test passed", 0)
	require.NoError(t, err)

	// LoopAgent + AgentImpl + HarnessImpl.
	loop := core.NewLoopAgent(
		core.WithLLM(mockLLM),
		core.WithTools(toolSrv),
		core.WithMaxIterations(10),
	)
	agent := core.NewAgentImpl("et-pipeline-002", loop)
	harness := core.NewHarnessImpl(agent, core.WithEventBuffer(64))

	// Submit a message and collect all events from the EventStream.
	stream, err := harness.Submit(ctx, "run multi-turn test")
	require.NoError(t, err)
	require.NotNil(t, stream)

	var allEvents []core.AgentEvent
	for ev := range stream.Events() {
		allEvents = append(allEvents, ev)
	}
	require.NotEmpty(t, allEvents, "should have received events")

	// Filter out incremental events for sequence verification.
	var seq []core.AgentEvent
	for _, ev := range allEvents {
		if !ev.Incremental {
			seq = append(seq, ev)
		}
	}

	// Verify the event sequence:
	// message, tool_call, tool_result, message, tool_call, tool_result, message, done
	expectedKinds := []string{
		"message",     // Turn 1 content
		"tool_call",   // read_file
		"tool_result", // read_file result
		"message",     // Turn 2 content
		"tool_call",   // bash
		"tool_result", // bash result
		"message",     // Turn 3 content
		"done",        // harness done
	}

	require.Len(t, seq, len(expectedKinds),
		"event count mismatch; got %d events: %v", len(seq), eventKinds(seq))

	for i, expected := range expectedKinds {
		assert.Equal(t, expected, seq[i].Kind,
			"event %d: expected %s, got %s (content: %q)",
			i, expected, seq[i].Kind, seq[i].Content)
	}

	// Verify the final result.
	result, err := stream.Result()
	require.NoError(t, err)
	assert.Equal(t, "All done!", result.Content)
}

// =============================================================================
// Helpers (local to this file)
// =============================================================================

// eventKinds extracts the Kind field from a slice of AgentEvents.
func eventKinds(events []core.AgentEvent) []string {
	kinds := make([]string, len(events))
	for i, ev := range events {
		kinds[i] = ev.Kind
	}
	return kinds
}
