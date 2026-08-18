package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// fakeSubAgent is a minimal SubAgent used to exercise the dispatcher without
// the full DefaultSubAgent harness.
type fakeSubAgent struct {
	name   string
	result AgentMessage
	err    error
	events []AgentEvent
	done   chan struct{}
	ranMu  sync.Mutex
	ran    bool
}

var _ SubAgent = (*fakeSubAgent)(nil)

func newFakeSubAgent(name string, result AgentMessage) *fakeSubAgent {
	return &fakeSubAgent{name: name, result: result, done: make(chan struct{})}
}

func (s *fakeSubAgent) Name() string { return s.name }

func (s *fakeSubAgent) Run(_ context.Context, prompt string) (<-chan AgentEvent, error) {
	s.ranMu.Lock()
	s.ran = true
	s.ranMu.Unlock()

	ch := make(chan AgentEvent, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	close(s.done)
	return ch, nil
}

func (s *fakeSubAgent) Send(_ context.Context, _ string) error { return nil }
func (s *fakeSubAgent) Interrupt(_ context.Context) error      { return nil }
func (s *fakeSubAgent) Wait(_ context.Context) (AgentMessage, error) {
	<-s.done
	return s.result, s.err
}

// fakeSubAgentFactory returns a programmed sub-agent on each Create and
// records the configs it was called with.
type fakeSubAgentFactory struct {
	mu      sync.Mutex
	subs    []SubAgent
	configs []SubAgentConfig
	idx     int
}

func (f *fakeSubAgentFactory) Create(_ context.Context, name string, config SubAgentConfig) (SubAgent, error) {
	if name != "" {
		config.Name = name
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.configs = append(f.configs, config)
	if f.idx < len(f.subs) {
		sub := f.subs[f.idx]
		f.idx++
		return sub, nil
	}
	return nil, errors.New("fakeSubAgentFactory: no sub programmed")
}

func TestDefaultSubagentDispatcherDispatchSuccess(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	sub.events = []AgentEvent{{Kind: "status", Content: "running", Timestamp: time.Now()}}
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	res, err := d.Dispatch(context.Background(), SubagentTask{
		ID:       "t1",
		Prompt:   "summarize",
		Tools:    []string{"bash"},
		MaxTurns: 3,
	})
	require.NoError(t, err)
	assert.Equal(t, "t1", res.TaskID)
	assert.Equal(t, "done", res.Content)
	assert.Greater(t, res.Duration, time.Duration(0))
	assert.NoError(t, res.Error)

	require.Len(t, factory.configs, 1)
	assert.Equal(t, "t1", factory.configs[0].Name)
	assert.Equal(t, []string{"bash"}, factory.configs[0].Tools)
	assert.Equal(t, 3, factory.configs[0].MaxTurns)

	sub.ranMu.Lock()
	assert.True(t, sub.ran)
	sub.ranMu.Unlock()
}

func TestDefaultSubagentDispatcherDispatchWaitError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	wantErr := errors.New("sub failed")
	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: ""})
	sub.err = wantErr
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	res, err := d.Dispatch(context.Background(), SubagentTask{ID: "t2", Prompt: "go"})
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, "t2", res.TaskID)
	assert.ErrorIs(t, res.Error, wantErr)
}

func TestDefaultSubagentDispatcherCreateError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	factory := &fakeSubAgentFactory{subs: nil} // no subs -> Create errors
	d := NewDefaultSubagentDispatcher(factory)

	res, err := d.Dispatch(context.Background(), SubagentTask{ID: "t3", Prompt: "go"})
	require.Error(t, err)
	assert.Equal(t, "t3", res.TaskID)
	assert.Error(t, res.Error)
	assert.Empty(t, d.ListRunning(), "failed dispatch must not leave a running task")
}

func TestDefaultSubagentDispatcherNilFactoryFallsBack(t *testing.T) {
	d := NewDefaultSubagentDispatcher(nil)
	assert.NotNil(t, d.factory, "nil factory should fall back to the default registry factory")
}

func TestDefaultSubagentDispatcherListRunning(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// A sub-agent whose Wait blocks until we close its done channel, so the
	// task stays running while we observe it.
	sub := &blockingFakeSubAgent{
		name:    "long",
		result:  AgentMessage{Role: "assistant", Content: "ok"},
		done:    make(chan struct{}),
		started: make(chan struct{}),
	}
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}
	d := NewDefaultSubagentDispatcher(factory)

	dispatchDone := make(chan struct{})
	go func() {
		_, _ = d.Dispatch(context.Background(), SubagentTask{ID: "running-1", Prompt: "go"}) //nolint:errcheck
		close(dispatchDone)
	}()

	<-sub.started
	running := d.ListRunning()
	require.Len(t, running, 1)
	assert.Equal(t, "running-1", running[0].ID)

	close(sub.done)
	select {
	case <-dispatchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not complete")
	}
	assert.Empty(t, d.ListRunning())
}

// blockingFakeSubAgent blocks Wait until done is closed, announcing start when
// Run is invoked.
type blockingFakeSubAgent struct {
	name    string
	result  AgentMessage
	done    chan struct{}
	started chan struct{}
}

var _ SubAgent = (*blockingFakeSubAgent)(nil)

func (s *blockingFakeSubAgent) Name() string { return s.name }
func (s *blockingFakeSubAgent) Run(_ context.Context, _ string) (<-chan AgentEvent, error) {
	close(s.started)
	ch := make(chan AgentEvent)
	go func() {
		<-s.done
		close(ch)
	}()
	return ch, nil
}
func (s *blockingFakeSubAgent) Send(_ context.Context, _ string) error { return nil }
func (s *blockingFakeSubAgent) Interrupt(_ context.Context) error      { return nil }
func (s *blockingFakeSubAgent) Wait(_ context.Context) (AgentMessage, error) {
	<-s.done
	return s.result, nil
}

// fakeDispatcher is a core.SubagentDispatcher stub for adapter tests.
type fakeDispatcher struct {
	mu         sync.Mutex
	task       SubagentTask
	res        SubagentResult
	err        error
	dispatched bool
}

func (f *fakeDispatcher) Dispatch(_ context.Context, task SubagentTask) (SubagentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.task = task
	f.dispatched = true
	return f.res, f.err
}

func (f *fakeDispatcher) ParallelDispatch(_ context.Context, tasks []SubagentTask) ([]SubagentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(tasks) > 0 {
		f.task = tasks[0]
		f.dispatched = true
	}
	results := make([]SubagentResult, len(tasks))
	for i, t := range tasks {
		results[i] = SubagentResult{TaskID: t.ID, Content: f.res.Content, Error: f.err}
	}
	return results, f.err
}
func (f *fakeDispatcher) ListRunning() []SubagentTask { return nil }

func TestAdaptSubagentDispatcherAndNewSubagentTool(t *testing.T) {
	fd := &fakeDispatcher{
		res: SubagentResult{TaskID: "t9", Content: "answer", Duration: 5 * time.Millisecond},
	}

	tool := NewSubagentTool(fd)
	assert.Equal(t, "dispatch_subagent", tool.Name())

	res, err := tool.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{
			"prompt":    "do something",
			"id":        "t9",
			"tools":     []any{"bash", "read"},
			"max_turns": float64(2),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "answer", res.Output)
	assert.Equal(t, "t9", res.Metadata["task_id"])

	fd.mu.Lock()
	assert.True(t, fd.dispatched)
	assert.Equal(t, "t9", fd.task.ID)
	assert.Equal(t, "do something", fd.task.Prompt)
	assert.Equal(t, []string{"bash", "read"}, fd.task.Tools)
	assert.Equal(t, 2, fd.task.MaxTurns)
	fd.mu.Unlock()
}

func TestSubagentToolGeneratedID(t *testing.T) {
	fd := &fakeDispatcher{res: SubagentResult{Content: "ok"}}
	tool := NewSubagentTool(fd)

	res, err := tool.Execute(context.Background(), tools.ToolCall{
		Args: map[string]any{"prompt": "hi"},
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", res.Output)

	fd.mu.Lock()
	assert.NotEmpty(t, fd.task.ID)
	fd.mu.Unlock()
}

// TestDispatchPassesSystemPromptToConfig proves an explicit SystemPrompt flows
// from SubagentTask through Dispatch into the SubAgentConfig.
func TestDispatchPassesSystemPromptToConfig(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	_, err := d.Dispatch(context.Background(), SubagentTask{
		ID:           "t1",
		Prompt:       "go",
		SystemPrompt: "custom sub-agent instructions",
	})
	require.NoError(t, err)

	require.Len(t, factory.configs, 1)
	assert.Equal(t, "custom sub-agent instructions", factory.configs[0].SystemPrompt)
}

// TestDispatchAppliesDefaultPromptWhenEmpty proves the default prompt is used
// when neither SystemPrompt nor Role is provided.
func TestDispatchAppliesDefaultPromptWhenEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	_, err := d.Dispatch(context.Background(), SubagentTask{
		ID:     "t2",
		Prompt: "go",
	})
	require.NoError(t, err)

	require.Len(t, factory.configs, 1)
	assert.Equal(t, DefaultSubAgentPrompt, factory.configs[0].SystemPrompt)
}

// TestDispatchAppliesRoleTemplate proves a recognized Role selects its template
// as the SystemPrompt when no explicit SystemPrompt is set.
func TestDispatchAppliesRoleTemplate(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	_, err := d.Dispatch(context.Background(), SubagentTask{
		ID:     "t3",
		Prompt: "go",
		Role:   "tester",
	})
	require.NoError(t, err)

	require.Len(t, factory.configs, 1)
	assert.Equal(t, TesterPrompt, factory.configs[0].SystemPrompt)
}

// TestDispatchSystemPromptPrecedenceOverRole proves an explicit SystemPrompt
// wins over a Role template.
func TestDispatchSystemPromptPrecedenceOverRole(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	_, err := d.Dispatch(context.Background(), SubagentTask{
		ID:           "t4",
		Prompt:       "go",
		Role:         "researcher",
		SystemPrompt: "explicit wins",
	})
	require.NoError(t, err)

	require.Len(t, factory.configs, 1)
	assert.Equal(t, "explicit wins", factory.configs[0].SystemPrompt)
}

// TestDispatchPassesModelToConfig proves the Model field flows from
// SubagentTask through Dispatch into the SubAgentConfig.
func TestDispatchPassesModelToConfig(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("worker", AgentMessage{Role: "assistant", Content: "done"})
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)
	_, err := d.Dispatch(context.Background(), SubagentTask{
		ID:     "t1",
		Prompt: "go",
		Model:  "gpt-4",
	})
	require.NoError(t, err)

	require.Len(t, factory.configs, 1)
	assert.Equal(t, "gpt-4", factory.configs[0].Model)
}

// TestDefaultSubagentDispatcherParallelDispatch proves ParallelDispatch runs
// all tasks concurrently and returns results in input order.
func TestDefaultSubagentDispatcherParallelDispatch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub1 := newFakeSubAgent("w1", AgentMessage{Role: "assistant", Content: "result-1"})
	sub2 := newFakeSubAgent("w2", AgentMessage{Role: "assistant", Content: "result-2"})
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub1, sub2}}

	d := NewDefaultSubagentDispatcher(factory)
	results, err := d.ParallelDispatch(context.Background(), []SubagentTask{
		{ID: "t1", Prompt: "task1"},
		{ID: "t2", Prompt: "task2"},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)
	assert.Equal(t, "t1", results[0].TaskID)
	assert.Equal(t, "result-1", results[0].Content)
	assert.Equal(t, "t2", results[1].TaskID)
	assert.Equal(t, "result-2", results[1].Content)
}

// TestDefaultSubagentDispatcherParallelDispatchEmpty verifies ParallelDispatch
// with no tasks returns an empty slice.
func TestDefaultSubagentDispatcherParallelDispatchEmpty(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	factory := &fakeSubAgentFactory{}
	d := NewDefaultSubagentDispatcher(factory)
	results, err := d.ParallelDispatch(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, results)
}

// TestAdapterParallelDispatch proves the adapter bridges ParallelDispatch and
// copies the Model field.
func TestAdapterParallelDispatch(t *testing.T) {
	fd := &fakeDispatcher{
		res: SubagentResult{TaskID: "t1", Content: "answer", Duration: 5 * time.Millisecond},
	}
	adapter := AdaptSubagentDispatcher(fd)

	results, err := adapter.ParallelDispatch(context.Background(), []tools.SubagentTask{
		{ID: "t1", Prompt: "do something", Model: "gpt-4"},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "t1", results[0].TaskID)
	assert.Equal(t, "answer", results[0].Content)

	fd.mu.Lock()
	assert.True(t, fd.dispatched)
	assert.Equal(t, "t1", fd.task.ID)
	assert.Equal(t, "gpt-4", fd.task.Model)
	fd.mu.Unlock()
}

// TestParallelDispatchWaitsAllForwardEvents verifies that ParallelDispatch
// does not return until all sub-agent events have been forwarded through
// onEvent. Without the wg.Add(1) tracking the forwardEvents goroutine, the
// dispatcher could return while events are still being drained.
func TestParallelDispatchWaitsAllForwardEvents(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newFakeSubAgent("w1", AgentMessage{Role: "assistant", Content: "done"})
	sub.events = []AgentEvent{
		{Kind: "status", Content: "e1", Timestamp: time.Now()},
		{Kind: "status", Content: "e2", Timestamp: time.Now()},
		{Kind: "status", Content: "e3", Timestamp: time.Now()},
	}
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}
	d := NewDefaultSubagentDispatcher(factory)

	var mu sync.Mutex
	var forwarded []string
	d.SetEventForwarder(func(_ string, ev AgentEvent) {
		time.Sleep(10 * time.Millisecond) // simulate slow event processing
		mu.Lock()
		forwarded = append(forwarded, ev.Content)
		mu.Unlock()
	})

	_, err := d.ParallelDispatch(context.Background(), []SubagentTask{
		{ID: "t1", Prompt: "go"},
	})
	require.NoError(t, err)

	// After ParallelDispatch returns, every event must have been forwarded.
	// Without the forwardEvents wg tracking, this would race and forwarded
	// would typically be empty or incomplete.
	mu.Lock()
	assert.Len(t, forwarded, 3)
	mu.Unlock()
}

// TestParallelDispatchAllEventsBeforeResults verifies that all events from all
// tasks are forwarded before ParallelDispatch returns its results.
func TestParallelDispatchAllEventsBeforeResults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub1 := newFakeSubAgent("w1", AgentMessage{Role: "assistant", Content: "r1"})
	sub1.events = []AgentEvent{
		{Kind: "status", Content: "1a", Timestamp: time.Now()},
		{Kind: "status", Content: "1b", Timestamp: time.Now()},
	}
	sub2 := newFakeSubAgent("w2", AgentMessage{Role: "assistant", Content: "r2"})
	sub2.events = []AgentEvent{
		{Kind: "status", Content: "2a", Timestamp: time.Now()},
		{Kind: "status", Content: "2b", Timestamp: time.Now()},
	}
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub1, sub2}}
	d := NewDefaultSubagentDispatcher(factory)

	var mu sync.Mutex
	var forwarded []string
	d.SetEventForwarder(func(_ string, ev AgentEvent) {
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		forwarded = append(forwarded, ev.Content)
		mu.Unlock()
	})

	results, err := d.ParallelDispatch(context.Background(), []SubagentTask{
		{ID: "t1", Prompt: "go"},
		{ID: "t2", Prompt: "go"},
	})
	require.NoError(t, err)
	require.Len(t, results, 2)

	// All 4 events must have been forwarded before results were returned.
	mu.Lock()
	assert.Len(t, forwarded, 4)
	mu.Unlock()

	// Results are in input order.
	assert.Equal(t, "r1", results[0].Content)
	assert.Equal(t, "r2", results[1].Content)
}

// TestParallelDispatchMultipleTasks verifies that ParallelDispatch with three
// concurrent tasks correctly forwards all events and returns all results.
func TestParallelDispatchMultipleTasks(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	subs := []SubAgent{}
	for i := 0; i < 3; i++ {
		s := newFakeSubAgent("w", AgentMessage{Role: "assistant", Content: "result"})
		s.events = []AgentEvent{
			{Kind: "status", Content: "running", Timestamp: time.Now()},
			{Kind: "status", Content: "done", Timestamp: time.Now()},
		}
		subs = append(subs, s)
	}
	factory := &fakeSubAgentFactory{subs: subs}
	d := NewDefaultSubagentDispatcher(factory)

	var mu sync.Mutex
	var count int
	d.SetEventForwarder(func(_ string, _ AgentEvent) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	tasks := []SubagentTask{
		{ID: "t1", Prompt: "go"},
		{ID: "t2", Prompt: "go"},
		{ID: "t3", Prompt: "go"},
	}
	results, err := d.ParallelDispatch(context.Background(), tasks)
	require.NoError(t, err)
	require.Len(t, results, 3)

	// 3 tasks × 2 events = 6 forwarded events.
	mu.Lock()
	assert.Equal(t, 6, count)
	mu.Unlock()

	for i, r := range results {
		assert.Equal(t, tasks[i].ID, r.TaskID)
		assert.Equal(t, "result", r.Content)
	}
}

// TestParallelDispatchRaceClean runs ParallelDispatch repeatedly with multiple
// tasks to surface data races when run under -race.
func TestParallelDispatchRaceClean(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	for i := 0; i < 10; i++ {
		subs := []SubAgent{}
		for j := 0; j < 3; j++ {
			s := newFakeSubAgent("w", AgentMessage{Role: "assistant", Content: "ok"})
			s.events = []AgentEvent{
				{Kind: "status", Content: "start", Timestamp: time.Now()},
				{Kind: "status", Content: "end", Timestamp: time.Now()},
			}
			subs = append(subs, s)
		}
		factory := &fakeSubAgentFactory{subs: subs}
		d := NewDefaultSubagentDispatcher(factory)

		var mu sync.Mutex
		var n int
		d.SetEventForwarder(func(_ string, _ AgentEvent) {
			mu.Lock()
			n++
			mu.Unlock()
		})

		tasks := []SubagentTask{
			{ID: "t1", Prompt: "go"},
			{ID: "t2", Prompt: "go"},
			{ID: "t3", Prompt: "go"},
		}
		results, err := d.ParallelDispatch(context.Background(), tasks)
		require.NoError(t, err)
		require.Len(t, results, 3)

		mu.Lock()
		assert.Equal(t, 6, n, "iteration %d: expected 6 forwarded events", i)
		mu.Unlock()
	}
}

// tracerMarkerKey is a context key used to simulate the presence of a tracer
// in the parent context. The subagent dispatcher should propagate the parent
// context (and therefore the tracer) to every sub-agent's Run call.
type tracerMarkerKey struct{}

// ctxCapturingSubAgent is a fake SubAgent that records the context passed to
// Run so tests can verify the parent context is propagated for trace
// continuity.
type ctxCapturingSubAgent struct {
	name   string
	result AgentMessage
	done   chan struct{}

	mu     sync.Mutex
	runCtx context.Context
	ran    bool
}

var _ SubAgent = (*ctxCapturingSubAgent)(nil)

func newCtxCapturingSubAgent(name string) *ctxCapturingSubAgent {
	return &ctxCapturingSubAgent{
		name:   name,
		result: AgentMessage{Role: "assistant", Content: "ok"},
		done:   make(chan struct{}),
	}
}

func (s *ctxCapturingSubAgent) Name() string { return s.name }

func (s *ctxCapturingSubAgent) Run(ctx context.Context, _ string) (<-chan AgentEvent, error) {
	s.mu.Lock()
	s.runCtx = ctx
	s.ran = true
	s.mu.Unlock()

	ch := make(chan AgentEvent)
	close(ch)
	close(s.done)
	return ch, nil
}

func (s *ctxCapturingSubAgent) Send(_ context.Context, _ string) error { return nil }
func (s *ctxCapturingSubAgent) Interrupt(_ context.Context) error      { return nil }
func (s *ctxCapturingSubAgent) Wait(_ context.Context) (AgentMessage, error) {
	<-s.done
	return s.result, nil
}

func (s *ctxCapturingSubAgent) capturedCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runCtx
}

// TestDispatchPassesParentCtxToSubagent verifies that Dispatch forwards the
// parent context (which carries the tracer) to the sub-agent's Run call so
// the sub-agent can create child spans linked to the parent span.
func TestDispatchPassesParentCtxToSubagent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newCtxCapturingSubAgent("tracer-worker")
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub}}

	d := NewDefaultSubagentDispatcher(factory)

	// Inject a marker value into the parent context to simulate a tracer.
	marker := "parent-trace-id"
	ctx := context.WithValue(context.Background(), tracerMarkerKey{}, marker)

	_, err := d.Dispatch(ctx, SubagentTask{ID: "t1", Prompt: "go"})
	require.NoError(t, err)

	captured := sub.capturedCtx()
	require.NotNil(t, captured, "sub-agent must receive a non-nil context")
	got, ok := captured.Value(tracerMarkerKey{}).(string)
	assert.True(t, ok, "parent context value must be propagated to sub-agent")
	assert.Equal(t, marker, got)
}

// TestParallelDispatchPassesParentCtxToSubagents verifies that ParallelDispatch
// forwards the parent context to every sub-agent's Run call.
func TestParallelDispatchPassesParentCtxToSubagents(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub1 := newCtxCapturingSubAgent("w1")
	sub2 := newCtxCapturingSubAgent("w2")
	factory := &fakeSubAgentFactory{subs: []SubAgent{sub1, sub2}}

	d := NewDefaultSubagentDispatcher(factory)

	marker := "parallel-trace-id"
	ctx := context.WithValue(context.Background(), tracerMarkerKey{}, marker)

	_, err := d.ParallelDispatch(ctx, []SubagentTask{
		{ID: "t1", Prompt: "go"},
		{ID: "t2", Prompt: "go"},
	})
	require.NoError(t, err)

	for _, sub := range []*ctxCapturingSubAgent{sub1, sub2} {
		captured := sub.capturedCtx()
		require.NotNil(t, captured)
		got, ok := captured.Value(tracerMarkerKey{}).(string)
		assert.True(t, ok, "parent context must be propagated to each sub-agent")
		assert.Equal(t, marker, got)
	}
}

// concurrencyTracker records the current and high-water-mark number of
// concurrently running sub-agents. It is used by concurrency-limit tests to
// assert the dispatcher never exceeds defaultMaxConcurrentSubagents.
type concurrencyTracker struct {
	mu      sync.Mutex
	current int
	max     int
}

func (c *concurrencyTracker) inc() {
	c.mu.Lock()
	c.current++
	if c.current > c.max {
		c.max = c.current
	}
	c.mu.Unlock()
}

func (c *concurrencyTracker) dec() {
	c.mu.Lock()
	c.current--
	c.mu.Unlock()
}

func (c *concurrencyTracker) maxSeen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.max
}

// trackingBlockingSubAgent is a SubAgent that blocks Run/Wait until its release
// channel is closed, while recording concurrency via a shared tracker. inc
// happens in Run and dec in Wait, so the tracker's high-water mark reflects the
// number of sub-agents that have started but not yet finished. When started is
// non-nil, Run announces start with a non-blocking send so tests can wait for a
// specific sub-agent to begin.
type trackingBlockingSubAgent struct {
	name    string
	result  AgentMessage
	tracker *concurrencyTracker
	release chan struct{}
	started chan struct{}
}

var _ SubAgent = (*trackingBlockingSubAgent)(nil)

func (s *trackingBlockingSubAgent) Name() string { return s.name }

func (s *trackingBlockingSubAgent) Run(_ context.Context, _ string) (<-chan AgentEvent, error) {
	s.tracker.inc()
	select {
	case s.started <- struct{}{}:
	default:
	}
	ch := make(chan AgentEvent)
	go func() {
		<-s.release
		close(ch)
	}()
	return ch, nil
}

func (s *trackingBlockingSubAgent) Send(_ context.Context, _ string) error { return nil }
func (s *trackingBlockingSubAgent) Interrupt(_ context.Context) error      { return nil }
func (s *trackingBlockingSubAgent) Wait(_ context.Context) (AgentMessage, error) {
	<-s.release
	s.tracker.dec()
	return s.result, nil
}

// errorSubAgent is a SubAgent whose Run returns runErr immediately, modeling a
// sub-agent that fails to start. It verifies the dispatcher releases the
// concurrency semaphore when a sub-agent errors out.
type errorSubAgent struct {
	name   string
	runErr error
}

var _ SubAgent = (*errorSubAgent)(nil)

func (s *errorSubAgent) Name() string { return s.name }
func (s *errorSubAgent) Run(_ context.Context, _ string) (<-chan AgentEvent, error) {
	return nil, s.runErr
}
func (s *errorSubAgent) Send(_ context.Context, _ string) error { return nil }
func (s *errorSubAgent) Interrupt(_ context.Context) error      { return nil }
func (s *errorSubAgent) Wait(_ context.Context) (AgentMessage, error) {
	return AgentMessage{}, s.runErr
}

// waitAll blocks until wg is done or the timeout elapses, failing the test on
// timeout. It keeps concurrency tests from hanging indefinitely when the
// semaphore is misbehaving.
func waitAll(t *testing.T, wg *sync.WaitGroup, timeout time.Duration, msg string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal(msg)
	}
}

// TestSubagentDispatcher_ConcurrencyLimit verifies that ParallelDispatch never
// runs more than defaultMaxConcurrentSubagents at once: with 10 tasks and a
// shared blocking release, exactly 5 start, the rest wait, and all 10 complete
// once released.
func TestSubagentDispatcher_ConcurrencyLimit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	const n = 10
	tracker := &concurrencyTracker{}
	release := make(chan struct{})
	started := make(chan struct{}, n)
	subs := make([]SubAgent, n)
	for i := 0; i < n; i++ {
		subs[i] = &trackingBlockingSubAgent{
			name:    fmt.Sprintf("w%d", i),
			result:  AgentMessage{Role: "assistant", Content: "done"},
			tracker: tracker,
			release: release,
			started: started,
		}
	}
	factory := &fakeSubAgentFactory{subs: subs}
	d := NewDefaultSubagentDispatcher(factory)

	tasks := make([]SubagentTask, n)
	for i := range tasks {
		tasks[i] = SubagentTask{ID: fmt.Sprintf("t%d", i), Prompt: "go"}
	}

	pdDone := make(chan struct{})
	go func() {
		_, _ = d.ParallelDispatch(context.Background(), tasks) //nolint:errcheck
		close(pdDone)
	}()

	// Wait for the semaphore cap sub-agents to start. The remaining ones must
	// be blocked waiting for a slot.
	for i := 0; i < defaultMaxConcurrentSubagents; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for sub-agent %d to start (max seen: %d)", i, tracker.maxSeen())
		}
	}
	// No 6th sub-agent can have started: the semaphore is full and nothing has
	// been released yet.
	assert.Equal(t, defaultMaxConcurrentSubagents, tracker.maxSeen(),
		"concurrent sub-agents must not exceed the semaphore cap")

	// Release all blockers so every queued task can proceed.
	close(release)

	select {
	case <-pdDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ParallelDispatch did not complete after release")
	}

	// The high-water mark must still be exactly the cap.
	assert.Equal(t, defaultMaxConcurrentSubagents, tracker.maxSeen(),
		"concurrent sub-agents must never exceed the semaphore cap")
}

// TestSubagentDispatcher_SemaphoreReleaseOnError verifies that a sub-agent
// which errors out releases its semaphore slot, allowing a subsequent task to
// proceed. Five blockers fill the semaphore via ParallelDispatch (which uses a
// local WaitGroup for event forwarding, so each blocker releases its semaphore
// slot independently when it finishes); a failing task waits, acquires a freed
// slot, errors, and releases; a final task then proceeds on that slot.
func TestSubagentDispatcher_SemaphoreReleaseOnError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tracker := &concurrencyTracker{}
	// 5 blockers, each with its own release/start channel so a single slot can
	// be freed deterministically.
	blockers := make([]*trackingBlockingSubAgent, defaultMaxConcurrentSubagents)
	for i := range blockers {
		blockers[i] = &trackingBlockingSubAgent{
			name:    fmt.Sprintf("block-%d", i),
			result:  AgentMessage{Role: "assistant", Content: "ok"},
			tracker: tracker,
			release: make(chan struct{}),
			started: make(chan struct{}, 1),
		}
	}
	failSub := &errorSubAgent{name: "fail", runErr: errors.New("boom")}
	okSub := newFakeSubAgent("ok", AgentMessage{Role: "assistant", Content: "done"})

	// Factory order: 5 blockers (created by ParallelDispatch), then failSub and
	// okSub (created by subsequent Dispatch calls).
	subs := make([]SubAgent, 0, len(blockers)+2)
	for _, b := range blockers {
		subs = append(subs, b)
	}
	subs = append(subs, failSub, okSub)
	factory := &fakeSubAgentFactory{subs: subs}
	d := NewDefaultSubagentDispatcher(factory)

	ctx := context.Background()

	// Fill the semaphore via ParallelDispatch. ParallelDispatch creates all
	// sub-agents sequentially before launching goroutines, so the factory
	// assignment is deterministic: task i -> blockers[i]. Each goroutine uses
	// a local WaitGroup for event forwarding, so releasing a single blocker
	// frees its semaphore slot without waiting for the others.
	blockerTasks := make([]SubagentTask, len(blockers))
	for i := range blockers {
		blockerTasks[i] = SubagentTask{ID: fmt.Sprintf("b%d", i), Prompt: "block"}
	}
	pdDone := make(chan struct{})
	go func() {
		_, _ = d.ParallelDispatch(ctx, blockerTasks) //nolint:errcheck
		close(pdDone)
	}()

	// Wait for all 5 blockers to start, confirming the semaphore is full.
	for i, b := range blockers {
		select {
		case <-b.started:
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for blocker %d to start", i)
		}
	}

	// Dispatch the failing task: it must block waiting for a slot.
	failDone := make(chan struct{})
	var failErr error
	go func() {
		_, failErr = d.Dispatch(ctx, SubagentTask{ID: "fail", Prompt: "fail"})
		close(failDone)
	}()

	// Free exactly one slot by releasing blocker 0. The failing task should
	// acquire it, error out, and release the slot again.
	close(blockers[0].release)

	select {
	case <-failDone:
	case <-time.After(3 * time.Second):
		t.Fatal("failing dispatch did not complete after a slot was freed")
	}
	require.Error(t, failErr, "failing dispatch must report the sub-agent error")

	// A subsequent task must proceed: the 4 remaining blockers still hold
	// slots, so the only free slot is the one the failing task released. If the
	// semaphore were not released, this dispatch would hang.
	okDone := make(chan struct{})
	go func() {
		_, _ = d.Dispatch(ctx, SubagentTask{ID: "ok", Prompt: "ok"}) //nolint:errcheck
		close(okDone)
	}()
	select {
	case <-okDone:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatch after a failing task did not proceed; semaphore was not released")
	}

	// Tear down the remaining blockers and wait for ParallelDispatch to finish.
	for _, b := range blockers[1:] {
		close(b.release)
	}
	select {
	case <-pdDone:
	case <-time.After(3 * time.Second):
		t.Fatal("ParallelDispatch did not complete during teardown")
	}
}

// TestSubagentDispatcher_ConcurrencyRace verifies that Dispatch and
// ParallelDispatch share the same semaphore: mixing both entry points
// saturates the cap without exceeding it and runs cleanly under -race.
func TestSubagentDispatcher_ConcurrencyRace(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	const (
		dispatchCount = 4
		parallelCount = 8
		total         = dispatchCount + parallelCount
	)
	tracker := &concurrencyTracker{}
	release := make(chan struct{})
	subs := make([]SubAgent, total)
	for i := 0; i < total; i++ {
		subs[i] = &trackingBlockingSubAgent{
			name:    fmt.Sprintf("w%d", i),
			result:  AgentMessage{Role: "assistant", Content: "done"},
			tracker: tracker,
			release: release,
		}
	}
	factory := &fakeSubAgentFactory{subs: subs}
	d := NewDefaultSubagentDispatcher(factory)

	ctx := context.Background()
	var wg sync.WaitGroup

	// Launch individual Dispatch calls.
	for i := 0; i < dispatchCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			_, _ = d.Dispatch(ctx, SubagentTask{ID: fmt.Sprintf("d%d", id), Prompt: "go"}) //nolint:errcheck
		}(i)
	}
	// Launch one ParallelDispatch with the remaining tasks.
	parallelTasks := make([]SubagentTask, parallelCount)
	for i := range parallelTasks {
		parallelTasks[i] = SubagentTask{ID: fmt.Sprintf("p%d", i), Prompt: "go"}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = d.ParallelDispatch(ctx, parallelTasks) //nolint:errcheck
	}()

	// Wait until concurrency reaches the cap, proving Dispatch and
	// ParallelDispatch share the same semaphore.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tracker.maxSeen() >= defaultMaxConcurrentSubagents {
			break
		}
		time.Sleep(time.Millisecond)
	}
	assert.Equal(t, defaultMaxConcurrentSubagents, tracker.maxSeen(),
		"mixed Dispatch+ParallelDispatch must saturate the shared semaphore")

	// Release all blockers and wait for everything to finish.
	close(release)
	waitAll(t, &wg, 5*time.Second, "mixed dispatch did not complete; possible semaphore deadlock")

	// The cap must never have been exceeded.
	assert.Equal(t, defaultMaxConcurrentSubagents, tracker.maxSeen(),
		"concurrent sub-agents must never exceed the semaphore cap")
}

// TestSubagentDispatcher_ContextCancelReleasesSemaphore verifies that a
// Dispatch waiting on a full semaphore returns promptly when its context is
// canceled (instead of hanging), and does not leak a slot. Five blockers fill
// the semaphore; a sixth dispatch with a cancellable context is canceled while
// waiting and must return context.Canceled without consuming a slot.
func TestSubagentDispatcher_ContextCancelReleasesSemaphore(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tracker := &concurrencyTracker{}
	blockers := make([]*trackingBlockingSubAgent, defaultMaxConcurrentSubagents)
	for i := range blockers {
		blockers[i] = &trackingBlockingSubAgent{
			name:    fmt.Sprintf("block-%d", i),
			result:  AgentMessage{Role: "assistant", Content: "ok"},
			tracker: tracker,
			release: make(chan struct{}),
			started: make(chan struct{}, 1),
		}
	}
	// The canceled task uses a blocking sub-agent so that, were the semaphore
	// acquire NOT cancellation-aware, the dispatch would hang forever.
	canceledSub := &trackingBlockingSubAgent{
		name:    "canceled",
		result:  AgentMessage{Role: "assistant", Content: "never"},
		tracker: tracker,
		release: make(chan struct{}),
	}
	okSub := newFakeSubAgent("ok", AgentMessage{Role: "assistant", Content: "done"})

	subs := make([]SubAgent, 0, len(blockers)+2)
	for _, b := range blockers {
		subs = append(subs, b)
	}
	subs = append(subs, canceledSub, okSub)
	factory := &fakeSubAgentFactory{subs: subs}
	d := NewDefaultSubagentDispatcher(factory)

	ctx := context.Background()

	// Fill the semaphore with 5 blockers.
	var blockerWG sync.WaitGroup
	for i, b := range blockers {
		blockerWG.Add(1)
		go func(id int) {
			defer blockerWG.Done()
			_, _ = d.Dispatch(ctx, SubagentTask{ID: fmt.Sprintf("b%d", id), Prompt: "block"}) //nolint:errcheck
		}(i)
		select {
		case <-b.started:
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout waiting for blocker %d to start", i)
		}
	}

	// Dispatch a task with a cancellable context. It must block on the full
	// semaphore.
	cctx, cancel := context.WithCancel(ctx)
	cancelDone := make(chan struct{})
	var cancelErr error
	go func() {
		_, cancelErr = d.Dispatch(cctx, SubagentTask{ID: "canceled", Prompt: "go"})
		close(cancelDone)
	}()

	// Give the canceled dispatch a moment to begin waiting on the semaphore,
	// then cancel the context.
	time.Sleep(50 * time.Millisecond)
	cancel()

	// The dispatch must return promptly (it must not hang on the semaphore).
	select {
	case <-cancelDone:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled dispatch did not return after context cancel; semaphore acquire is not cancellation-aware")
	}
	require.ErrorIs(t, cancelErr, context.Canceled,
		"canceled dispatch must report context.Canceled")

	// The canceled task must not have started running (it never acquired a
	// slot), so the high-water mark must still be exactly the cap.
	assert.Equal(t, defaultMaxConcurrentSubagents, tracker.maxSeen(),
		"canceled task must not have consumed a semaphore slot")

	// Release the blockers and wait for their dispatches to finish.
	for _, b := range blockers {
		close(b.release)
	}
	waitAll(t, &blockerWG, 3*time.Second, "blocker dispatches did not complete during teardown")

	// A fresh dispatch must complete, proving the canceled waiter did not
	// permanently block the semaphore.
	res, err := d.Dispatch(ctx, SubagentTask{ID: "ok", Prompt: "ok"})
	require.NoError(t, err)
	assert.Equal(t, "done", res.Content)

	// The canceled sub-agent's Run was never called (it never acquired a slot),
	// so no goroutine waits on its release channel. Close it for safety.
	close(canceledSub.release)
}
