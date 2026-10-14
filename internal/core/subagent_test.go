package core

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// blockingSubAgentRunner blocks until its context is canceled, simulating a
// long-running sub-task that is interrupted externally. It returns the context
// error so the sub-agent reaches a terminal (interrupted/failed) state.
type blockingSubAgentRunner struct {
	canceled atomic.Bool
}

var _ subAgentRunner = (*blockingSubAgentRunner)(nil)

func (b *blockingSubAgentRunner) Run(ctx context.Context, prompt string, inbox <-chan string, emit func(AgentEvent)) (AgentMessage, error) {
	emit(AgentEvent{Kind: "user", Content: prompt, Timestamp: time.Now()})
	<-ctx.Done()
	b.canceled.Store(true)
	emit(errEvent(ctx.Err()))
	return AgentMessage{}, ctx.Err()
}

// blockingFactory returns a runner that blocks until canceled.
func blockingFactory(cfg SubAgentConfig) subAgentRunner {
	return &blockingSubAgentRunner{}
}

// drainChan consumes all events from a channel until it closes.
func drainChan(ch <-chan AgentEvent) []AgentEvent {
	var evs []AgentEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func newTestSubAgent(name string) *DefaultSubAgent {
	sub := NewDefaultSubAgent(SubAgentConfig{
		Name:     name,
		Model:    "mock",
		MaxTurns: 2,
	})
	return mustSubAgent(sub)
}

func mustSubAgent(sub SubAgent) *DefaultSubAgent {
	impl, ok := sub.(*DefaultSubAgent)
	if !ok {
		panic("NewDefaultSubAgent should return *DefaultSubAgent")
	}
	return impl
}

func TestSubAgentRunExecutesIndependentRun(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newTestSubAgent("worker")
	assert.Equal(t, SubAgentIdle, sub.State())

	ch, err := sub.Run(context.Background(), "summarize this")
	require.NoError(t, err)

	events := drainChan(ch)
	require.NotEmpty(t, events)
	assert.Equal(t, "user", events[0].Kind)
	assert.Equal(t, "summarize this", events[0].Content)

	res, err := sub.Wait(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "assistant", res.Role)
	assert.NotEmpty(t, res.Content)

	// a fresh independent run executes and completes.
	assert.Equal(t, SubAgentCompleted, sub.State())
}

func TestSubAgentSendRecordsMessage(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseRunner := func() { once.Do(func() { close(release) }) }
	runner := &heldInboxSubAgentRunner{started: started, release: release}
	sub := mustSubAgent(NewDefaultSubAgent(SubAgentConfig{Name: "worker"},
		WithSubAgentRunner(func(SubAgentConfig) subAgentRunner { return runner })))
	_, err := sub.Run(ctx, "go")
	require.NoError(t, err)
	defer func() {
		releaseRunner()
		_, _ = sub.Wait(ctx)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("runner did not reach the live barrier")
	}
	require.NoError(t, sub.Send(ctx, "follow-up"))
	assert.Equal(t, []string{"follow-up"}, sub.Received())
	releaseRunner()
	_, werr := sub.Wait(ctx)
	require.NoError(t, werr)
	assert.Equal(t, SubAgentCompleted, sub.State())
}

type heldInboxSubAgentRunner struct {
	started chan struct{}
	release <-chan struct{}
}

func (r *heldInboxSubAgentRunner) Run(ctx context.Context, _ string, _ <-chan string, _ func(AgentEvent)) (AgentMessage, error) {
	close(r.started)
	select {
	case <-r.release:
		return AgentMessage{Role: "assistant", Content: "done"}, nil
	case <-ctx.Done():
		return AgentMessage{}, ctx.Err()
	}
}

func TestSubagentSendReportsDroppedMessage(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRunner := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseRunner()
	runner := &heldInboxSubAgentRunner{started: started, release: release}
	sub := mustSubAgent(NewDefaultSubAgent(
		SubAgentConfig{Name: "held-inbox"},
		WithSubAgentRunner(func(SubAgentConfig) subAgentRunner { return runner }),
	))

	_, err := sub.Run(context.Background(), "hold inbox")
	require.NoError(t, err)
	defer func() {
		releaseRunner()
		_, _ = sub.Wait(context.Background())
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("sub-agent runner did not start")
	}

	for i := range inboxBufferSize {
		require.NoError(t, sub.Send(context.Background(), "queued-"+string(rune('a'+i))))
	}

	overflowErr := sub.Send(context.Background(), "overflow")
	require.Error(t, overflowErr, "a full inbox must reject the message instead of reporting false success")
	assert.Len(t, sub.Received(), inboxBufferSize,
		"Received must contain only messages actually accepted into the inbox")

	releaseRunner()
	_, err = sub.Wait(context.Background())
	require.NoError(t, err)
}

type panickingSubAgentRunner struct {
	sideEffects *atomic.Int32
}

func (r *panickingSubAgentRunner) Run(context.Context, string, <-chan string, func(AgentEvent)) (AgentMessage, error) {
	r.sideEffects.Add(1)
	panic("runner fixture panic")
}

func TestSubAgentPanicIsContainedAndFinalized(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tests := []struct {
		name    string
		factory SubAgentRunnerFactory
	}{
		{
			name: "runner factory",
			factory: func(SubAgentConfig) subAgentRunner {
				panic("factory fixture panic")
			},
		},
		{
			name: "runner",
			factory: func(SubAgentConfig) subAgentRunner {
				return &panickingSubAgentRunner{}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sideEffects := &atomic.Int32{}
			factory := tt.factory
			if tt.name == "runner" {
				factory = func(SubAgentConfig) subAgentRunner {
					return &panickingSubAgentRunner{sideEffects: sideEffects}
				}
			}
			sub := mustSubAgent(NewDefaultSubAgent(
				SubAgentConfig{Name: "panic-" + tt.name},
				WithSubAgentRunner(factory),
			))
			events, err := sub.Run(context.Background(), "trigger panic")
			require.NoError(t, err)

			waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			result, waitErr := sub.Wait(waitCtx)

			require.ErrorIs(t, waitErr, ErrSubAgentPanicked, "panic must not be reported as successful completion")
			var unknownOutcome interface{ OutcomeUnknown() bool }
			require.ErrorAs(t, waitErr, &unknownOutcome)
			assert.True(t, unknownOutcome.OutcomeUnknown(), "panic may follow an unobserved side effect")
			if tt.name == "runner" {
				assert.EqualValues(t, 1, sideEffects.Load(), "the runner may cause an effect before panicking")
			}
			assert.Empty(t, result)
			assert.Equal(t, SubAgentFailed, sub.State())
			assert.ErrorIs(t, sub.Send(context.Background(), "too late"), ErrSubAgentNotRunning)

			var sawErrorEvent bool
			for event := range events {
				if event.Kind == "error" {
					sawErrorEvent = true
					assert.Contains(t, event.Content, "panic")
				}
			}
			assert.True(t, sawErrorEvent, "panic failure should be recorded on the closed event stream")
		})
	}
}

func TestSubAgentInterruptStopsRun(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := mustSubAgent(NewDefaultSubAgent(SubAgentConfig{Name: "blocker"}, WithSubAgentRunner(blockingFactory)))
	ch, err := sub.Run(context.Background(), "long task")
	require.NoError(t, err)

	// Give the blocking runner a moment to start, then interrupt.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, sub.Interrupt(context.Background()))

	msg, err := sub.Wait(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Empty(t, msg.Content)

	// the sub-agent reached the Interrupted state.
	assert.Equal(t, SubAgentInterrupted, sub.State())
	drainChan(ch)
}

func TestSubAgentWaitReturnsResult(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sub := newTestSubAgent("worker")
	_, err := sub.Run(context.Background(), "prompt")
	require.NoError(t, err)

	// Wait blocks until the sub-run finishes and returns the result.
	begin := time.Now()
	res, err := sub.Wait(context.Background())
	require.NoError(t, err)
	assert.Equal(t, SubAgentCompleted, sub.State())
	assert.NotEmpty(t, res.Content)
	assert.GreaterOrEqual(t, time.Since(begin), time.Duration(0))
}

func TestSubAgentStateTransitions(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releaseRunner := func() { once.Do(func() { close(release) }) }
	runner := &heldInboxSubAgentRunner{started: started, release: release}

	// Idle -> Running -> Completed. Hold the runner until the Running
	// assertion has observed the live state, independently of scheduling.
	sub := mustSubAgent(NewDefaultSubAgent(SubAgentConfig{Name: "worker"},
		WithSubAgentRunner(func(SubAgentConfig) subAgentRunner { return runner })))
	assert.Equal(t, SubAgentIdle, sub.State())
	_, err := sub.Run(ctx, "p")
	require.NoError(t, err)
	defer func() {
		releaseRunner()
		_, _ = sub.Wait(ctx)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("runner did not reach the live barrier")
	}
	assert.Equal(t, SubAgentRunning, sub.State())
	releaseRunner()
	_, werr := sub.Wait(ctx)
	require.NoError(t, werr)
	assert.Equal(t, SubAgentCompleted, sub.State())

	// A second Run on a completed sub-agent is rejected.
	_, err = sub.Run(ctx, "again")
	require.Error(t, err)

	// Idle -> Running -> Failed when the runner errors (blocking ctx cancel
	// without an explicit Interrupt yields a failed terminal state).
	blocker := mustSubAgent(NewDefaultSubAgent(SubAgentConfig{Name: "b"}, WithSubAgentRunner(blockingFactory)))
	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	_, err = blocker.Run(rctx, "p")
	require.NoError(t, err)
	rcancel()
	_, err = blocker.Wait(ctx)
	require.Error(t, err)
	assert.Equal(t, SubAgentFailed, blocker.State())
}

func TestSubAgentParentCancellationCascades(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// the sub-agent uses an independent context; canceling the
	// parent context cancels the child context (observed via a terminal error).
	sub := mustSubAgent(NewDefaultSubAgent(SubAgentConfig{Name: "b"}, WithSubAgentRunner(blockingFactory)))
	parent, cancel := context.WithCancel(context.Background())
	_, err := sub.Run(parent, "p")
	require.NoError(t, err)

	time.Sleep(10 * time.Millisecond)
	cancel() // canceling the parent must cascade to the running sub-task

	_, err = sub.Wait(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
	assert.Equal(t, SubAgentFailed, sub.State())
}

// captureExporter is a local in-memory tracing exporter used to capture spans
// emitted by the sub-agent without importing the mock package (avoiding any
// test-only import coupling).
type captureExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

var _ tracing.TraceExporter = (*captureExporter)(nil)

func (e *captureExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans = append(e.spans, tracing.SpanToData(span))
	return nil
}

func (e *captureExporter) Shutdown(_ context.Context) error { return nil }

func (e *captureExporter) all() []tracing.SpanData {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]tracing.SpanData(nil), e.spans...)
}

// waitForPanSpan polls the exporter until a span with the given name appears,
// or fails the test after a timeout. Spans are exported asynchronously.
func waitForSpan(t *testing.T, exp *captureExporter, name string) tracing.SpanData {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, span := range exp.all() {
			if span.Name == name {
				return span
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("span with name %q not exported; got %d spans", name, len(exp.all()))
	return tracing.SpanData{}
}

func attrValue(attrs []tracing.Attribute, key string) (any, bool) {
	var val any
	var found bool
	for _, a := range attrs {
		if a.Key == key {
			val = a.Value
			found = true
		}
	}
	return val, found
}

func subAgentWithTrace(t *testing.T) (*captureExporter, context.Context) {
	t.Helper()
	exp := &captureExporter{}
	tracer := tracing.NewTracer("test-trace", exp)
	root, spanCtx := tracer.Start(context.Background(), "subagent.test", tracing.SpanKindInternal)
	root.SetAttributes(tracing.Attribute{Key: "root", Value: "true"})
	t.Cleanup(root.End)
	return exp, spanCtx
}

func TestSubAgentSpawnSpanAttrs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exp, spanCtx := subAgentWithTrace(t)
	sub := newTestSubAgent("trace-worker")
	_, err := sub.Run(spanCtx, "hello-world-prompt")
	require.NoError(t, err)
	_, werr := sub.Wait(spanCtx)
	require.NoError(t, werr)

	// subagent.spawn carries subagent_name and prompt_length.
	spawn := waitForSpan(t, exp, "subagent.spawn")
	name, ok := attrValue(spawn.Attributes, "subagent_name")
	require.True(t, ok)
	assert.Equal(t, "trace-worker", name)
	lenVal, ok := attrValue(spawn.Attributes, "prompt_length")
	require.True(t, ok)
	assert.Equal(t, len("hello-world-prompt"), lenVal)
}

func TestSubAgentRunSpanAttrs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exp, spanCtx := subAgentWithTrace(t)
	sub := newTestSubAgent("trace-worker")
	_, err := sub.Run(spanCtx, "prompt")
	require.NoError(t, err)
	_, werr := sub.Wait(spanCtx)
	require.NoError(t, werr)

	// subagent.run carries subagent_name and status.
	run := waitForSpan(t, exp, "subagent.run")
	name, ok := attrValue(run.Attributes, "subagent_name")
	require.True(t, ok)
	assert.Equal(t, "trace-worker", name)
	status, ok := attrValue(run.Attributes, "status")
	require.True(t, ok)
	assert.Equal(t, "completed", status)
}

func TestSubAgentTraceChainConsistent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	exp, spanCtx := subAgentWithTrace(t)
	sub := newTestSubAgent("trace-worker")
	_, err := sub.Run(spanCtx, "prompt")
	require.NoError(t, err)
	_, werr := sub.Wait(spanCtx)
	require.NoError(t, werr)

	spawn := waitForSpan(t, exp, "subagent.spawn")
	run := waitForSpan(t, exp, "subagent.run")

	// trace_id is consistent across the trace.
	assert.Equal(t, spawn.TraceID, run.TraceID)
	assert.Equal(t, "test-trace", spawn.TraceID)

	// the run span is a child of the spawn span.
	assert.Equal(t, spawn.SpanID, run.ParentSpanID)
}

func TestSubAgentFactoryCreate(t *testing.T) {
	factory := NewSubAgentFactory()
	sub, err := factory.Create(context.Background(), "task-1", SubAgentConfig{Model: "mock"})
	require.NoError(t, err)
	assert.Equal(t, "task-1", sub.Name())

	// SubAgentFactory is registered and lazily defaulted.
	RegisterSubAgentFactory(subFactoryStub{})
	f := GetSubAgentFactory()
	require.NotNil(t, f)
	RegisterSubAgentFactory(nil)
	f2 := GetSubAgentFactory()
	require.NotNil(t, f2)
}

// TestSubAgentFactoryForwardsRunnerFactory proves the pluggable runner seam:
// a factory built with WithSubAgentRunnerFactory must hand the custom runner
// to every created sub-agent. The blocking runner emits the prompt then blocks,
// behavior the default simulated runner would not exhibit.
func TestSubAgentFactoryForwardsRunnerFactory(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	factory := NewSubAgentFactory(WithSubAgentRunnerFactory(blockingFactory))
	sub, err := factory.Create(context.Background(), "custom-runner", SubAgentConfig{Model: "mock", MaxTurns: 2})
	require.NoError(t, err)
	assert.Equal(t, "custom-runner", sub.Name())

	ch, err := sub.Run(context.Background(), "block me")
	require.NoError(t, err)

	ev := <-ch
	assert.Equal(t, "user", ev.Kind)
	assert.Equal(t, "block me", ev.Content)

	require.NoError(t, sub.Interrupt(context.Background()))
	_, werr := sub.Wait(context.Background())
	require.Error(t, werr)
	drainChan(ch)
}

// subFactoryStub is a minimal SubAgentFactory used to exercise the registry.
type subFactoryStub struct{}

var _ SubAgentFactory = (*subFactoryStub)(nil)

func (subFactoryStub) Create(_ context.Context, _ string, _ SubAgentConfig) (SubAgent, error) {
	return newTestSubAgent("stub"), nil
}
