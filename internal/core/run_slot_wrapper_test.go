package core

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

type wrapperBoundaryAgent struct{}

func (wrapperBoundaryAgent) Name() string { return "wrapper-boundary" }

func (wrapperBoundaryAgent) Run(context.Context, Submission, ...EventStream) (Result, error) {
	return Result{Message: "done"}, nil
}

type wrapperBoundaryLoop struct{}

func (wrapperBoundaryLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return []AgentEvent{{Kind: "message", Content: "done"}}, nil
}

type countingBoundaryLoop struct{ calls atomic.Int32 }

func (l *countingBoundaryLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	l.calls.Add(1)
	return []AgentEvent{{Kind: "message", Content: "done"}}, nil
}

type countingSpanExporter struct{ calls atomic.Int32 }

func (e *countingSpanExporter) ExportSpan(context.Context, tracing.TraceSpan) error {
	e.calls.Add(1)
	return nil
}

func (*countingSpanExporter) Shutdown(context.Context) error { return nil }

type blockingSpanExporter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *blockingSpanExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	if span.Name() == "harness.start" {
		e.once.Do(func() { close(e.started) })
		<-e.release
	}
	return nil
}

func (*blockingSpanExporter) Shutdown(context.Context) error { return nil }

type blockingTurnLifecycleHook struct {
	startStarted chan struct{}
	endStarted   chan struct{}
	release      chan struct{}
	blockStart   bool
	blockEnd     bool
	panicStart   bool
	startOnce    sync.Once
	endOnce      sync.Once
}

func (*blockingTurnLifecycleHook) Name() string { return "blocking-turn-boundary" }

func (*blockingTurnLifecycleHook) BeforeRun(context.Context, Submission) error { return nil }

func (*blockingTurnLifecycleHook) AfterRun(context.Context, Submission, Result, error) error {
	return nil
}

func (h *blockingTurnLifecycleHook) OnTurnStart(ctx context.Context, _ string) error {
	if h.panicStart {
		panic("turn start panic")
	}
	if !h.blockStart {
		return nil
	}
	h.startOnce.Do(func() { close(h.startStarted) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-h.release:
		return nil
	}
}

func (h *blockingTurnLifecycleHook) OnTurnEnd(context.Context, string, Result, error) error {
	if !h.blockEnd {
		return nil
	}
	h.endOnce.Do(func() { close(h.endStarted) })
	<-h.release
	return nil
}

func (*blockingTurnLifecycleHook) OnCompaction(context.Context, int, int) error { return nil }

func (*blockingTurnLifecycleHook) OnError(context.Context, string, error) error { return nil }

func TestHarnessRunSlotCoversSpanEndAndStreamClose(t *testing.T) {
	guard := NewDefaultRunSlotGuard()
	exporter := &blockingSpanExporter{started: make(chan struct{}), release: make(chan struct{})}
	harness := NewHarnessImpl(
		wrapperBoundaryAgent{},
		WithRunSlotGuard(guard),
		WithHarnessTracer(tracing.NewTracer("wrapper-test", exporter)),
		WithEventBuffer(4),
	)

	stream, err := harness.Submit(context.Background(), "hello")
	require.NoError(t, err)
	select {
	case <-exporter.started:
	case <-time.After(time.Second):
		t.Fatal("harness span export did not start")
	}

	claimCtx, cancelClaim := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelClaim()
	claim, claimErr := guard.ClaimRun(claimCtx)
	if claimErr == nil {
		guard.Release(claim)
	}
	require.ErrorIs(t, claimErr, context.DeadlineExceeded, "a new run must not pass the active harness wrapper")

	close(exporter.release)
	streamClosed := make(chan struct{})
	go func() {
		for range stream.Events() {
		}
		close(streamClosed)
	}()
	select {
	case <-streamClosed:
	case <-time.After(time.Second):
		t.Fatal("harness stream did not close after span export returned")
	}

	nextClaim, nextErr := guard.ClaimRun(context.Background())
	require.NoError(t, nextErr)
	guard.Release(nextClaim)
}

func TestTurnRunnerRunSlotCoversTurnStartHook(t *testing.T) {
	guard := NewDefaultRunSlotGuard()
	hook := &blockingTurnLifecycleHook{startStarted: make(chan struct{}), endStarted: make(chan struct{}), release: make(chan struct{}), blockStart: true}
	runner := NewEinoTurnRunner(wrapperBoundaryLoop{})
	runner.SetRunSlotGuard(guard)
	runner.SetHookChain(NewHookChain(hook))

	resultCh := make(chan error, 1)
	go func() {
		_, err := runner.RunTurn(context.Background(), Submission{Content: "hello"})
		resultCh <- err
	}()
	select {
	case <-hook.startStarted:
	case <-time.After(time.Second):
		t.Fatal("turn-start hook did not start")
	}

	idle := guard.CloseAdmission()
	select {
	case <-idle:
		t.Fatal("guard became idle while an accepted turn-start hook was running")
	case <-time.After(30 * time.Millisecond):
	}

	close(hook.release)
	select {
	case err := <-resultCh:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("turn did not finish after turn-start hook was released")
	}
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("guard did not become idle after the accepted turn wrapper finished")
	}
}

func TestTurnRunnerRunSlotCoversPostRunHooks(t *testing.T) {
	guard := NewDefaultRunSlotGuard()
	hook := &blockingTurnLifecycleHook{startStarted: make(chan struct{}), endStarted: make(chan struct{}), release: make(chan struct{}), blockEnd: true}
	runner := NewEinoTurnRunner(wrapperBoundaryLoop{})
	runner.SetRunSlotGuard(guard)
	runner.SetHookChain(NewHookChain(hook))

	resultCh := make(chan error, 1)
	go func() {
		_, err := runner.RunTurn(context.Background(), Submission{Content: "hello"})
		resultCh <- err
	}()
	select {
	case <-hook.endStarted:
	case <-time.After(time.Second):
		t.Fatal("turn hook did not start")
	}

	claimCtx, cancelClaim := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelClaim()
	_, claimErr := guard.ClaimRun(claimCtx)
	require.ErrorIs(t, claimErr, context.DeadlineExceeded, "a new run must not overlap the previous turn's post-run hook")

	close(hook.release)
	select {
	case err := <-resultCh:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("turn did not return after its post-run hook was released")
	}

	claim, err := guard.ClaimRun(context.Background())
	require.NoError(t, err)
	guard.Release(claim)
}

func TestRunSlotWrapperAndOwnerReleaseBothGateClaim(t *testing.T) {
	for _, releaseOwnerFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "owner-first", false: "wrapper-first"}[releaseOwnerFirst], func(t *testing.T) {
			guard := NewDefaultRunSlotGuard()
			claim, err := guard.ClaimRun(context.Background())
			require.NoError(t, err)
			wrapperDone, err := guard.RegisterRunWrapper(claim, nil)
			require.NoError(t, err)

			if releaseOwnerFirst {
				guard.Release(claim)
			} else {
				wrapperDone()
			}
			assertClaimRemainsBlocked(t, guard)
			idle := guard.CloseAdmission()
			select {
			case <-idle:
				t.Fatal("idle closed before both owner release and wrapper completion")
			default:
			}

			if releaseOwnerFirst {
				wrapperDone()
			} else {
				guard.Release(claim)
			}
			select {
			case <-idle:
			case <-time.After(time.Second):
				t.Fatal("idle did not close after owner and wrapper completed")
			}
		})
	}
}

func TestRunSlotPureClaimReleasesWithoutWrapper(t *testing.T) {
	guard := NewDefaultRunSlotGuard()
	claim, err := guard.ClaimRun(context.Background())
	require.NoError(t, err)
	idle := guard.CloseAdmission()
	guard.Release(claim)
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("pure selector claim did not release without a wrapper")
	}
}

func TestRunSlotWrapperRegistrationRejectsDuplicateAndStaleClaims(t *testing.T) {
	guard := NewDefaultRunSlotGuard()
	claim, err := guard.ClaimRun(context.Background())
	require.NoError(t, err)
	_, err = guard.RegisterRunWrapper(RunClaim{ID: "stale"}, nil)
	require.ErrorIs(t, err, ErrRunSlotStaleClaim)
	done, err := guard.RegisterRunWrapper(claim, nil)
	require.NoError(t, err)
	_, err = guard.RegisterRunWrapper(claim, nil)
	require.ErrorIs(t, err, ErrRunWrapperAlreadyRegistered)
	guard.Release(claim)
	done()
}

func TestRunSlotWrapperStatusStartsActiveAndAttachesOnce(t *testing.T) {
	guard := NewDefaultRunSlotGuard()
	claim, err := guard.ClaimRun(context.Background())
	require.NoError(t, err)
	done, err := guard.RegisterRunWrapper(claim, nil)
	require.NoError(t, err)
	status, ok := guard.ActiveRunStatus()
	require.True(t, ok, "a wrapper lease is active even before owner status is attached")
	assert.Equal(t, claim.ID, status.RunID)
	assert.Equal(t, RunExecutionRunning, status.ExecutionState)

	handle := probingRunStatusHandle{guard: guard, call: make(chan error, 1)}
	require.NoError(t, guard.SetRunWrapperStatus(claim, handle))
	require.ErrorIs(t, guard.SetRunWrapperStatus(claim, handle), ErrRunWrapperStatusAlreadySet)
	status, ok = guard.ActiveRunStatus()
	require.True(t, ok)
	assert.Equal(t, RunExecutionCleanupPending, status.ExecutionState)
	guard.Release(claim)
	done()
}

type probingRunStatusHandle struct {
	guard *DefaultRunSlotGuard
	call  chan error
}

func (h probingRunStatusHandle) Snapshot() RunStatus {
	_, err := h.guard.TryClaimRun()
	h.call <- err
	return RunStatus{RunID: "active", ExecutionState: RunExecutionCleanupPending}
}

func TestRunSlotActiveStatusCallsHandleOutsideGuardLock(t *testing.T) {
	guard := NewDefaultRunSlotGuard()
	claim, err := guard.ClaimRun(context.Background())
	require.NoError(t, err)
	handle := probingRunStatusHandle{guard: guard, call: make(chan error, 1)}
	done, err := guard.RegisterRunWrapper(claim, handle)
	require.NoError(t, err)

	statusDone := make(chan struct{})
	var status RunStatus
	var ok bool
	go func() {
		status, ok = guard.ActiveRunStatus()
		close(statusDone)
	}()
	select {
	case <-statusDone:
	case <-time.After(time.Second):
		t.Fatal("ActiveRunStatus deadlocked while calling an external status handle")
	}
	select {
	case err := <-handle.call:
		require.ErrorIs(t, err, ErrRunSlotBusy)
	default:
		t.Fatal("status handle was not called")
	}
	assert.True(t, ok)
	assert.Equal(t, RunExecutionCleanupPending, status.ExecutionState)
	guard.Release(claim)
	done()
}

func assertClaimRemainsBlocked(t *testing.T, guard *DefaultRunSlotGuard) {
	t.Helper()
	claim, err := guard.TryClaimRun()
	if err == nil {
		guard.Release(claim)
	}
	require.ErrorIs(t, err, ErrRunSlotBusy)
}

func TestHarnessAndTurnRunnerRejectAfterAdmissionFenceBeforeCallbacks(t *testing.T) {
	t.Run("harness", func(t *testing.T) {
		guard := NewDefaultRunSlotGuard()
		guard.CloseAdmission()
		exporter := &countingSpanExporter{}
		harness := NewHarnessImpl(wrapperBoundaryAgent{}, WithRunSlotGuard(guard), WithHarnessTracer(tracing.NewTracer("fenced", exporter)))
		var stream EventStream
		var err error
		require.NotPanics(t, func() {
			stream, err = harness.Submit(panicValueContext{Context: context.Background()}, "hello")
		})
		require.Nil(t, stream)
		require.ErrorIs(t, err, ErrRunAdmissionClosed)
		assert.Zero(t, exporter.calls.Load(), "closed admission must reject before borrowing the tracer")
	})

	t.Run("turn-runner", func(t *testing.T) {
		guard := NewDefaultRunSlotGuard()
		guard.CloseAdmission()
		loop := &countingBoundaryLoop{}
		hook := &spyLifecycleHook{name: "fenced", order: new([]string)}
		runner := NewEinoTurnRunner(loop)
		runner.SetRunSlotGuard(guard)
		runner.SetHookChain(NewHookChain(hook))
		exporter := &countingSpanExporter{}
		ctx := tracing.NewTracer("fenced", exporter).ContextWithTracer(panicValueContext{Context: context.Background()})
		var err error
		require.NotPanics(t, func() {
			_, err = runner.RunTurn(ctx, Submission{Content: "hello"})
		})
		require.ErrorIs(t, err, ErrRunAdmissionClosed)
		assert.Zero(t, loop.calls.Load(), "closed admission must reject before provider work")
		assert.Empty(t, hook.recorded(), "closed admission must reject before lifecycle callbacks")
		assert.Zero(t, exporter.calls.Load(), "closed admission must reject before borrowing the tracer")
	})
}

func TestEntrySpanSetupPanicReleasesClaimAndWrapper(t *testing.T) {
	t.Run("harness", func(t *testing.T) {
		guard := NewDefaultRunSlotGuard()
		var calls atomic.Int32
		harness := NewHarnessImpl(countingBoundaryAgent{calls: &calls}, WithRunSlotGuard(guard), WithEventBuffer(2))
		require.Panics(t, func() {
			_, _ = harness.Submit(panicValueContext{Context: context.Background()}, "hello")
		})
		assert.Zero(t, calls.Load())
		claim, err := guard.TryClaimRun()
		require.NoError(t, err, "Submit must return its claim and wrapper lease after span setup panics")
		guard.Release(claim)
	})

	t.Run("turn-runner", func(t *testing.T) {
		guard := NewDefaultRunSlotGuard()
		loop := &countingBoundaryLoop{}
		hook := &spyLifecycleHook{name: "panic-context", order: new([]string)}
		runner := NewEinoTurnRunner(loop)
		runner.SetRunSlotGuard(guard)
		runner.SetHookChain(NewHookChain(hook))
		require.Panics(t, func() {
			_, _ = runner.RunTurn(panicValueContext{Context: context.Background()}, Submission{Content: "hello"})
		})
		assert.Zero(t, loop.calls.Load())
		assert.Empty(t, hook.recorded())
		claim, err := guard.TryClaimRun()
		require.NoError(t, err, "RunTurn must return its claim and wrapper lease after span setup panics")
		guard.Release(claim)
	})
}

func TestTurnStartHookPanicIsContainedByOwnerAndReleasesWrapper(t *testing.T) {
	guard := NewDefaultRunSlotGuard()
	hook := &blockingTurnLifecycleHook{startStarted: make(chan struct{}), endStarted: make(chan struct{}), release: make(chan struct{}), panicStart: true}
	runner := NewEinoTurnRunner(wrapperBoundaryLoop{})
	runner.SetRunSlotGuard(guard)
	runner.SetHookChain(NewHookChain(hook))

	result, err := runner.RunTurn(context.Background(), Submission{Content: "hello"})
	require.Error(t, err)
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	claim, err := guard.TryClaimRun()
	require.NoError(t, err, "a start-hook panic must not leave the wrapper claim held")
	guard.Release(claim)
}

func TestMessageSnapshotPanicIsContainedByOwnerAndReleasesWrapper(t *testing.T) {
	guard := NewDefaultRunSlotGuard()
	loop := &countingBoundaryLoop{}
	runner := NewEinoTurnRunner(loop)
	runner.SetAgent(panicMessageAgent{})
	runner.SetRunSlotGuard(guard)
	runner.SetHookChain(NewHookChain(&spyLifecycleHook{name: "message-panic", order: new([]string)}))

	result, err := runner.RunTurn(context.Background(), Submission{Content: "hello"})
	require.Error(t, err)
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	assert.Zero(t, loop.calls.Load())
	claim, err := guard.TryClaimRun()
	require.NoError(t, err, "message snapshot panics must not strand the wrapper claim")
	guard.Release(claim)
}

func TestRunLifetimeCancellationStopsRunInput(t *testing.T) {
	t.Run("harness", func(t *testing.T) {
		lifetime, cancelLifetime := context.WithCancel(context.Background())
		defer cancelLifetime()
		started := make(chan struct{})
		agent := lifecycleCancelAgent{started: started}
		harness := NewHarnessImpl(agent, WithRunLifetimeContext(lifetime), WithEventBuffer(4))
		stream, err := harness.Submit(context.Background(), "hello")
		require.NoError(t, err)
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("harness agent did not start")
		}
		cancelLifetime()
		select {
		case <-stream.Events():
		case <-time.After(time.Second):
			t.Fatal("harness stream did not close after lifetime cancellation")
		}
		for range stream.Events() {
		}
		_, resultErr := stream.Result()
		require.ErrorIs(t, resultErr, context.Canceled)
	})

	t.Run("turn-runner", func(t *testing.T) {
		lifetime, cancelLifetime := context.WithCancel(context.Background())
		defer cancelLifetime()
		started := make(chan struct{})
		runner := NewEinoTurnRunner(lifecycleCancelLoop{started: started})
		runner.SetRunLifetimeContext(lifetime)
		resultCh := make(chan error, 1)
		go func() {
			_, err := runner.RunTurn(context.Background(), Submission{Content: "hello"})
			resultCh <- err
		}()
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("turn loop did not start")
		}
		cancelLifetime()
		select {
		case err := <-resultCh:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("turn runner did not return after lifetime cancellation")
		}
	})
}

type lifecycleCancelAgent struct{ started chan struct{} }

func (a lifecycleCancelAgent) Name() string { return "lifecycle-cancel" }

func (a lifecycleCancelAgent) Run(ctx context.Context, _ Submission, _ ...EventStream) (Result, error) {
	close(a.started)
	<-ctx.Done()
	return Result{}, ctx.Err()
}

type lifecycleCancelLoop struct{ started chan struct{} }

func (l lifecycleCancelLoop) Run(ctx context.Context, _ Submission, _ ...EventStream) ([]AgentEvent, error) {
	close(l.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type countingBoundaryAgent struct{ calls *atomic.Int32 }

func (a countingBoundaryAgent) Name() string { return "counting-boundary" }

func (a countingBoundaryAgent) Run(context.Context, Submission, ...EventStream) (Result, error) {
	a.calls.Add(1)
	return Result{Message: "done"}, nil
}

type panicValueContext struct{ context.Context }

func (panicValueContext) Value(any) any { panic("span setup value panic") }

type panicMessageAgent struct{}

func (panicMessageAgent) Name() string { return "panic-message-agent" }

func (panicMessageAgent) Run(context.Context, Submission, ...EventStream) (Result, error) {
	return Result{Message: "unexpected"}, nil
}

func (panicMessageAgent) Messages() []AgentMessage { panic("message snapshot panic") }
