// Package e2e_20260802 contains end-to-end integration tests for gap-filling
// FOUNDATION infrastructure. It exercises ModelMiddleware chain wiring,
// OutputGuardMiddleware blocking, PlanMode/Auto classifiers, SubAgent lifecycle,
// LoopDetector dimensions and reset, SessionTree deep branch + context rebuild,
// SessionTree compaction folding, Provider three-layer composition,
// FileMutationQueue serialization, compaction MidTurn threshold and Unified
// strategy routing, multi-exporter tracing fan-out, PermissionModeResolver,
// CircuitBreaker + RetryPolicy integration, and Config five-layer merge.
package e2e_20260802

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/approval"
	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/config"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// =============================================================================
// 1. ModelMiddleware Chain Wiring
// =============================================================================

// loggingModelMW is a pass-through ModelMiddleware that records its execution
// order in a shared log slice.
type loggingModelMW struct {
	name string
	log  *[]string
}

var _ extension.ModelMiddleware = (*loggingModelMW)(nil)

func (m *loggingModelMW) Name() string { return m.name }
func (m *loggingModelMW) WrapModel(next extension.ModelFunc) extension.ModelFunc {
	return func(ctx context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		*m.log = append(*m.log, m.name)
		return next(ctx, req)
	}
}

func TestModelMiddlewareChainWiring(t *testing.T) {
	var log []string

	m1 := &loggingModelMW{name: "m1", log: &log}
	m2 := &loggingModelMW{name: "m2", log: &log}
	m3 := &loggingModelMW{name: "m3", log: &log}

	base := func(_ context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: "base:" + req.Prompt}, nil
	}

	// Chain: m1 -> m2 -> m3 -> base
	chained := m1.WrapModel(m2.WrapModel(m3.WrapModel(base)))

	resp, err := chained(context.Background(), extension.ModelRequest{Prompt: "hello"})
	require.NoError(t, err)
	assert.Equal(t, "base:hello", resp.Text)
	assert.Equal(t, []string{"m1", "m2", "m3"}, log)

	// Verify interface compliance.
	var _ extension.ModelMiddleware = m1
	var _ extension.ModelMiddleware = m2
	var _ extension.ModelMiddleware = m3
}

// =============================================================================
// 2. OutputGuardMiddleware Blocks Malicious
// =============================================================================

func TestOutputGuardMiddlewareBlocksMalicious(t *testing.T) {
	ctx := context.Background()

	// Create a guard chain: PII + CodeInjection.
	chain := production.NewOutputGuardChain([]production.OutputGuard{
		production.NewPIIOutputGuard(),
		production.NewCodeInjectionGuard(),
	})

	// Wrap as ModelMiddleware.
	mw := production.NewOutputGuardMiddleware(chain)

	// Base model func that returns malicious content.
	base := func(_ context.Context, _ extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: "please DROP TABLE users"}, nil
	}
	wrapped := mw.WrapModel(base)

	// Verify blocked.
	resp, err := wrapped(ctx, extension.ModelRequest{Prompt: "hack"})
	require.NoError(t, err)
	assert.NotEqual(t, "please DROP TABLE users", resp.Text)
	assert.Contains(t, resp.Text, "Blocked")

	// Safe content should pass through.
	safeBase := func(_ context.Context, _ extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: "hello world"}, nil
	}
	safeWrapped := mw.WrapModel(safeBase)
	safeResp, err := safeWrapped(ctx, extension.ModelRequest{Prompt: "greet"})
	require.NoError(t, err)
	assert.Equal(t, "hello world", safeResp.Text)
}

// =============================================================================
// 3. PlanMode PermissionClassifier
// =============================================================================

func TestPlanModeClassifierDeniesAll(t *testing.T) {
	ctx := context.Background()

	plan := approval.NewPlanClassifier()
	assert.Equal(t, "plan-classifier", plan.Name())

	// PlanClassifier returns Ask for every tool call.
	assert.Equal(t, approval.Ask, plan.Classify(ctx, toolCall("read")))
	assert.Equal(t, approval.Ask, plan.Classify(ctx, toolCall("write")))
	assert.Equal(t, approval.Ask, plan.Classify(ctx, toolCall("bash")))

	// AutoClassifier returns Allow for safe tools and unknown tools.
	auto := approval.NewAutoClassifier([]string{"read"}, []string{"bash"})
	assert.Equal(t, "auto-classifier", auto.Name())
	assert.Equal(t, approval.Allow, auto.Classify(ctx, toolCall("read"))) // safe
	assert.Equal(t, approval.Ask, auto.Classify(ctx, toolCall("bash")))   // dangerous
	assert.Equal(t, approval.Allow, auto.Classify(ctx, toolCall("grep"))) // unknown → default Allow
}

// =============================================================================
// 4. SubAgent Full Lifecycle
// =============================================================================

func TestSubAgentFullLifecycle(t *testing.T) {
	conf := core.SubAgentConfig{Name: "lifecycle-worker", MaxTurns: 3}
	sub := core.NewDefaultSubAgent(conf)

	// Initial state: Idle.
	sa := sub.(*core.DefaultSubAgent)
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
	assert.Equal(t, core.SubAgentCompleted, sa.State())
}

// =============================================================================
// 5. SubAgent Interrupt
// =============================================================================

func TestSubAgentInterruptLifecycle(t *testing.T) {
	conf := core.SubAgentConfig{Name: "interrupt-worker", MaxTurns: 50}
	sub := core.NewDefaultSubAgent(conf)
	sa := sub.(*core.DefaultSubAgent)

	ch, err := sub.Run(context.Background(), "long task")
	require.NoError(t, err)

	// Interrupt the running sub-agent.
	ierr := sub.Interrupt(context.Background())
	require.NoError(t, ierr)

	// Drain channel.
	for range ch {
	}

	// Verify state is Interrupted.
	_, werr := sub.Wait(context.Background())
	require.Error(t, werr)
	assert.Equal(t, core.SubAgentInterrupted, sa.State())
}

// =============================================================================
// 6. LoopDetector Disposition Terminate
// =============================================================================

func TestLoopDetectorDispositionTerminate(t *testing.T) {
	ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
		EditThreshold: 3,
		Disposition:   production.DispositionTerminate,
	})
	ctx := context.Background()

	// Observe 3 edit events to the same file.
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindEdit, Content: "main.go"}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindEdit, Content: "main.go"}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindEdit, Content: "main.go"}))

	res := ld.Check(ctx)
	assert.True(t, res.Detected)
	assert.Equal(t, production.DimensionEditCount, res.Dimension)
	assert.Equal(t, production.DispositionTerminate, res.Disposition)
	assert.GreaterOrEqual(t, res.Count, 3)
}

// =============================================================================
// 7. LoopDetector SameToolCall Dimension
// =============================================================================

func TestLoopDetectorSameToolCall(t *testing.T) {
	ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
		SameToolCallThreshold: 3,
		Disposition:           production.DispositionSteer,
	})
	ctx := context.Background()

	// Observe 3 identical tool_call events.
	payload := `{"tool":"read","args":{"path":"a.go"}}`
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: payload}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: payload}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: payload}))

	res := ld.Check(ctx)
	assert.True(t, res.Detected)
	assert.Equal(t, production.DimensionSameToolCall, res.Dimension)
	assert.Equal(t, production.DispositionSteer, res.Disposition)
}

// =============================================================================
// 8. LoopDetector Reset
// =============================================================================

func TestLoopDetectorResetClearsState(t *testing.T) {
	ld := production.NewDefaultLoopDetector(production.LoopDetectionConfig{
		SameToolCallThreshold: 2,
	})
	ctx := context.Background()

	// Observe events until detected.
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: `{"tool":"x"}`}))
	require.NoError(t, ld.Observe(ctx, core.AgentEvent{Kind: production.KindToolCall, Content: `{"tool":"x"}`}))
	assert.True(t, ld.Check(ctx).Detected)

	// Reset.
	require.NoError(t, ld.Reset(ctx))

	// After reset, no detection.
	res := ld.Check(ctx)
	assert.False(t, res.Detected)
}

// =============================================================================
// 9. SessionTree Deep Branch + Context Rebuild
// =============================================================================

func TestSessionTreeDeepBranchAndRebuild(t *testing.T) {
	ctx := context.Background()
	tree := session.NewDefaultSessionTree()

	// Append linear chain of 10 entries.
	entryIDs := make([]string, 10)
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("e-%d", i)
		entryIDs[i] = id
		parentID := ""
		if i > 0 {
			parentID = entryIDs[i-1]
		}
		err := tree.Append(ctx, &session.SessionEntry{
			ID:        id,
			ParentID:  parentID,
			Type:      session.EntryTypeUser,
			Content:   fmt.Sprintf("content-%d", i),
			Timestamp: time.Now().Add(time.Duration(i) * time.Minute),
		})
		require.NoError(t, err)
	}
	// The current leaf is the first appended entry by default; move to the
	// latest entry to reflect a normal forward traversal.
	require.NoError(t, tree.MoveTo(ctx, "e-9"))
	assert.Equal(t, "e-9", tree.CurrentLeaf())

	// MoveTo entry 5 and Branch.
	require.NoError(t, tree.MoveTo(ctx, "e-5"))
	require.NoError(t, tree.Branch(ctx, "e-5"))

	// Append 3 more entries on the branch.
	branchIDs := make([]string, 3)
	prevID := "e-5"
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("b-%d", i)
		branchIDs[i] = id
		err := tree.Append(ctx, &session.SessionEntry{
			ID:        id,
			ParentID:  prevID,
			Type:      session.EntryTypeAssistant,
			Content:   fmt.Sprintf("branch-%d", i),
			Timestamp: time.Now().Add(time.Duration(10+i) * time.Minute),
		})
		require.NoError(t, err)
		prevID = id
	}

	// BuildContext for the branch leaf.
	sc, err := tree.BuildContext(ctx, "b-2")
	require.NoError(t, err)
	require.NotNil(t, sc)

	// Verify correct entries in context: e-0..e-5, b-0..b-2.
	assert.Equal(t, 9, sc.EntryCount)
	contents := make([]string, len(sc.Messages))
	for i, m := range sc.Messages {
		contents[i] = m.Content
	}
	assert.Contains(t, contents, "content-0")
	assert.Contains(t, contents, "content-5")
	assert.Contains(t, contents, "branch-0")
	assert.Contains(t, contents, "branch-2")
	assert.NotContains(t, contents, "content-6") // e-6..e-9 are on the other branch
}

// =============================================================================
// 10. SessionTree Compaction Folding
// =============================================================================

func TestSessionTreeCompactionFolding(t *testing.T) {
	ctx := context.Background()
	tree := session.NewDefaultSessionTree()

	// Append entries including a Compaction entry.
	require.NoError(t, tree.Append(ctx, &session.SessionEntry{
		ID:        "e-0",
		Type:      session.EntryTypeUser,
		Content:   "original user msg",
		Timestamp: time.Now(),
	}))
	require.NoError(t, tree.Append(ctx, &session.SessionEntry{
		ID:        "e-1",
		ParentID:  "e-0",
		Type:      session.EntryTypeCompaction,
		Content:   "old conversation details",
		Summary:   "Summary: the user asked about X and Y",
		Timestamp: time.Now().Add(time.Minute),
	}))
	require.NoError(t, tree.Append(ctx, &session.SessionEntry{
		ID:        "e-2",
		ParentID:  "e-1",
		Type:      session.EntryTypeAssistant,
		Content:   "latest assistant response",
		Timestamp: time.Now().Add(2 * time.Minute),
	}))

	// Use ContextManager to build context with compaction folding.
	cm := session.NewDefaultContextManager(tree)
	sc, err := cm.BuildContext(ctx, "e-2")
	require.NoError(t, err)
	require.NotNil(t, sc)

	// The compaction entry's Summary should replace earlier entries in the
	// effective context (the ContextManager folds compaction entries).
	assert.GreaterOrEqual(t, sc.EntryCount, 2)
	// Verify the compaction summary is present.
	hasSummary := false
	for _, m := range sc.Messages {
		if m.Content == "Summary: the user asked about X and Y" {
			hasSummary = true
		}
	}
	assert.True(t, hasSummary, "compaction summary should appear in context")
}

// =============================================================================
// 11. Provider Composition Three-Layer Priority
// =============================================================================

func TestProviderCompositionThreeLayerPriority(t *testing.T) {
	ctx := context.Background()

	// Config layer: provider named "openai" (different from builtin).
	configOpenAI := llm.NewEinoProvider(llm.WithProviderName("openai"), llm.WithDefaultModel("config-model"))

	// Extension layer: provider named "openai" (yet different instance).
	extOpenAI := llm.NewEinoProvider(llm.WithProviderName("openai"), llm.WithDefaultModel("ext-model"))

	composer := llm.NewDefaultProviderComposer(
		llm.WithConfigProviders([]llm.ModelProvider{configOpenAI}),
		llm.WithExtensionProviders([]llm.ModelProvider{extOpenAI}),
	)

	reg, err := composer.Compose(ctx)
	require.NoError(t, err)

	got, err := reg.Get("openai")
	require.NoError(t, err)

	// Extension "openai" should win over config and builtin.
	assert.Same(t, extOpenAI, got, "extension provider should win over config and builtin")
}

// =============================================================================
// 12. Provider Composition Extension > Config
// =============================================================================

func TestProviderCompositionExtensionOverridesConfig(t *testing.T) {
	ctx := context.Background()

	configProvider := llm.NewEinoProvider(llm.WithProviderName("custom-1"), llm.WithDefaultModel("config-custom"))
	extProvider := llm.NewEinoProvider(llm.WithProviderName("custom-1"), llm.WithDefaultModel("ext-custom"))

	composer := llm.NewDefaultProviderComposer(
		llm.WithConfigProviders([]llm.ModelProvider{configProvider}),
		llm.WithExtensionProviders([]llm.ModelProvider{extProvider}),
	)

	reg, err := composer.Compose(ctx)
	require.NoError(t, err)

	got, err := reg.Get("custom-1")
	require.NoError(t, err)
	assert.Same(t, extProvider, got, "extension source should win over config source")
}

// =============================================================================
// 13. FileMutationQueue Serialization
// =============================================================================

func TestFileMutationQueueSerialization(t *testing.T) {
	ctx := context.Background()

	var mu sync.Mutex
	var appliedOrder []string

	handler := func(_ context.Context, m tools.FileMutation) error {
		mu.Lock()
		appliedOrder = append(appliedOrder, m.Operation+":"+m.FilePath)
		mu.Unlock()
		return nil
	}

	q := tools.NewDefaultFileMutationQueue(tools.WithMutationHandler(handler))

	// Enqueue 5 mutations to the same file — must apply in FIFO order.
	sameFile := "/tmp/serialize-test.txt"
	var results []<-chan tools.FileMutationResult
	for i := 0; i < 5; i++ {
		ch, err := q.Enqueue(ctx, tools.FileMutation{
			FilePath:  sameFile,
			Operation: fmt.Sprintf("op%d", i),
			Content:   fmt.Sprintf("content-%d", i),
			ToolName:  "write",
		})
		require.NoError(t, err)
		results = append(results, ch)
	}

	// Wait for all mutations to apply.
	for _, ch := range results {
		res := <-ch
		assert.True(t, res.Success, "mutation should succeed")
	}

	// Verify FIFO order for same-file mutations.
	mu.Lock()
	require.Len(t, appliedOrder, 5)
	for i := 0; i < 5; i++ {
		assert.Equal(t, fmt.Sprintf("op%d:%s", i, sameFile), appliedOrder[i],
			"same-file mutations should apply in FIFO order")
	}
	mu.Unlock()

	// Enqueue 2 mutations to different files — they can apply in parallel.
	diffResults := make([]<-chan tools.FileMutationResult, 2)
	for i := 0; i < 2; i++ {
		ch, err := q.Enqueue(ctx, tools.FileMutation{
			FilePath:  fmt.Sprintf("/tmp/parallel-%d.txt", i),
			Operation: fmt.Sprintf("par%d", i),
			Content:   "data",
			ToolName:  "write",
		})
		require.NoError(t, err)
		diffResults[i] = ch
	}
	for _, ch := range diffResults {
		res := <-ch
		assert.True(t, res.Success, "different-file mutation should succeed")
	}

	// Clean up the queue.
	qImpl := q.(*tools.DefaultFileMutationQueue)
	require.NoError(t, qImpl.Close())
}

// =============================================================================
// 14. Compaction Split-Turn Boundary
// =============================================================================

func TestCompactionMidTurnThreshold(t *testing.T) {
	ctx := context.Background()
	est := compaction.NewHeuristicTokenEstimator()

	// Create MidTurnCompact with threshold ratio 0.5.
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

// =============================================================================
// 15. Unified Compactor Route Order
// =============================================================================

func TestUnifiedCompactorRouteOrder(t *testing.T) {
	ctx := context.Background()
	est := compaction.NewHeuristicTokenEstimator()

	t.Run("micro strategy used when budget is generous", func(t *testing.T) {
		// Items with large tool results that micro can compact by placeholdering.
		items := []compaction.TurnItem{
			{Role: compaction.RoleSystem, Content: "sys"},
			{Role: compaction.RoleUser, Content: "hello"},
			{Role: compaction.RoleTool, ToolName: "read", ToolResult: strings.Repeat("t", 200)},
			{Role: compaction.RoleAssistant, Content: "hi"},
		}

		uni := compaction.NewUnifiedCompactor()
		out, err := uni.Compact(ctx, items, 100, est)
		require.NoError(t, err)
		assert.Equal(t, compaction.StrategyMicro, uni.LastStrategy())
		assert.LessOrEqual(t, tokenSum(out, est), 100)
	})

	t.Run("truncating strategy used when micro cannot fit", func(t *testing.T) {
		// Many non-tool entries that micro cannot shrink enough.
		items := make([]compaction.TurnItem, 0, 40)
		for i := 0; i < 40; i++ {
			items = append(items, compaction.TurnItem{
				Role:    compaction.RoleUser,
				Content: strings.Repeat("x", 100),
			})
		}

		uni := compaction.NewUnifiedCompactor()
		out, err := uni.Compact(ctx, items, 20, est)
		require.NoError(t, err)
		assert.Equal(t, compaction.StrategyTruncating, uni.LastStrategy())
		assert.LessOrEqual(t, tokenSum(out, est), 20)
	})
}

// =============================================================================
// 16. Multi-Exporter Tracing Fan-out
// =============================================================================

func TestMultiExporterTracingFanOut(t *testing.T) {
	var buf1, buf2 bytes.Buffer

	exp1 := tracing.NewStdoutTraceExporterWithWriter(false, &buf1)
	exp2 := tracing.NewStdoutTraceExporterWithWriter(false, &buf2)

	multi := tracing.NewMultiExporter(exp1, exp2)
	tracer := tracing.NewTracer("fan-out-test", multi)

	span, _ := tracer.Start(context.Background(), "fan.out.op", tracing.SpanKindInternal)
	span.SetAttributes(tracing.Attribute{Key: "key1", Value: "val1"})
	span.SetStatus(tracing.SpanStatusOK, "")
	span.End()

	// Wait for async export.
	time.Sleep(100 * time.Millisecond)

	assert.NotEmpty(t, buf1.String(), "exporter 1 should receive the span")
	assert.NotEmpty(t, buf2.String(), "exporter 2 should receive the span")
}

// =============================================================================
// 17. PermissionModeResolver
// =============================================================================

func TestPermissionModeResolver(t *testing.T) {
	ctx := context.Background()
	resolver := approval.NewDefaultPermissionModeResolver()
	assert.Equal(t, "permission_mode", resolver.Name())

	t.Run("PermissionPlan resolves to PlanClassifier", func(t *testing.T) {
		cl := resolver.Resolve(approval.PermissionPlan)
		assert.Equal(t, "plan-classifier", cl.Name())
		assert.Equal(t, approval.Ask, cl.Classify(ctx, toolCall("bash")))
	})

	t.Run("PermissionAuto resolves to AutoClassifier", func(t *testing.T) {
		cl := resolver.Resolve(approval.PermissionAuto)
		assert.Equal(t, "auto-classifier", cl.Name())
	})

	t.Run("PermissionAutoFull resolves to AllowAll", func(t *testing.T) {
		cl := resolver.Resolve(approval.PermissionAutoFull)
		assert.Equal(t, "allow_all", cl.Name())
		assert.Equal(t, approval.Allow, cl.Classify(ctx, toolCall("bash")))
	})

	t.Run("PermissionDefault resolves to SafetyPolicy", func(t *testing.T) {
		cl := resolver.Resolve(approval.PermissionDefault)
		assert.Equal(t, "safety_policy", cl.Name())
	})
}

// =============================================================================
// 18. Circuit Breaker + Retry Policy Integration
// =============================================================================

func TestCircuitBreakerWithRetryPolicy(t *testing.T) {
	ctx := context.Background()

	clock := &fakeClock{t: time.Now()}
	cb := production.NewDefaultCircuitBreaker(production.CircuitBreakerConfig{
		FailureThreshold: 3,
		RecoveryTimeout:  30 * time.Second,
	}, production.WithClock(clock.Now))

	policy := production.NewDefaultRetryPolicy(production.RetryConfig{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Millisecond,
		Jitter:      0,
	})

	// Classify is a method on the concrete DefaultRetryPolicy type.
	dp, ok := policy.(*production.DefaultRetryPolicy)
	require.True(t, ok)

	transientErr := production.NewError(production.ErrorTransient, errors.New("connection reset"))

	// Simulate transient errors: circuit breaker should record failures.
	for i := 0; i < 3; i++ {
		classification := dp.Classify(transientErr)
		assert.Equal(t, production.ErrorTransient, classification, "error should be classified as transient")

		_, err := cb.Execute(ctx, func() (any, error) {
			return nil, transientErr
		})
		assert.Error(t, err)
	}

	// Verify retry policy allows retries for transient errors.
	assert.True(t, policy.ShouldRetry(ctx, transientErr, 0))
	assert.True(t, policy.ShouldRetry(ctx, transientErr, 1))
	assert.False(t, policy.ShouldRetry(ctx, transientErr, 3), "should not retry after max attempts")

	// After 3 failures, circuit breaker should be Open.
	assert.Equal(t, production.CircuitOpen, cb.State())

	// Further calls should be refused (no fallback).
	_, err := cb.Execute(ctx, func() (any, error) { return "ok", nil })
	assert.ErrorIs(t, err, production.ErrCircuitOpen)
}

// =============================================================================
// 19. Config Five-Layer Merge
// =============================================================================

func TestConfigFiveLayerMerge(t *testing.T) {
	ctx := context.Background()

	// Create a temp config file.
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	configJSON := `{"provider":{"model":"file-model","max_tokens":2048}}`
	require.NoError(t, writeFile(configPath, configJSON))

	// Set an env var for the provider model (env layer).
	t.Setenv("GO_CLI_PROVIDER_MODEL", "env-model")

	// Flag layer: override provider name.
	flagCfg := &config.Config{
		Provider: config.ProviderConfig{Name: "flag-provider"},
	}

	// Override layer: highest priority — override provider model.
	overrideCfg := &config.Config{
		Provider: config.ProviderConfig{Model: "override-model"},
	}

	loader := config.NewLoader().
		WithFile(configPath).
		WithFlag(flagCfg).
		WithOverride(overrideCfg)

	cfg, err := loader.Load(ctx)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Override wins over flag over env over file over defaults.
	assert.Equal(t, "override-model", cfg.Provider.Model, "override should win over all other layers")
	assert.Equal(t, "flag-provider", cfg.Provider.Name, "flag layer should win over env/file/default for name")
	assert.Equal(t, 2048, cfg.Provider.MaxTokens, "file layer should provide max_tokens when not overridden")
}

// =============================================================================
// Helpers
// =============================================================================

// writeFile writes data to a file at the given path.
func writeFile(path, data string) error {
	return os.WriteFile(path, []byte(data), 0o644)
}
