// Package tests contains end-to-end integration tests for go-cli.
//
// This file is the Phase 2 end-to-end gate. It proves, in one trace
// with a consistent trace_id rooted at a single span:
//
//  1. A 100+ turn long conversation runs stably against a real core.HarnessImpl
//     assembled from core.LoopAgent + an llm.ModelProvider (MockLLMServer) and a
//     real tools.ToolRegistry (DefaultToolRegistry).
//  2. Automatic compaction fires on context overflow via UnifiedCompactor +
//     MidTurnCompact and the result fits the token budget.
//  3. Session recovery: the same context is reconstructed after reopening a
//     JSONLSessionStore from disk as a brand-new store.
//  4. Full trace chain integrity: session.save / session.load / context.rebuild /
//     compaction / compaction.trigger / compaction.quality / tool.call(edit|grep)
//     spans all exist, share one trace_id and resolve to a root.
//  5. Quality metrics (Coverage / InfoLoss / CompressionRatio) are computed in
//     valid ranges after a compaction.
package tests

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// phase2Turns is the number of user submissions that must run stably to prove
// long conversation stability.
const phase2Turns = 120

// TestPhase2EndToEnd exercises the full Phase 2 feature set under a single
// tracing root so the trace chain can be asserted end to end.
func TestPhase2EndToEnd(t *testing.T) {
	exporter := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("e2e-phase2", exporter)
	root, spanCtx := tracer.Start(context.Background(), "phase2.root", tracing.SpanKindInternal)

	estimator := compaction.NewHeuristicTokenEstimator()
	const maxTokens = 64

	var (
		itemsIn   []compaction.TurnItem
		compacted []compaction.TurnItem
		strategy  = compaction.StrategyNone
	)

	t.Run("long_conversation_stable", func(t *testing.T) {
		phase1LongConversation(spanCtx, t)
	})

	t.Run("compaction_and_quality", func(t *testing.T) {
		itemsIn, compacted, strategy = phase2Compaction(spanCtx, t, estimator, maxTokens)
		phase5QualityMetrics(spanCtx, t, estimator, itemsIn, compacted, strategy)
	})

	t.Run("session_recovery", func(t *testing.T) {
		phase3SessionRecovery(spanCtx, t)
	})

	t.Run("tool_calls", func(t *testing.T) {
		phase4ToolCalls(spanCtx, t)
	})

	// End the root so it is exported, then verify the full trace chain.
	root.End()
	verifyTraceChain(t, exporter, tracer)
}

// phase1LongConversation drives 100+ user submissions through a real harness
// assembled from real core.LoopAgent, the MockLLMServer (an llm.ModelProvider
// / BaseChatModel) and a real tools.DefaultToolRegistry, draining every
// EventStream to completion.
func phase1LongConversation(spanCtx context.Context, t *testing.T) {
	t.Helper()

	gen := mock.NewLongConversationGenerator(phase2Turns, 5, 3)
	server := mock.NewMockLLMServer(gen.Generate())

	reg := tools.NewDefaultToolRegistry()
	for _, name := range []string{"read_file", "bash"} {
		require.NoError(t, reg.Register(spanCtx, cannedTool{name: name}), "register %s", name)
	}
	// Real edit/grep get wired in for the tool-call trace span.
	require.NoError(t, reg.Register(spanCtx, tools.NewEditFileTool()))
	require.NoError(t, reg.Register(spanCtx, tools.NewGrepTool(tools.WithForcePureGo(true))))

	loop := core.NewLoopAgent(
		core.WithLLM(server),
		core.WithTools(reg),
		core.WithMaxIterations(20),
	)
	agent := core.NewAgentImpl("phase2", loop)
	harness := core.NewHarnessImpl(agent, core.WithEventBuffer(16))

	var totalEvents int
	for i := 0; i < phase2Turns; i++ {
		stream, err := harness.Submit(spanCtx, fmt.Sprintf("continue the task in iteration %d", i))
		require.NoError(t, err, "submit iteration %d", i)
		for ev := range stream.Events() {
			totalEvents++
			_ = ev
		}
		_, err = stream.Result()
		require.NoError(t, err, "iteration %d result", i)
	}

	require.GreaterOrEqual(t, phase2Turns, 100, "test must cover 100+ turns")
	require.GreaterOrEqual(t, server.CallCount(), 100,
		"expected at least 100 LLM calls, got %d", server.CallCount())
	require.Greater(t, totalEvents, 0, "expected streamed agent events")
}

// phase2Compaction feeds an oversized conversation through MidTurnCompact with
// a UnifiedCompactor (Micro → Summary → Truncating) and asserts auto-compaction
// fires on overflow and the result fits maxTokens.
func phase2Compaction(spanCtx context.Context, t *testing.T, estimator compaction.TokenEstimator, maxTokens int) ([]compaction.TurnItem, []compaction.TurnItem, compaction.Strategy) {
	t.Helper()

	summarizer := &cannedSummarizer{summary: "older turns summarized into a short note"}
	unified := compaction.NewUnifiedCompactor(
		compaction.WithSummary(compaction.NewSummaryCompactor(summarizer)),
		compaction.WithTriggerReason("overflow"),
	)
	midturn := compaction.NewMidTurnCompact(compaction.WithThresholdRatio(0.5))

	items := oversizedItems(24)

	compacted, res, err := midturn.CompactIfNeeded(spanCtx, items, maxTokens, estimator, unified)
	require.NoError(t, err, "auto-compaction should not error")
	require.True(t, res.Triggered, "oversized context must trigger compaction")
	require.NotEqual(t, compaction.TriggerNone, res.Reason, "trigger reason should be set")
	require.NotEqual(t, compaction.StrategyNone, unified.LastStrategy(),
		"a concrete strategy (micro/summary/truncating) must be selected")

	require.LessOrEqual(t, countTokens(compacted, estimator), maxTokens,
		"compacted result must fit within maxTokens")
	require.LessOrEqual(t, len(compacted), len(items), "compaction must not grow the context")

	return items, compacted, unified.LastStrategy()
}

// phase5QualityMetrics evaluates compaction quality across Coverage, InfoLoss
// and CompressionRatio and asserts all land in valid ranges without panicking.
func phase5QualityMetrics(spanCtx context.Context, t *testing.T, estimator compaction.TokenEstimator, items, compressed []compaction.TurnItem, strategy compaction.Strategy) {
	t.Helper()

	evaluator := compaction.NewDefaultQualityEvaluator(estimator, compaction.WithQualityStrategy(strategy))
	qm, err := evaluator.Evaluate(spanCtx, items, compressed)
	require.NoError(t, err, "quality evaluation should not error")
	require.NotNil(t, qm)

	require.GreaterOrEqual(t, qm.Coverage, 0.0)
	require.LessOrEqual(t, qm.Coverage, 1.0)
	require.GreaterOrEqual(t, qm.InfoLoss, 0.0)
	require.LessOrEqual(t, qm.InfoLoss, 1.0)
	require.GreaterOrEqual(t, qm.CompressionRatio, 0.0)
	require.Equal(t, strategy, qm.Strategy)
}

// phase3SessionRecovery appends a series of SessionEntry records to a
// DefaultSessionTree while persisting the same records to a JSONLSessionStore,
// rebuilds the context via DefaultContextManager.BuildContext, then reopens the
// JSONL file as a brand-new store and asserts the reconstruction matches.
func phase3SessionRecovery(spanCtx context.Context, t *testing.T) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	store := session.NewJSONLSessionStore(path)
	require.NoError(t, store.Open(spanCtx))

	tree := session.NewDefaultSessionTree()
	var parent string
	for i := 0; i < 20; i++ {
		e := &session.SessionEntry{
			ID:        fmt.Sprintf("e-%02d", i),
			ParentID:  parent,
			Type:      session.EntryTypeUser,
			Content:   fmt.Sprintf("user message %d in the long session", i),
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
		}
		if i%5 == 4 {
			e.Type = session.EntryTypeCompaction
			e.Content = ""
			e.Summary = fmt.Sprintf("compacted summary covering up to turn %d", i)
		} else if i%2 == 0 {
			e.Type = session.EntryTypeAssistant
		}
		require.NoError(t, tree.Append(spanCtx, e), "tree append %d", i)
		require.NoError(t, store.Append(spanCtx, e), "store append %d", i)
		parent = e.ID
	}

	leaf := "e-19"
	mgr := session.NewDefaultContextManager(tree)
	sc, err := mgr.BuildContext(spanCtx, leaf)
	require.NoError(t, err)
	require.Greater(t, sc.EntryCount, 0, "rebuilt context must not be empty")

	// "New process": close the store and reopen it fresh from the same file.
	require.NoError(t, store.Close())
	fresh := session.NewJSONLSessionStore(path)
	require.NoError(t, fresh.Open(spanCtx))

	newTree := session.NewDefaultSessionTree()
	var parent2 string
	for i := 0; i < 20; i++ {
		e, gerr := fresh.Get(spanCtx, fmt.Sprintf("e-%02d", i))
		require.NoError(t, gerr, "reload entry %d", i)
		require.NoError(t, newTree.Append(spanCtx, e), "rebuilt tree append %d", i)
		parent2 = e.ID
	}
	_ = parent2

	mgr2 := session.NewDefaultContextManager(newTree)
	sc2, err2 := mgr2.BuildContext(spanCtx, leaf)
	require.NoError(t, err2)

	require.Equal(t, sc.EntryCount, sc2.EntryCount, "context entry count must survive reload")
	require.Equal(t, len(sc.Messages), len(sc2.Messages), "message list length must survive reload")
	require.Equal(t, sc.EstimatedTokens, sc2.EstimatedTokens, "token estimate must survive reload")
	for i := range sc.Messages {
		require.Equal(t, sc.Messages[i].Content, sc2.Messages[i].Content,
			"message %d content must survive reload", i)
	}
}

// phase4ToolCalls executes a real edit and a real grep tool under the shared
// tracing context so requirement 4's tool.call(edit|grep) spans are produced.
func phase4ToolCalls(spanCtx context.Context, t *testing.T) {
	t.Helper()

	target := filepath.Join(t.TempDir(), "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("hello world\nphase2 marker\n"), 0o600))

	// Route the calls through the real registry's Execute, which is what emits
	// (and ends) the "tool.call" span with a tool_name attribute.
	reg := tools.NewDefaultToolRegistry()
	require.NoError(t, reg.Register(spanCtx, tools.NewEditFileTool()))
	require.NoError(t, reg.Register(spanCtx, tools.NewGrepTool(tools.WithForcePureGo(true))))

	regExec, ok := reg.(*tools.DefaultToolRegistry)
	require.True(t, ok, "reg should be *DefaultToolRegistry")
	_, err := regExec.Execute(spanCtx, tools.ToolCall{
		Name: "edit",
		Args: map[string]any{
			"file_path":  target,
			"old_string": "world",
			"new_string": "go-cli",
		},
	})
	require.NoError(t, err, "edit tool should replace the block")

	_, err = regExec.Execute(spanCtx, tools.ToolCall{
		Name: "grep",
		Args: map[string]any{"pattern": "go-cli", "path": target},
	})
	require.NoError(t, err, "grep tool should match the edited content")
}

// verifyTraceChain asserts the required Phase 2 span names exist, share one
// trace_id and resolve to a single root. It runs after the root span has been
// ended so the trace is fully exported.
func verifyTraceChain(t *testing.T, exporter *mock.MockTraceExporter, tracer *tracing.Tracer) {
	t.Helper()

	// Give the asynchronously exported spans time to settle behind the root.
	waitForChain(t, exporter)

	for _, name := range []string{
		"session.save",
		"session.load",
		"context.rebuild",
		"compaction",
		"compaction.trigger",
		"compaction.quality",
		"tool.call",
	} {
		exporter.AssertSpanExists(t, name)
	}

	spans := exporter.Spans()
	require.NotEmpty(t, spans)

	for _, s := range spans {
		require.Equal(t, tracer.TraceID(), s.TraceID,
			"span %s (%s) must share the trace root id", s.SpanID, s.Name)
	}

	// Assert a tool.call span carries edit or grep as its tool_name.
	hasEditOrGrep := false
	for _, s := range spans {
		if s.Name != "tool.call" {
			continue
		}
		for _, a := range s.Attributes {
			if a.Key == "tool_name" {
				v := fmt.Sprint(a.Value)
				if v == "edit" || v == "grep" {
					hasEditOrGrep = true
				}
			}
		}
	}
	require.True(t, hasEditOrGrep, "expected a tool.call span for edit or grep")

	// Full parent chain must be traceable to a root.
	require.True(t, chainResolvesToRoot(spans), "span parent chain must resolve to a root")
}

// chainResolvesToRoot reports whether every non-root span's parent exists in
// the set and at least one root span (empty parent) is present.
func chainResolvesToRoot(spans []tracing.SpanData) bool {
	if len(spans) == 0 {
		return false
	}
	ids := make(map[string]bool, len(spans))
	for _, s := range spans {
		ids[s.SpanID] = true
	}
	hasRoot := false
	for _, s := range spans {
		if s.ParentSpanID == "" {
			hasRoot = true
			continue
		}
		if !ids[s.ParentSpanID] {
			return false
		}
	}
	return hasRoot
}

// waitForChain polls the exporter until every collected span's parent resolves
// to a root, bounding the wait so an export backlog cannot cause a flaky chain.
func waitForChain(t *testing.T, exporter *mock.MockTraceExporter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		spans := exporter.Spans()
		if len(spans) > 0 && chainResolvesToRoot(spans) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	exporter.AssertSpanExists(t, "session.save") // force a meaningful failure if nothing arrived
}

// oversizedItems builds a conversation whose estimated token count dwarfs a
// small budget so MidTurnCompact must trigger compaction.
func oversizedItems(n int) []compaction.TurnItem {
	items := make([]compaction.TurnItem, 0, n)
	for i := 0; i < n; i++ {
		role := compaction.RoleUser
		if i%2 == 0 {
			role = compaction.RoleAssistant
		}
		items = append(items, compaction.TurnItem{
			ID:      fmt.Sprintf("turn-%02d", i),
			Role:    role,
			Content: "verbose " + strings.Repeat("x", 40) + fmt.Sprintf(" content for turn %d", i),
		})
	}
	return items
}

// countTokens sums the estimated tokens across items' content and tool results.
func countTokens(items []compaction.TurnItem, est compaction.TokenEstimator) int {
	total := 0
	for _, it := range items {
		for _, text := range []string{it.Content, it.ToolResult} {
			if text == "" {
				continue
			}
			if v, err := est.Estimate(text); err == nil {
				total += v
			}
		}
	}
	return total
}

// cannedTool is a deterministic mock-free ToolDefinition used to satisfy the
// read_file/bash tool calls produced by the long-conversation generator.
type cannedTool struct{ name string }

var _ tools.ToolDefinition = cannedTool{}

func (c cannedTool) Name() string        { return c.name }
func (c cannedTool) Description() string { return "canned tool: " + c.name }
func (c cannedTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "ok"}, nil
}

// cannedSummarizer is a stub compaction.Summarizer returning a fixed summary.
type cannedSummarizer struct{ summary string }

var _ compaction.Summarizer = (*cannedSummarizer)(nil)

func (s *cannedSummarizer) Summarize(_ context.Context, _ string) (string, error) {
	return s.summary, nil
}
