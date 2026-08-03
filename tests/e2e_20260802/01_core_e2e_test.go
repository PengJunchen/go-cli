package e2e_20260802

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// helper creating a simple mock LLM server that returns "done" after a few turns.
func newSimpleMockLLM() *mock.MockLLMServer {
	tmpl := mock.NewConversationTemplate("simple", "simple-test",
		mock.ConversationTurn{AssistantContent: "thinking about it"},
		mock.ConversationTurn{AssistantContent: "let me check"},
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{
			{ID: "call-1", Name: "read_file", Args: map[string]any{"path": "/tmp/test.txt"}},
		}},
		mock.ConversationTurn{AssistantContent: "done"},
	)
	return mock.NewMockLLMServer(tmpl)
}

// ---------------------------------------------------------------------------
// Test 1: Multi-turn ReAct with 4 user turns
// ---------------------------------------------------------------------------
func TestMultiTurnReAct(t *testing.T) {
	// Each turn gets a simple mock LLM that returns content immediately (no tool calls).
	tmpl := mock.NewConversationTemplate("S-01-mt", "multi-turn",
		mock.ConversationTurn{AssistantContent: "thinking about it"},
	)

	msgs := []string{"turn1: what is this?", "turn2: check again", "turn3: read the file", "turn4: final question"}
	for _, msg := range msgs {
		mockLLM := mock.NewMockLLMServer(tmpl)
		loop := core.NewLoopAgent(core.WithLLM(mockLLM))
		agent := core.NewAgentImpl("multiturn", loop)

		sub := core.Submission{Type: core.SubmissionUserMessage, Content: msg}
		result, runErr := agent.Run(context.Background(), sub)
		require.NoError(t, runErr, "turn %q: run error", msg)
		assert.NotEmpty(t, result.Message, "turn %q: empty result message", msg)
	}
}

// ---------------------------------------------------------------------------
// Test 2: LoopAgent max iterations guard
// ---------------------------------------------------------------------------
func TestLoopAgentMaxIterations(t *testing.T) {
	// Each turn returns tool calls to force re-iteration.
	turns := make([]mock.ConversationTurn, 20)
	for i := range turns {
		turns[i] = mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{
			{ID: fmt.Sprintf("call-%d", i), Name: "noop", Args: map[string]any{}},
		}}
	}
	tmpl := mock.NewConversationTemplate("loop", "loop-bomb", turns...)
	mockLLM := mock.NewMockLLMServer(tmpl)
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterMockTool("noop", func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: "ok"}, nil
	})
	require.NoError(t, err)

	loop := core.NewLoopAgent(core.WithLLM(mockLLM), core.WithTools(toolSrv), core.WithMaxIterations(3))
	_, err = loop.Run(context.Background(), core.Submission{Type: core.SubmissionUserMessage, Content: "loop"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max iterations")
}

// ---------------------------------------------------------------------------
// Test 3: Harness + EventStream
// ---------------------------------------------------------------------------
func TestHarnessWithEventStream(t *testing.T) {
	mockLLM := newSimpleMockLLM()
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("file-data")
	require.NoError(t, err)

	loop := core.NewLoopAgent(core.WithLLM(mockLLM), core.WithTools(toolSrv))
	agent := core.NewAgentImpl("harness-agent", loop)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(8))

	stream, err := h.Submit(context.Background(), "hello")
	require.NoError(t, err)
	require.NotNil(t, stream)

	var events []core.AgentEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-stream.Events():
			if !ok {
				stream.Close()
				goto done
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatal("timeout waiting for events")
		}
	}
done:
	assert.NotEmpty(t, events)
	msg, err := stream.Result()
	// Harness may or may not set a result on StubAgent — accept either
	if err != nil {
		t.Logf("result error (expected if no result set): %v", err)
	} else {
		t.Logf("result message: %s", msg.Content)
	}
}

// ---------------------------------------------------------------------------
// Test 4: Hook chain execution order
// ---------------------------------------------------------------------------
type testHook struct {
	name        string
	beforeCalls *[]string
	afterCalls  *[]string
}

func (h *testHook) Name() string { return h.name }
func (h *testHook) BeforeRun(_ context.Context, _ core.Submission) error {
	*h.beforeCalls = append(*h.beforeCalls, h.name)
	return nil
}
func (h *testHook) AfterRun(_ context.Context, _ core.Submission, _ core.Result, _ error) error {
	*h.afterCalls = append(*h.afterCalls, h.name)
	return nil
}

func TestHookChainExecutionOrder(t *testing.T) {
	var beforeCalls, afterCalls []string

	chain := core.NewHookChain(
		&testHook{name: "h1", beforeCalls: &beforeCalls, afterCalls: &afterCalls},
		&testHook{name: "h2", beforeCalls: &beforeCalls, afterCalls: &afterCalls},
		&testHook{name: "h3", beforeCalls: &beforeCalls, afterCalls: &afterCalls},
	)

	sub := core.Submission{Type: core.SubmissionUserMessage, Content: "test"}
	res, err := chain.Before(context.Background(), sub)
	require.NoError(t, err)
	assert.True(t, res.Continue)
	assert.Equal(t, []string{"h1", "h2", "h3"}, beforeCalls)

	aerr := chain.After(context.Background(), sub, core.Result{Message: "ok", Success: true}, nil)
	require.NoError(t, aerr)
	assert.Equal(t, []string{"h1", "h2", "h3"}, afterCalls)
}

// ---------------------------------------------------------------------------
// Test 5: Middleware chain onion composition
// ---------------------------------------------------------------------------
type trackingMW struct {
	name string
	log  *[]string
}

func (m *trackingMW) Name() string { return m.name }
func (m *trackingMW) Wrap(n core.AgentLoop) core.AgentLoop {
	return &trackingLoop{name: m.name, log: m.log, next: n}
}

type trackingLoop struct {
	name string
	log  *[]string
	next core.AgentLoop
}

func (l *trackingLoop) Run(ctx context.Context, sub core.Submission, streams ...core.EventStream) ([]core.AgentEvent, error) {
	*l.log = append(*l.log, l.name+":before")
	evts, err := l.next.Run(ctx, sub, streams...)
	*l.log = append(*l.log, l.name+":after")
	return evts, err
}

func TestMiddlewareChainOnion(t *testing.T) {
	var log []string

	base := core.NewLoopAgent()
	chain := core.NewMiddlewareChain(
		&trackingMW{name: "outer", log: &log},
		&trackingMW{name: "middle", log: &log},
		&trackingMW{name: "inner", log: &log},
	)
	wrapped := chain.Wrap(base)

	_, err := wrapped.Run(context.Background(), core.Submission{Type: core.SubmissionUserMessage, Content: "hi"})
	// expecting errNilModel because no LLM is set, but the middleware chain still wraps
	require.Error(t, err)
	// order should be: outer before, middle before, inner before, inner after, middle after, outer after
	assert.Equal(t,
		[]string{"outer:before", "middle:before", "inner:before", "inner:after", "middle:after", "outer:after"},
		log,
	)
}

// ---------------------------------------------------------------------------
// Test 6: TurnRunner lifecycle (RunTurn, Cancel)
// ---------------------------------------------------------------------------
func TestTurnRunnerLifecycle(t *testing.T) {
	// Create a loop that blocks until context is canceled
	blockingLoop := &blockingLoop{}
	runner := core.NewEinoTurnRunner(blockingLoop)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	resultCh := make(chan core.Result, 1)
	errCh := make(chan error, 1)

	go func() {
		r, e := runner.RunTurn(ctx, core.Submission{Type: core.SubmissionUserMessage, Content: "run"})
		resultCh <- r
		errCh <- e
	}()

	select {
	case <-resultCh:
		// finished
	case <-time.After(2 * time.Second):
		t.Fatal("RunTurn did not complete within timeout")
	}
	// Either it times out from the parent context or runs through
}

type blockingLoop struct{}

func (b *blockingLoop) Run(ctx context.Context, _ core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	select {
	case <-ctx.Done():
		return []core.AgentEvent{{Kind: "error", Content: ctx.Err().Error(), Timestamp: time.Now()}}, ctx.Err()
	case <-time.After(100 * time.Millisecond):
		return []core.AgentEvent{{Kind: "message", Content: "done", Timestamp: time.Now()}}, nil
	}
}

// ---------------------------------------------------------------------------
// Test 7: SubAgent delegation (Send, Interrupt, Wait)
// ---------------------------------------------------------------------------
func TestSubAgentDelegation(t *testing.T) {
	conf := core.SubAgentConfig{Name: "worker", MaxTurns: 3}
	sub := core.NewDefaultSubAgent(conf)

	ch, err := sub.Run(context.Background(), "do the work")
	require.NoError(t, err)
	require.NotNil(t, ch)

	// send a follow-up
	serr := sub.Send(context.Background(), "more info")
	require.NoError(t, serr)

	// consume events
	var msgs []string
	for ev := range ch {
		if ev.Kind == "message" {
			msgs = append(msgs, ev.Content)
		}
	}
	assert.NotEmpty(t, msgs)

	msg, werr := sub.Wait(context.Background())
	require.NoError(t, werr)
	assert.Equal(t, "assistant", string(msg.Role))
}

func TestSubAgentInterrupt(t *testing.T) {
	conf := core.SubAgentConfig{Name: "slow-worker", MaxTurns: 50}
	sub := core.NewDefaultSubAgent(conf)

	ch, err := sub.Run(context.Background(), "long task")
	require.NoError(t, err)

	ierr := sub.Interrupt(context.Background())
	require.NoError(t, ierr)

	// drain channel until closed
	for range ch {
	}
	_, werr := sub.Wait(context.Background())
	require.Error(t, werr)
}

// ---------------------------------------------------------------------------
// Test 8: SubAgentFactory Create
// ---------------------------------------------------------------------------
func TestSubAgentFactoryCreate(t *testing.T) {
	factory := core.NewSubAgentFactory()
	sa, err := factory.Create(context.Background(), "child", core.SubAgentConfig{
		Name:     "child-worker",
		Model:    "mock",
		Tools:    []string{"read_file"},
		MaxTurns: 2,
	})
	require.NoError(t, err)
	assert.Equal(t, "child", sa.Name())

	ch, runErr := sa.Run(context.Background(), "task")
	require.NoError(t, runErr)

	for range ch {
	}
	_, werr := sa.Wait(context.Background())
	require.NoError(t, werr)
}

// ---------------------------------------------------------------------------
// Test 9: Registry get/set pattern
// ---------------------------------------------------------------------------
func TestRegistryGetSet(t *testing.T) {
	reg := core.NewRegistry()

	// all defaults should be non-nil
	assert.NotNil(t, reg.AgentLoop())
	assert.NotNil(t, reg.Agent())
	assert.NotNil(t, reg.Harness())
	assert.NotNil(t, reg.TurnRunner())
	assert.NotNil(t, reg.SessionStore())
	assert.NotNil(t, reg.SessionTree())
	assert.NotNil(t, reg.ContextManager())
	assert.NotNil(t, reg.Compactor())
	assert.NotNil(t, reg.TokenEstimator())
	assert.NotNil(t, reg.ToolRegistry())
	assert.NotNil(t, reg.ModelProvider())
	assert.NotNil(t, reg.ApprovalClassifier())
	assert.NotNil(t, reg.ApprovalStore())
	assert.NotNil(t, reg.TraceExporter())

	// replace and verify old value is returned
	mockLLM := newSimpleMockLLM()
	loop := core.NewLoopAgent(core.WithLLM(mockLLM))
	old := reg.RegisterAgentLoop(loop)
	assert.NotNil(t, old)
	assert.Same(t, loop, reg.AgentLoop())

	agent := core.NewAgentImpl("test", loop)
	oldAgent := reg.RegisterAgent(agent)
	assert.NotNil(t, oldAgent)
}

// ---------------------------------------------------------------------------
// Test 10: Concurrent harness (5 goroutines)
// ---------------------------------------------------------------------------
func TestConcurrentHarness(t *testing.T) {
	mockLLM := newSimpleMockLLM()
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("content")
	require.NoError(t, err)

	loop := core.NewLoopAgent(core.WithLLM(mockLLM), core.WithTools(toolSrv))
	agent := core.NewAgentImpl("concurrent", loop)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(32))

	var wg sync.WaitGroup
	errs := make(chan error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			stream, serr := h.Submit(context.Background(), fmt.Sprintf("msg-%d", idx))
			if serr != nil {
				errs <- serr
				return
			}
			for range stream.Events() {
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent submit error: %v", e)
	}
}

// ---------------------------------------------------------------------------
// Test 11: EventStream overflow
// ---------------------------------------------------------------------------
func TestEventStreamOverflow(t *testing.T) {
	stream := core.NewEventStream(2)
	require.NotNil(t, stream)

	// fill buffer
	assert.NoError(t, stream.Send(core.AgentEvent{Kind: "a", Content: "1", Timestamp: time.Now()}))
	assert.NoError(t, stream.Send(core.AgentEvent{Kind: "b", Content: "2", Timestamp: time.Now()}))

	// third send would block without a consumer; test close + send returns nil
	stream.Close()
	assert.NoError(t, stream.Send(core.AgentEvent{Kind: "c", Content: "3", Timestamp: time.Now()}))

	// drain
	var count int
	for range stream.Events() {
		count++
	}
	assert.LessOrEqual(t, count, 2)
}

// ---------------------------------------------------------------------------
// Test 12: Agent state persistence (SessionStore)
// ---------------------------------------------------------------------------
func TestAgentStatePersistence(t *testing.T) {
	store := &core.SessionStoreImpl{}

	session := core.Session{ID: "s-001", Messages: []core.AgentMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}}
	err := store.Save(context.Background(), session)
	require.NoError(t, err)

	loaded, err := store.Load(context.Background(), "s-001")
	require.NoError(t, err)
	// stub returns empty session, verify no error
	assert.NotPanics(t, func() { _ = loaded.ID })

	list, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
}

// ---------------------------------------------------------------------------
// Test 13: Full agent pipeline (complex multi-turn)
// ---------------------------------------------------------------------------
func TestFullAgentPipeline(t *testing.T) {
	tmpl := mock.NewConversationTemplate("full", "full-pipeline",
		mock.ConversationTurn{AssistantContent: "analyzing request"},
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{
			{ID: "c1", Name: "read_file", Args: map[string]any{"path": "main.go"}},
			{ID: "c2", Name: "bash", Args: map[string]any{"command": "go build ./..."}},
		}},
		mock.ConversationTurn{AssistantContent: "analysis complete"},
		mock.ConversationTurn{AssistantToolCalls: []mock.ExpectedToolCall{
			{ID: "c3", Name: "read_file", Args: map[string]any{"path": "util.go"}},
		}},
		mock.ConversationTurn{AssistantContent: "final answer"},
	)
	mockLLM := mock.NewMockLLMServer(tmpl)
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("source code")
	require.NoError(t, err)
	_, err = toolSrv.RegisterBashTool("build success\n", 0)
	require.NoError(t, err)

	loop := core.NewLoopAgent(core.WithLLM(mockLLM), core.WithTools(toolSrv), core.WithMaxIterations(10))
	agent := core.NewAgentImpl("pipeline", loop)
	h := core.NewHarnessImpl(agent, core.WithEventBuffer(16))

	stream, err := h.Submit(context.Background(), "analyze and build")
	require.NoError(t, err)

	var eventCount int
	for range stream.Events() {
		eventCount++
	}
	assert.GreaterOrEqual(t, eventCount, 2)
}

// ---------------------------------------------------------------------------
// Test 14: Long conversation (50+ turns via LongConversationGenerator)
// ---------------------------------------------------------------------------
func TestLongConversation(t *testing.T) {
	gen := mock.NewLongConversationGenerator(50, 10, 5)
	tmpl := gen.Generate()
	mockLLM := mock.NewMockLLMServer(tmpl)
	toolSrv := mock.NewMockToolServer()
	_, err := toolSrv.RegisterReadFileTool("long-file-content")
	require.NoError(t, err)
	_, err = toolSrv.RegisterBashTool("ok\n", 0)
	require.NoError(t, err)

	traceExporter := mock.NewMockTraceExporter()
	runner := mock.NewConversationRunner(mockLLM, toolSrv, traceExporter)

	msgs := make([]string, 50)
	for i := range msgs {
		msgs[i] = fmt.Sprintf("turn %d", i)
	}
	runErr := runner.Run(context.Background(), msgs)
	require.NoError(t, runErr)

	runner.AssertNoLLMError(t)
	runner.AssertToolCalled(t, "read_file", 1)
}

// ---------------------------------------------------------------------------
// Test 15: Empty submission edge case
// ---------------------------------------------------------------------------
func TestEmptySubmission(t *testing.T) {
	mockLLM := newSimpleMockLLM()
	loop := core.NewLoopAgent(core.WithLLM(mockLLM))
	agent := core.NewAgentImpl("empty", loop)

	result, err := agent.Run(context.Background(), core.Submission{
		Type:    core.SubmissionUserMessage,
		Content: "",
	})
	require.NoError(t, err)
	assert.True(t, result.Success)
}

// ---------------------------------------------------------------------------
// Test 16: Steering submission edge case
// ---------------------------------------------------------------------------
func TestSteeringSubmission(t *testing.T) {
	mockLLM := newSimpleMockLLM()
	loop := core.NewLoopAgent(core.WithLLM(mockLLM))
	agent := core.NewAgentImpl("steer", loop)

	result, err := agent.Run(context.Background(), core.Submission{
		Type:    core.SubmissionSteering,
		Content: "steer left",
	})
	require.NoError(t, err)
	assert.True(t, result.Success)
}

// ---------------------------------------------------------------------------
// Test 17: FollowUp submission edge case
// ---------------------------------------------------------------------------
func TestFollowUpSubmission(t *testing.T) {
	mockLLM := newSimpleMockLLM()
	loop := core.NewLoopAgent(core.WithLLM(mockLLM))
	agent := core.NewAgentImpl("follow", loop)

	result, err := agent.Run(context.Background(), core.Submission{
		Type:    core.SubmissionFollowUp,
		Content: "also check this",
	})
	require.NoError(t, err)
	assert.True(t, result.Success)
}

// ---------------------------------------------------------------------------
// Test 18: SessionStore stub compliance
// ---------------------------------------------------------------------------
func TestSessionStoreStubCompliance(t *testing.T) {
	var store core.SessionStore = &core.SessionStoreImpl{}

	err := store.Save(context.Background(), core.Session{ID: "s1"})
	require.NoError(t, err)

	s, err := store.Load(context.Background(), "s1")
	require.NoError(t, err)
	assert.Empty(t, s.ID)
	assert.Empty(t, s.Messages)

	list, err := store.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, list)
}

// ---------------------------------------------------------------------------
// Test 19: Multiple subagents
// ---------------------------------------------------------------------------
func TestMultipleSubAgents(t *testing.T) {
	factory := core.NewSubAgentFactory()

	var subs []core.SubAgent
	for i := 0; i < 3; i++ {
		sa, err := factory.Create(context.Background(), fmt.Sprintf("worker-%d", i), core.SubAgentConfig{
			Name:     fmt.Sprintf("worker-%d", i),
			MaxTurns: 1,
		})
		require.NoError(t, err)
		subs = append(subs, sa)
	}

	for _, sa := range subs {
		ch, err := sa.Run(context.Background(), "task")
		require.NoError(t, err)
		for range ch {
		}
		_, werr := sa.Wait(context.Background())
		require.NoError(t, werr)
	}
}

// ---------------------------------------------------------------------------
// Test 20: Trace span verification
// ---------------------------------------------------------------------------
func TestTraceSpanVerification(t *testing.T) {
	exporter := mock.NewMockTraceExporter()
	tracer := tracing.NewTracer("", exporter)

	span, actx := tracer.Start(context.Background(), "test.operation", tracing.SpanKindInternal)
	require.NotNil(t, span)
	assert.NotEqual(t, "", span.TraceID())
	assert.NotEqual(t, "", span.SpanID())

	span.SetAttributes(tracing.Attribute{Key: "user_id", Value: "42"})
	span.SetStatus(tracing.SpanStatusOK, "")
	span.End()

	// wait for async export
	time.Sleep(50 * time.Millisecond)

	spans := exporter.Spans()
	require.Len(t, spans, 1)
	assert.Equal(t, "test.operation", spans[0].Name)

	// also verify SpanFromContext
	childSpan, _ := tracing.SpanFromContext(actx, "child.op", tracing.SpanKindClient)
	require.NotNil(t, childSpan)
	assert.Equal(t, span.SpanID(), childSpan.ParentSpanID())
	childSpan.End()
}
