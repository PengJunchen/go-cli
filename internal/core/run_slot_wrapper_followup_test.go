package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type pendingMessagesAgent struct {
	calls         atomic.Int32
	runCalls      atomic.Int32
	active        atomic.Int32
	overlapped    atomic.Bool
	firstStarted  chan struct{}
	secondStarted chan struct{}
	firstRelease  chan struct{}
	secondRelease chan struct{}
}

func (*pendingMessagesAgent) Name() string { return "pending-messages" }

func (a *pendingMessagesAgent) Run(ctx context.Context, _ Submission, _ ...EventStream) (Result, error) {
	a.runCalls.Add(1)
	<-ctx.Done()
	return Result{}, ctx.Err()
}

func (a *pendingMessagesAgent) Messages() []AgentMessage {
	call := a.calls.Add(1)
	if a.active.Add(1) > 1 {
		a.overlapped.Store(true)
	}
	defer a.active.Add(-1)
	if call == 1 {
		close(a.firstStarted)
		<-a.firstRelease
		return []AgentMessage{{Role: "assistant", Content: "before"}}
	}
	close(a.secondStarted)
	<-a.secondRelease
	return nil
}

func TestTurnRunnerDoesNotReadMessagesWhileCallbackPending(t *testing.T) {
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	agent := &pendingMessagesAgent{
		firstStarted: make(chan struct{}), secondStarted: make(chan struct{}),
		firstRelease: make(chan struct{}), secondRelease: make(chan struct{}),
	}
	var releaseFirst, releaseSecond sync.Once
	defer releaseFirst.Do(func() { close(agent.firstRelease) })
	defer releaseSecond.Do(func() { close(agent.secondRelease) })
	guard := NewDefaultRunSlotGuard()
	runner := NewEinoTurnRunner(wrapperBoundaryLoop{})
	runner.SetAgent(agent)
	runner.SetRunSlotGuard(guard)
	runner.SetRunLifetimeContext(lifetime)
	runner.SetHookChain(NewHookChain(&blockingTurnLifecycleHook{}))
	runner.cleanupTimeout = time.Millisecond
	type runOutcome struct {
		result Result
		err    error
	}
	runDone := make(chan runOutcome, 1)
	go func() {
		result, err := runner.RunTurn(context.Background(), Submission{Content: "hello"})
		runDone <- runOutcome{result: result, err: err}
	}()
	select {
	case <-agent.firstStarted:
	case <-time.After(time.Second):
		t.Fatal("initial Messages snapshot did not start")
	}
	cancelLifetime()
	select {
	case outcome := <-runDone:
		require.ErrorIs(t, outcome.err, errRunCleanupPending)
		require.Equal(t, RunExecutionCleanupPending, outcome.result.ExecutionState)
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not return its bounded pending snapshot")
	}
	select {
	case <-agent.secondStarted:
		t.Fatal("post-run Messages snapshot started before the callback joined")
	case <-time.After(20 * time.Millisecond):
	}
	require.EqualValues(t, 1, agent.calls.Load())
	require.False(t, agent.overlapped.Load())

	releaseFirst.Do(func() { close(agent.firstRelease) })
	select {
	case <-agent.secondStarted:
	case <-time.After(time.Second):
		t.Fatal("post-run Messages snapshot did not start after callback completion")
	}
	require.False(t, agent.overlapped.Load())
	releaseSecond.Do(func() { close(agent.secondRelease) })
	require.Zero(t, agent.runCalls.Load(), "callback cancellation during the initial snapshot must not start the agent")
	select {
	case <-guard.CloseAdmission():
	case <-time.After(time.Second):
		t.Fatal("turn wrapper did not finish after final snapshot returned")
	}
}

type pendingTurnStartHook struct {
	started   chan struct{}
	release   chan struct{}
	ended     chan struct{}
	active    atomic.Int32
	endEarly  atomic.Bool
	endCtxErr chan error
}

type countingMessageAgent struct {
	messages atomic.Int32
	runs     atomic.Int32
}

func (*countingMessageAgent) Name() string { return "counting-message-agent" }

func (a *countingMessageAgent) Messages() []AgentMessage {
	a.messages.Add(1)
	return nil
}

func (a *countingMessageAgent) Run(ctx context.Context, _ Submission, _ ...EventStream) (Result, error) {
	a.runs.Add(1)
	return Result{}, ctx.Err()
}

func (*pendingTurnStartHook) Name() string { return "pending-turn-start" }

func (*pendingTurnStartHook) BeforeRun(context.Context, Submission) error { return nil }

func (*pendingTurnStartHook) AfterRun(context.Context, Submission, Result, error) error {
	return nil
}

func (*pendingTurnStartHook) OnCompaction(context.Context, int, int) error { return nil }

func (*pendingTurnStartHook) OnError(context.Context, string, error) error { return nil }

func (h *pendingTurnStartHook) OnTurnStart(context.Context, string) error {
	h.active.Add(1)
	close(h.started)
	<-h.release // deliberately ignores cancellation
	h.active.Add(-1)
	return nil
}

func (h *pendingTurnStartHook) OnTurnEnd(ctx context.Context, _ string, _ Result, _ error) error {
	if h.active.Load() != 0 {
		h.endEarly.Store(true)
	}
	h.endCtxErr <- ctx.Err()
	close(h.ended)
	return nil
}

func TestTurnRunnerPostHooksWaitForPendingTurnStart(t *testing.T) {
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()
	hook := &pendingTurnStartHook{
		started: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{}),
		endCtxErr: make(chan error, 1),
	}
	var releaseStart sync.Once
	defer releaseStart.Do(func() { close(hook.release) })
	loop := &countingBoundaryLoop{}
	agent := &countingMessageAgent{}
	guard := NewDefaultRunSlotGuard()
	runner := NewEinoTurnRunner(loop)
	runner.SetAgent(agent)
	runner.SetRunSlotGuard(guard)
	runner.SetRunLifetimeContext(lifetime)
	runner.SetHookChain(NewHookChain(hook))
	runner.cleanupTimeout = time.Millisecond
	type runOutcome struct {
		result Result
		err    error
	}
	runDone := make(chan runOutcome, 1)
	go func() {
		result, err := runner.RunTurn(context.Background(), Submission{Content: "hello"})
		runDone <- runOutcome{result: result, err: err}
	}()
	select {
	case <-hook.started:
	case <-time.After(time.Second):
		t.Fatal("OnTurnStart did not start")
	}
	cancelLifetime()
	select {
	case outcome := <-runDone:
		require.ErrorIs(t, outcome.err, errRunCleanupPending)
		require.Equal(t, RunExecutionCleanupPending, outcome.result.ExecutionState)
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not return its bounded pending snapshot")
	}
	select {
	case <-hook.ended:
		t.Fatal("OnTurnEnd ran while OnTurnStart was still pending")
	case <-time.After(20 * time.Millisecond):
	}
	require.Zero(t, loop.calls.Load(), "canceled OnTurnStart must not continue into the loop")

	releaseStart.Do(func() { close(hook.release) })
	select {
	case <-hook.ended:
	case <-time.After(time.Second):
		t.Fatal("OnTurnEnd did not run after OnTurnStart returned")
	}
	require.False(t, hook.endEarly.Load())
	require.ErrorIs(t, <-hook.endCtxErr, context.Canceled, "lifetime cancellation remains visible to late hooks")
	require.Zero(t, agent.messages.Load(), "canceled OnTurnStart must not begin an initial history snapshot")
	require.Zero(t, agent.runs.Load(), "canceled OnTurnStart must not begin agent execution")
	require.Zero(t, loop.calls.Load(), "canceled OnTurnStart must not continue into the loop")
	select {
	case <-guard.CloseAdmission():
	case <-time.After(time.Second):
		t.Fatal("turn wrapper did not complete after final hooks")
	}
}

type pendingResourceTurnHook struct {
	resourceDone chan struct{}
	ended        chan struct{}
	endCtxErr    chan error
	endResult    chan Result
	endErr       chan error
}

func (*pendingResourceTurnHook) Name() string { return "pending-resource-turn" }

func (*pendingResourceTurnHook) BeforeRun(context.Context, Submission) error { return nil }

func (*pendingResourceTurnHook) AfterRun(context.Context, Submission, Result, error) error {
	return nil
}

func (h *pendingResourceTurnHook) OnTurnStart(ctx context.Context, _ string) error {
	registerRunPendingResource(ctx, "pending-hook-resource", "test", h.resourceDone)
	return nil
}

func (*pendingResourceTurnHook) OnCompaction(context.Context, int, int) error { return nil }

func (*pendingResourceTurnHook) OnError(context.Context, string, error) error { return nil }

func (h *pendingResourceTurnHook) OnTurnEnd(ctx context.Context, _ string, result Result, err error) error {
	h.endCtxErr <- ctx.Err()
	h.endResult <- result
	h.endErr <- err
	close(h.ended)
	return nil
}

func TestTurnRunnerTransfersOriginalContextUntilPendingOwnerJoins(t *testing.T) {
	resourceDone := make(chan struct{})
	var releaseResource sync.Once
	defer releaseResource.Do(func() { close(resourceDone) })
	hook := &pendingResourceTurnHook{
		resourceDone: resourceDone,
		ended:        make(chan struct{}),
		endCtxErr:    make(chan error, 1),
		endResult:    make(chan Result, 1),
		endErr:       make(chan error, 1),
	}
	guard := NewDefaultRunSlotGuard()
	runner := NewEinoTurnRunner(wrapperBoundaryLoop{})
	runner.SetRunSlotGuard(guard)
	runner.SetHookChain(NewHookChain(hook))
	runner.cleanupTimeout = time.Millisecond
	result, err := runner.RunTurn(context.Background(), Submission{Content: "hello"})
	require.ErrorIs(t, err, errRunCleanupPending)
	require.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	select {
	case <-hook.ended:
		t.Fatal("post-run lifecycle must wait for the pending resource")
	case <-time.After(20 * time.Millisecond):
	}
	releaseResource.Do(func() { close(resourceDone) })
	select {
	case <-hook.ended:
	case <-time.After(time.Second):
		t.Fatal("late OnTurnEnd was not run after the owner joined")
	}
	require.NoError(t, <-hook.endCtxErr, "RunTurn returning pending must not cancel the inherited hook context")
	finalResult := <-hook.endResult
	require.Equal(t, RunExecutionCleanupPending, finalResult.ExecutionState, "late hooks observe the immutable result returned to the caller")
	require.ErrorIs(t, <-hook.endErr, errRunCleanupPending)
	require.Equal(t, RunExecutionQuiescent, finalResult.StatusHandle.Snapshot().ExecutionState, "the status handle remains a live view independent of the result snapshot")
	select {
	case <-guard.CloseAdmission():
	case <-time.After(time.Second):
		t.Fatal("entrypoint wrapper did not release after the late hook")
	}
}

type panickingWrapperRegistrarGuard struct{ releases atomic.Int32 }

func (*panickingWrapperRegistrarGuard) ClaimRun(context.Context) (RunClaim, error) {
	return RunClaim{ID: "panicking-wrapper-registration"}, nil
}

func (*panickingWrapperRegistrarGuard) ExecuteClaimedRun(_ RunClaim, fn func() error) error {
	return fn()
}

func (g *panickingWrapperRegistrarGuard) Release(RunClaim) { g.releases.Add(1) }

func (*panickingWrapperRegistrarGuard) RegisterRunWrapper(RunClaim, RunStatusHandle) (func(), error) {
	panic("registrar setup panic")
}

func (*panickingWrapperRegistrarGuard) SetRunWrapperStatus(RunClaim, RunStatusHandle) error {
	return nil
}

func (*panickingWrapperRegistrarGuard) ActiveRunStatus() (RunStatus, bool) {
	return RunStatus{}, false
}

func TestEntrypointsReleaseClaimWhenWrapperRegistrarPanics(t *testing.T) {
	t.Run("harness", func(t *testing.T) {
		guard := &panickingWrapperRegistrarGuard{}
		harness := NewHarnessImpl(wrapperBoundaryAgent{}, WithRunSlotGuard(guard))
		require.Panics(t, func() { _, _ = harness.Submit(context.Background(), "hello") })
		require.EqualValues(t, 1, guard.releases.Load())
	})
	t.Run("turn-runner", func(t *testing.T) {
		guard := &panickingWrapperRegistrarGuard{}
		runner := NewEinoTurnRunner(wrapperBoundaryLoop{})
		runner.SetRunSlotGuard(guard)
		require.Panics(t, func() { _, _ = runner.RunTurn(context.Background(), Submission{Content: "hello"}) })
		require.EqualValues(t, 1, guard.releases.Load())
	})
}
