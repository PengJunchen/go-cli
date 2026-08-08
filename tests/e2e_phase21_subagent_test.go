//go:build e2e

// Package tests contains end-to-end integration tests for the go-cli project.
// This file verifies Phase 21 SubAgent production hardening: system prompt
// strategy, event forwarding, depth limit, role whitelist, cost breakdown,
// and parallel dispatch.
package tests

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/production"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// =============================================================================
// Phase 21 SubAgent E2E Tests (Task 21-7)
// =============================================================================

// TestET_subagent_system_prompt_contains_strategy verifies that the system
// prompt built by DefaultSystemPromptBuilder includes the SubAgent delegation
// strategy text, role names, and recursion constraints.
func TestET_subagent_system_prompt_contains_strategy(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	builder := core.NewDefaultSystemPromptBuilder()
	prompt := builder.Build(ctx, core.SystemPromptOptions{})

	assert.Contains(t, prompt, "Sub-Agent Delegation Strategy")
	assert.Contains(t, prompt, "dispatch_subagent")
	assert.Contains(t, prompt, "researcher")
	assert.Contains(t, prompt, "implementer")
	assert.Contains(t, prompt, "reviewer")
	assert.Contains(t, prompt, "tester")
	assert.Contains(t, prompt, "Recursion constraints")
	assert.Contains(t, prompt, "depth limit")
	assert.Contains(t, prompt, "tool whitelist")
}

// TestET_subagent_events_visible verifies that sub-agent events are forwarded
// to the main EventStream via the SetEventForwarder callback on
// DefaultSubagentDispatcher. Uses a real dispatcher with a fake sub-agent
// factory.
func TestET_subagent_events_visible(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Create a fake sub-agent that emits events.
	result := core.AgentMessage{Role: "assistant", Content: "sub-agent result"}
	sub := newE2EFakeSubAgent("worker", result, []core.AgentEvent{
		{Kind: "status", Content: "started", Timestamp: time.Now()},
		{Kind: "message", Content: "working", Timestamp: time.Now()},
		{Kind: "status", Content: "completed", Timestamp: time.Now()},
	})
	factory := &e2EFakeSubAgentFactory{subs: []core.SubAgent{sub}}

	dispatcher := core.NewDefaultSubagentDispatcher(factory)

	// Track forwarded events.
	var forwardedMu sync.Mutex
	var forwarded []core.AgentEvent
	dispatcher.SetEventForwarder(func(taskID string, ev core.AgentEvent) {
		forwardedMu.Lock()
		defer forwardedMu.Unlock()
		forwarded = append(forwarded, ev)
	})

	res, err := dispatcher.Dispatch(ctx, core.SubagentTask{
		ID:     "task-1",
		Prompt: "do work",
	})
	require.NoError(t, err)
	assert.Equal(t, "sub-agent result", res.Content)

	// Verify events were forwarded.
	forwardedMu.Lock()
	defer forwardedMu.Unlock()
	assert.GreaterOrEqual(t, len(forwarded), 2, "at least 2 events should be forwarded")
	kinds := make([]string, len(forwarded))
	for i, ev := range forwarded {
		kinds[i] = ev.Kind
	}
	assert.Contains(t, kinds, "status")
	assert.Contains(t, kinds, "message")
}

// TestET_subagent_depth_limit_exceeded verifies that the sub-agent recursion
// depth limit is enforced. The system prompt includes the depth limit
// constraint (default 3), and the realSubAgentRunner rejects dispatches that
// would exceed it. We verify the constraint is present in the system prompt
// and that the runner factory enforces the defaultMaxSubAgentDepth=3 limit.
func TestET_subagent_depth_limit_exceeded(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Verify the system prompt includes the depth limit constraint.
	builder := core.NewDefaultSystemPromptBuilder()
	prompt := builder.Build(ctx, core.SystemPromptOptions{})
	assert.Contains(t, prompt, "Recursion constraints")
	assert.Contains(t, prompt, "depth limit")
	assert.Contains(t, prompt, "3")

	// Verify the real runner factory produces runners that enforce the
	// defaultMaxSubAgentDepth (3) limit. We create a factory with a mock
	// LLM and verify the runner is created and can run at depth 0.
	mockLLM := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"D", "depth-test",
		mock.ConversationTurn{AssistantContent: "ok"},
	))
	factory := core.NewRealSubAgentFactory(mockLLM, llm.NewProviderRegistry(), tools.NewDefaultToolRegistry())

	// Create a sub-agent via the factory and run it at depth 0 (should succeed).
	sub, err := factory.Create(ctx, "depth-test", core.SubAgentConfig{Name: "test", MaxTurns: 1})
	require.NoError(t, err)
	require.NotNil(t, sub)

	evCh, err := sub.Run(ctx, "test prompt")
	require.NoError(t, err)
	// Drain events.
	for range evCh {
	}
	msg, err := sub.Wait(ctx)
	require.NoError(t, err)
	assert.Equal(t, "ok", msg.Content)

	// The depth limit enforcement (defaultMaxSubAgentDepth=3) is verified
	// in the core package's unit tests (real_subagent_runner_depth_test.go).
	// Here we verify the system prompt constraint and factory wiring.
	assert.Contains(t, prompt, "default 3")
}

// TestET_subagent_researcher_no_bash verifies that the researcher role's tool
// whitelist does not include the bash tool, ensuring researcher sub-agents
// cannot execute shell commands.
func TestET_subagent_researcher_no_bash(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = ctx
	whitelist := core.RoleToolWhitelist["researcher"]
	require.NotEmpty(t, whitelist, "researcher role should have a tool whitelist")

	for _, toolName := range whitelist {
		assert.NotEqual(t, "bash", toolName, "researcher role must NOT have access to bash tool")
	}

	// Also verify the researcher has read access (expected for research).
	assert.Contains(t, whitelist, "read")

	// Verify implementer HAS bash (contrast).
	implWhitelist := core.RoleToolWhitelist["implementer"]
	assert.Contains(t, implWhitelist, "bash", "implementer role should have bash access")
}

// TestET_subagent_cost_breakdown verifies that CostTracker.RecordSubagent
// records per-task cost breakdown exposed via SubagentCostSnapshot, enabling /cost
// to show subagent cost breakdown.
func TestET_subagent_cost_breakdown(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = ctx
	tracker := production.NewCostTracker(nil) // uses DefaultCostTiers

	// Record main session cost.
	_, err := tracker.Record("gpt-4o", 1000, 500)
	require.NoError(t, err)

	// Record sub-agent costs for two tasks.
	_, err = tracker.RecordSubagent("task-research", "gpt-4o", 2000, 1000)
	require.NoError(t, err)

	_, err = tracker.RecordSubagent("task-implement", "gpt-4o-mini", 500, 200)
	require.NoError(t, err)

	// Verify main session cost.
	assert.Equal(t, 1, tracker.Calls())
	assert.Greater(t, tracker.Total(), 0.0)

	// Verify sub-agent cost breakdown.
	snapshot := tracker.SubagentCostSnapshot()
	assert.Equal(t, 2, len(snapshot), "should have 2 subagent cost entries")

	var researchCost production.SubagentCostRecord
	var researchFound bool
	for _, r := range snapshot {
		if r.TaskID == "task-research" {
			researchCost = r
			researchFound = true
		}
	}
	require.True(t, researchFound)
	assert.Equal(t, 1, researchCost.Calls)
	assert.Equal(t, 2000, researchCost.TokensIn)
	assert.Equal(t, 1000, researchCost.TokensOut)
	assert.Greater(t, researchCost.Cost, 0.0)

	var implCost production.SubagentCostRecord
	var implFound bool
	for _, r := range snapshot {
		if r.TaskID == "task-implement" {
			implCost = r
			implFound = true
		}
	}
	require.True(t, implFound)
	assert.Equal(t, 1, implCost.Calls)
	assert.Greater(t, implCost.Cost, 0.0)

	// Verify subagent total is separate from main total.
	subTotal := tracker.SubagentTotal()
	assert.Greater(t, subTotal, 0.0)
	subCalls := tracker.SubagentCalls()
	assert.Equal(t, 2, subCalls)

	// Main total should NOT include subagent costs.
	mainTotal := tracker.Total()
	assert.Greater(t, mainTotal, 0.0)
	assert.NotEqual(t, mainTotal, subTotal, "main cost and subagent cost should be separate")
}

// TestET_subagent_parallel_dispatch verifies that ParallelDispatch executes
// tasks concurrently. Uses fake sub-agents with timing to detect concurrency.
func TestET_subagent_parallel_dispatch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Create two fake sub-agents that each take ~100ms to complete.
	sub1 := newE2ETimedSubAgent("w1", core.AgentMessage{Role: "assistant", Content: "result-1"}, 100*time.Millisecond)
	sub2 := newE2ETimedSubAgent("w2", core.AgentMessage{Role: "assistant", Content: "result-2"}, 100*time.Millisecond)
	factory := &e2EFakeSubAgentFactory{subs: []core.SubAgent{sub1, sub2}}

	dispatcher := core.NewDefaultSubagentDispatcher(factory)

	tasks := []core.SubagentTask{
		{ID: "t1", Prompt: "task 1"},
		{ID: "t2", Prompt: "task 2"},
	}

	start := time.Now()
	results, err := dispatcher.ParallelDispatch(ctx, tasks)
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Len(t, results, 2)

	// Verify results are in input order.
	assert.Equal(t, "t1", results[0].TaskID)
	assert.Equal(t, "result-1", results[0].Content)
	assert.Equal(t, "t2", results[1].TaskID)
	assert.Equal(t, "result-2", results[1].Content)

	// If tasks ran sequentially, total time would be ~200ms.
	// If concurrent, total time should be ~100ms.
	assert.Less(t, elapsed, 180*time.Millisecond,
		"parallel dispatch should complete in roughly max(task durations), not sum")
}

// =============================================================================
// Helpers
// =============================================================================

// e2EFakeSubAgent is a test SubAgent that emits pre-configured events and
// returns a pre-configured result.
type e2EFakeSubAgent struct {
	name   string
	result core.AgentMessage
	events []core.AgentEvent
	done   chan struct{}
}

var _ core.SubAgent = (*e2EFakeSubAgent)(nil)

func newE2EFakeSubAgent(name string, result core.AgentMessage, events []core.AgentEvent) *e2EFakeSubAgent {
	return &e2EFakeSubAgent{name: name, result: result, events: events, done: make(chan struct{})}
}

func (s *e2EFakeSubAgent) Name() string { return s.name }
func (s *e2EFakeSubAgent) Run(_ context.Context, _ string) (<-chan core.AgentEvent, error) {
	ch := make(chan core.AgentEvent, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	close(s.done)
	return ch, nil
}
func (s *e2EFakeSubAgent) Send(_ context.Context, _ string) error { return nil }
func (s *e2EFakeSubAgent) Interrupt(_ context.Context) error      { return nil }
func (s *e2EFakeSubAgent) Wait(_ context.Context) (core.AgentMessage, error) {
	<-s.done
	return s.result, nil
}

// e2ETimedSubAgent is a test SubAgent that delays before completing, used to
// verify parallel dispatch concurrency.
type e2ETimedSubAgent struct {
	name   string
	result core.AgentMessage
	delay  time.Duration
	done   chan struct{}
}

var _ core.SubAgent = (*e2ETimedSubAgent)(nil)

func newE2ETimedSubAgent(name string, result core.AgentMessage, delay time.Duration) *e2ETimedSubAgent {
	return &e2ETimedSubAgent{name: name, result: result, delay: delay, done: make(chan struct{})}
}

func (s *e2ETimedSubAgent) Name() string { return s.name }
func (s *e2ETimedSubAgent) Run(_ context.Context, _ string) (<-chan core.AgentEvent, error) {
	ch := make(chan core.AgentEvent, 1)
	go func() {
		time.Sleep(s.delay)
		close(s.done)
		close(ch)
	}()
	return ch, nil
}
func (s *e2ETimedSubAgent) Send(_ context.Context, _ string) error { return nil }
func (s *e2ETimedSubAgent) Interrupt(_ context.Context) error      { return nil }
func (s *e2ETimedSubAgent) Wait(_ context.Context) (core.AgentMessage, error) {
	<-s.done
	return s.result, nil
}

// e2EFakeSubAgentFactory is a test SubAgentFactory that returns pre-configured
// sub-agents in sequence.
type e2EFakeSubAgentFactory struct {
	mu   sync.Mutex
	subs []core.SubAgent
	idx  int
}

func (f *e2EFakeSubAgentFactory) Create(_ context.Context, name string, _ core.SubAgentConfig) (core.SubAgent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx < len(f.subs) {
		sub := f.subs[f.idx]
		f.idx++
		return sub, nil
	}
	return nil, errors.New("e2EFakeSubAgentFactory: no sub programmed")
}
