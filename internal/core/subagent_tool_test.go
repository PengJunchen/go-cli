package core

import (
	"context"
	"errors"
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
