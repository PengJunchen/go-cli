package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

type countingRunSlotGuard struct {
	mu       sync.Mutex
	claim    RunClaim
	released []RunClaim
}

func newCountingRunSlotGuard() *countingRunSlotGuard {
	return &countingRunSlotGuard{claim: RunClaim{ID: "test-claim", AcquiredAt: time.Now()}}
}

func (g *countingRunSlotGuard) ClaimRun(ctx context.Context) (RunClaim, error) {
	select {
	case <-ctx.Done():
		return RunClaim{}, ctx.Err()
	default:
		return g.claim, nil
	}
}

func (g *countingRunSlotGuard) ExecuteClaimedRun(_ RunClaim, fn func() error) error { return fn() }

func (g *countingRunSlotGuard) Release(claim RunClaim) {
	g.mu.Lock()
	g.released = append(g.released, claim)
	g.mu.Unlock()
}

func (g *countingRunSlotGuard) releaseCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.released)
}

func TestRunOwnerNormalCallbackCanExceedCleanupTimeout(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	started := time.Now()

	result, err := owner.run(20*time.Millisecond, func(context.Context) (Result, error) {
		time.Sleep(60 * time.Millisecond)
		return Result{Message: "finished"}, nil
	})

	require.NoError(t, err)
	assert.Equal(t, "finished", result.Message)
	assert.Equal(t, RunOutcomeSuccess, result.Outcome)
	assert.GreaterOrEqual(t, time.Since(started), 50*time.Millisecond)
	assert.Equal(t, RunExecutionQuiescent, result.StatusHandle.Snapshot().ExecutionState)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestTransientBackpressureDoesNotCancelLongRun(t *testing.T) {
	const cleanupTimeout = 40 * time.Millisecond
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	backpressure := &observedBackpressureStatus{runOwner: owner, began: make(chan struct{}), resumed: make(chan struct{})}
	stream := NewEventStream(1)
	stream.SetRunStatusHandle(backpressure)
	secondSendDone := make(chan struct{})
	callbackDone := make(chan struct{})
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(cleanupTimeout, func(ctx context.Context) (Result, error) {
			defer close(callbackDone)
			if err := stream.Send(AgentEvent{Kind: "message", Content: "fills buffer"}); err != nil {
				return Result{}, err
			}
			if err := stream.Send(AgentEvent{Kind: "message", Content: "blocked event"}); err != nil {
				close(secondSendDone)
				return Result{}, err
			}
			close(secondSendDone)
			time.Sleep(3 * cleanupTimeout)
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			return Result{Message: "completed after drain"}, nil
		})
	}()
	<-backpressure.began
	_, ok := <-stream.Events()
	require.True(t, ok, "the initial event should fill the buffer")
	select {
	case <-backpressure.resumed:
	case <-time.After(time.Second):
		t.Fatal("blocked send did not resume after the consumer drained the buffer")
	}
	<-secondSendDone
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("run did not finish after the callback completed")
	}
	<-callbackDone
	stream.Close()

	require.NoError(t, runErr)
	assert.Equal(t, "completed after drain", result.Message)
	assert.Equal(t, RunOutcomeSuccess, result.Outcome)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestContinuousBackpressureBoundsBlockedSendAndPublishesResult(t *testing.T) {
	const cleanupTimeout = 25 * time.Millisecond
	guard := newCountingRunSlotGuard()
	stream := NewEventStream(1)
	owner := newRunOwner(context.Background(), guard, guard.claim, func(result Result, err error) {
		stream.SetResult(AgentMessage{Role: "assistant", Content: result.Message}, err)
		_ = stream.SendTerminal(AgentEvent{Kind: "error", Content: "cleanup pending"})
		stream.Close()
	})
	owner.SetEventGapSource(stream)
	backpressure := &observedBackpressureStatus{runOwner: owner, began: make(chan struct{}), resumed: make(chan struct{})}
	stream.SetRunStatusHandle(backpressure)
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(cleanupTimeout, func(ctx context.Context) (Result, error) {
			if err := stream.Send(AgentEvent{Kind: "message", Content: "fills buffer"}); err != nil {
				return Result{}, err
			}
			if err := stream.Send(AgentEvent{Kind: "message", Content: "blocked"}); err != nil {
				return Result{}, err
			}
			return Result{}, ctx.Err()
		})
	}()
	<-backpressure.began
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("sustained blocked Send did not trigger bounded cleanup")
	}
	require.ErrorIs(t, runErr, errRunCleanupPending)
	for range stream.Events() {
		// The initial event remains buffered when cleanup closes the stream.
	}
	_, streamErr := stream.Result()
	require.ErrorIs(t, streamErr, errRunCleanupPending)
	status := owner.Snapshot()
	assert.Equal(t, RunOutcomeUnknown, status.Outcome)
	assert.Greater(t, status.EventGapCount, uint64(0))
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	waitForOwnerState(t, result.StatusHandle, RunExecutionQuiescent)
	assert.Equal(t, 1, guard.releaseCount())
}

type observedBackpressureStatus struct {
	*runOwner
	began      chan struct{}
	resumed    chan struct{}
	beginOnce  sync.Once
	resumeOnce sync.Once
}

func (status *observedBackpressureStatus) BeginBackpressure() func() {
	resume := status.runOwner.BeginBackpressure()
	status.beginOnce.Do(func() { close(status.began) })
	return func() {
		resume()
		status.resumeOnce.Do(func() { close(status.resumed) })
	}
}

func TestOwnerDoesNotStartCallbackAfterClaimContextCanceled(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	guard := newCountingRunSlotGuard()
	claim := guard.claim // the run slot was claimed before the caller canceled.
	cancel()
	owner := newRunOwner(parent, guard, claim, nil)
	var callbackCalls atomic.Int32

	result, err := owner.run(time.Second, func(context.Context) (Result, error) {
		callbackCalls.Add(1)
		return Result{Message: "should not run"}, nil
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, callbackCalls.Load(), "a pre-canceled run must not dispatch provider or tool work")
	assert.Equal(t, RunOutcomeCancelled, result.Outcome)
	assert.Equal(t, RunExecutionQuiescent, result.ExecutionState)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerCancellationOutcomeDoesNotDependOnWatcherScheduling(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		parent, cancel := context.WithCancel(context.Background())
		guard := newCountingRunSlotGuard()
		owner := newRunOwner(parent, guard, guard.claim, nil)
		result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
			cancel()
			return Result{}, ctx.Err()
		})
		require.ErrorIs(t, err, context.Canceled, "attempt %d", attempt)
		assert.Equal(t, RunOutcomeCancelled, result.Outcome, "attempt %d", attempt)
		assert.Equal(t, 1, guard.releaseCount(), "attempt %d", attempt)
		cancel()
	}
}

func TestCleanupPendingAfterQuiescencePreservesCompletedResult(t *testing.T) {
	guard := newCountingRunSlotGuard()
	var timeoutCalled bool
	owner := newRunOwner(context.Background(), guard, guard.claim, func(Result, error) {
		timeoutCalled = true
	})

	_, err := owner.run(time.Second, func(context.Context) (Result, error) {
		return Result{Message: "complete"}, nil
	})
	require.NoError(t, err)

	lateResult, lateErr := owner.cleanupPending()
	require.NoError(t, lateErr)
	assert.Equal(t, "complete", lateResult.Message)
	assert.Equal(t, RunExecutionQuiescent, lateResult.ExecutionState)
	assert.False(t, timeoutCalled, "a timeout race after release must not publish a false pending result")
	assert.Equal(t, 1, guard.releaseCount())
}

type releaseBarrierGuard struct {
	*countingRunSlotGuard
	releaseEntered chan struct{}
	allowRelease   chan struct{}
}

func (g *releaseBarrierGuard) Release(claim RunClaim) {
	close(g.releaseEntered)
	<-g.allowRelease
	g.countingRunSlotGuard.Release(claim)
}

func TestCleanupPendingRaceWithFinalReleaseDoesNotPublishPending(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()
	baseGuard := newCountingRunSlotGuard()
	guard := &releaseBarrierGuard{
		countingRunSlotGuard: baseGuard,
		releaseEntered:       make(chan struct{}),
		allowRelease:         make(chan struct{}),
	}
	var timeoutCalled bool
	owner := newRunOwner(parent, guard, baseGuard.claim, func(Result, error) { timeoutCalled = true })
	owner.MarkCancelRequested()
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(25*time.Millisecond, func(ctx context.Context) (Result, error) {
			return Result{}, ctx.Err()
		})
	}()

	select {
	case <-guard.releaseEntered:
	case <-time.After(time.Second):
		t.Fatal("owner did not reach final claim release")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup timeout did not return while final release was blocked")
	}
	close(guard.allowRelease)
	<-owner.done

	require.ErrorIs(t, runErr, context.Canceled)
	assert.Equal(t, RunOutcomeCancelled, result.Outcome)
	assert.False(t, timeoutCalled, "the owner already proved quiescence before the timeout handler ran")
	assert.Equal(t, 1, baseGuard.releaseCount())
}

type contextErrorTool struct {
	err   error
	calls *atomic.Int32
}

func (tool contextErrorTool) Name() string        { return "context-error" }
func (tool contextErrorTool) Description() string { return "returns a context error after dispatch" }
func (tool contextErrorTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	if tool.calls != nil {
		tool.calls.Add(1)
	}
	return nil, tool.err
}

func TestDispatchedContextErrorMakesOwnerOutcomeUnknown(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	registry := scriptedRegistry(contextErrorTool{err: context.Canceled})

	result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		_, _, toolErr := executeSingleTool(ctx, registry, tools.ToolCall{ID: "call-1", Name: "context-error"}, nil)
		return Result{}, toolErr
	})

	require.ErrorIs(t, err, context.Canceled)
	var unknown runOutcomeUnknown
	require.ErrorAs(t, err, &unknown)
	assert.True(t, unknown.OutcomeUnknown())
	status := result.StatusHandle.Snapshot()
	assert.Equal(t, RunOutcomeUnknown, status.Outcome)
	assert.Equal(t, RunExecutionQuiescent, status.ExecutionState)
	assert.Equal(t, 1, guard.releaseCount(), "unknown outcome is releasable once all owned work has stopped")
}

type knownInterruptedError struct{ cause error }

func (e knownInterruptedError) Error() string        { return "execution stopped before side effect" }
func (e knownInterruptedError) Unwrap() error        { return e.cause }
func (e knownInterruptedError) OutcomeUnknown() bool { return false }

func TestDispatchedKnownInterruptedContextErrorIsNotUnknown(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	var dispatches atomic.Int32
	registry := scriptedRegistry(contextErrorTool{err: knownInterruptedError{cause: context.Canceled}, calls: &dispatches})

	result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		cancel()
		_, _, toolErr := executeSingleTool(ctx, registry, tools.ToolCall{ID: "call-1", Name: "context-error"}, nil)
		return Result{}, toolErr
	})

	assert.EqualValues(t, 1, dispatches.Load(), "the known-interrupted receipt must come from a dispatched tool")
	require.ErrorIs(t, err, context.Canceled)
	var explicit runOutcomeUnknown
	require.ErrorAs(t, err, &explicit)
	assert.False(t, explicit.OutcomeUnknown())
	assert.Equal(t, RunOutcomeCancelled, result.StatusHandle.Snapshot().Outcome)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestToolLookupFailureDoesNotMarkRunUnknown(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	registry := scriptedRegistry()

	result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		_, _, toolErr := executeSingleTool(ctx, registry, tools.ToolCall{ID: "call-1", Name: "missing"}, nil)
		return Result{}, toolErr
	})

	require.Error(t, err)
	var unknown runOutcomeUnknown
	assert.False(t, errors.As(err, &unknown) && unknown.OutcomeUnknown())
	assert.Equal(t, RunOutcomeError, result.Outcome)
	assert.Equal(t, RunExecutionQuiescent, result.StatusHandle.Snapshot().ExecutionState)
	assert.Equal(t, 1, guard.releaseCount())
}

type nilResultTool struct{}

func (nilResultTool) Name() string        { return "nil-result" }
func (nilResultTool) Description() string { return "returns no receipt" }
func (nilResultTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return nil, nil
}

func TestDispatchedToolWithoutReceiptMarksRunUnknown(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	registry := scriptedRegistry(nilResultTool{})

	result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		_, _, toolErr := executeSingleTool(ctx, registry, tools.ToolCall{ID: "call-1", Name: "nil-result"}, nil)
		return Result{}, toolErr
	})

	require.Error(t, err)
	var unknown runOutcomeUnknown
	require.ErrorAs(t, err, &unknown)
	assert.True(t, unknown.OutcomeUnknown())
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerCancellationKeepsClaimUntilWorkerStops(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	workerStarted := make(chan struct{})
	workerRelease := make(chan struct{})
	releaseOnce := sync.Once{}
	releaseWorker := func() { releaseOnce.Do(func() { close(workerRelease) }) }
	t.Cleanup(releaseWorker)
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(30*time.Millisecond, func(ctx context.Context) (Result, error) {
			workerDone := registerRunWorker(ctx, "call-1", "blocked")
			go func() {
				close(workerStarted)
				<-workerRelease
				workerDone()
			}()
			<-ctx.Done()
			return Result{}, ctx.Err()
		})
	}()
	<-workerStarted
	cancel()

	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("owner did not return after cleanup timeout")
	}
	require.Error(t, runErr)
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	assert.Equal(t, 0, guard.releaseCount(), "claim must remain held while the worker is active")

	releaseWorker()
	waitForOwnerState(t, result.StatusHandle, RunExecutionQuiescent)
	assert.Equal(t, 1, guard.releaseCount(), "the exact claim must be released once after worker completion")
	assert.Equal(t, RunOutcomeUnknown, result.StatusHandle.Snapshot().Outcome)
}

func TestRunOwnerNonCooperativeCallbackReturnsPendingThenQuiesces(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	callbackStarted := make(chan struct{})
	callbackRelease := make(chan struct{})
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(25*time.Millisecond, func(context.Context) (Result, error) {
			close(callbackStarted)
			<-callbackRelease // deliberately ignores cancellation
			return Result{Message: "late completion"}, nil
		})
	}()
	<-callbackStarted
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("non-cooperative callback exceeded the bounded return")
	}
	require.ErrorIs(t, runErr, errRunCleanupPending)
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	assert.Equal(t, 0, guard.releaseCount(), "the callback still owns the local claim")
	waitForOwnerState(t, result.StatusHandle, RunExecutionQuarantined)

	close(callbackRelease)
	waitForOwnerState(t, result.StatusHandle, RunExecutionQuiescent)
	assert.Equal(t, RunOutcomeUnknown, result.StatusHandle.Snapshot().Outcome)
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState, "the returned result is an immutable snapshot")
	assert.Equal(t, 1, guard.releaseCount())
}

type pendingToolError struct {
	done    <-chan struct{}
	unknown bool
}

func (e pendingToolError) Error() string                  { return "tool execution remains active" }
func (e pendingToolError) OutcomeUnknown() bool           { return e.unknown }
func (e pendingToolError) ExecutionDone() <-chan struct{} { return e.done }
func (e pendingToolError) Unwrap() error                  { return context.DeadlineExceeded }

type pendingTool struct{ err error }

func (tool pendingTool) Name() string        { return "pending" }
func (tool pendingTool) Description() string { return "returns a pending execution error" }
func (tool pendingTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return nil, tool.err
}

func TestRunOwnerPendingExecutionResourceHoldsClaimUntilDone(t *testing.T) {
	for _, unknown := range []bool{true, false} {
		t.Run(fmt.Sprintf("outcome-unknown-%t", unknown), func(t *testing.T) {
			guard := newCountingRunSlotGuard()
			owner := newRunOwner(context.Background(), guard, guard.claim, nil)
			executionDone := make(chan struct{})
			registry := scriptedRegistry(pendingTool{err: pendingToolError{done: executionDone, unknown: unknown}})
			result, runErr := owner.run(20*time.Millisecond, func(ctx context.Context) (Result, error) {
				_, _, err := executeSingleTool(ctx, registry, tools.ToolCall{ID: "pending-1", Name: "pending"}, nil)
				return Result{}, err
			})

			require.Error(t, runErr)
			assert.Equal(t, 1, result.StatusHandle.Snapshot().PendingWorkers)
			assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
			if unknown {
				assert.Equal(t, RunOutcomeUnknown, result.Outcome)
			} else {
				assert.Equal(t, RunOutcomeError, result.Outcome)
			}
			assert.Equal(t, 0, guard.releaseCount(), "an open ExecutionDone channel cannot release the claim")

			close(executionDone)
			waitForOwnerState(t, result.StatusHandle, RunExecutionQuiescent)
			assert.Equal(t, result.Outcome, result.StatusHandle.Snapshot().Outcome)
			assert.Equal(t, 1, guard.releaseCount())
		})
	}
}

func TestOwnerStatusTracksEventGapAndTerminalIsSentOnce(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	stream := NewEventStream(1, WithEventDiscardPolicy(DiscardNewest))
	owner.SetEventGapSource(stream)
	stream.SetRunStatusHandle(owner)

	require.NoError(t, stream.Send(AgentEvent{Kind: "message", Content: "kept"}))
	require.NoError(t, stream.Send(AgentEvent{Kind: "message", Content: "dropped"}))
	assert.Equal(t, uint64(1), owner.Snapshot().EventGapCount)
	first, ok := <-stream.Events()
	require.True(t, ok)
	assert.Equal(t, "kept", first.Content)
	_ = stream.SendTerminal(AgentEvent{Kind: "done", Content: "first terminal"})
	_ = stream.SendTerminal(AgentEvent{Kind: "error", Content: "duplicate terminal"})
	stream.Close()

	var terminal []AgentEvent
	for event := range stream.Events() {
		terminal = append(terminal, event)
	}
	require.Len(t, terminal, 1)
	assert.Equal(t, "done", terminal[0].Kind)
	assert.Equal(t, "first terminal", terminal[0].Content)
	assert.Equal(t, uint64(1), owner.Snapshot().EventGapCount)
}

type recordingOwnerSpanExporter struct {
	mu    sync.Mutex
	spans []tracing.SpanData
}

func (e *recordingOwnerSpanExporter) ExportSpan(_ context.Context, span tracing.TraceSpan) error {
	e.mu.Lock()
	e.spans = append(e.spans, tracing.SpanToData(span))
	e.mu.Unlock()
	return nil
}

func (*recordingOwnerSpanExporter) Shutdown(context.Context) error { return nil }

func (e *recordingOwnerSpanExporter) find(name string) (tracing.SpanData, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, span := range e.spans {
		if span.Name == name {
			return span, true
		}
	}
	return tracing.SpanData{}, false
}

type controlledHarnessAgent struct {
	started      chan struct{}
	release      <-chan struct{}
	ignoreCancel bool
}

func (agent *controlledHarnessAgent) Name() string { return "controlled-harness" }

func (agent *controlledHarnessAgent) Run(ctx context.Context, _ Submission, _ ...EventStream) (Result, error) {
	close(agent.started)
	if agent.ignoreCancel {
		<-agent.release
		return Result{Message: "late"}, nil
	}
	select {
	case <-agent.release:
		return Result{Message: "completed"}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

func TestHarnessSpanEndsAfterRunAndIncludesOutcomeAttributes(t *testing.T) {
	release := make(chan struct{})
	agent := &controlledHarnessAgent{started: make(chan struct{}), release: release}
	exporter := &recordingOwnerSpanExporter{}
	tracer := tracing.NewTracer("owner-span", exporter)
	harness := NewHarnessImpl(agent, WithHarnessTracer(tracer), WithRunCleanupTimeout(30*time.Millisecond))
	stream, err := harness.Submit(context.Background(), "run")
	require.NoError(t, err)
	<-agent.started
	time.Sleep(10 * time.Millisecond)
	_, exported := exporter.find("harness.start")
	assert.False(t, exported, "the root span stays open while the callback is active")
	close(release)
	for range stream.Events() {
	}

	span, exported := exporter.find("harness.start")
	require.True(t, exported)
	assert.NotEmpty(t, span.EndTime)
	attrs := spanAttributes(span)
	assert.Equal(t, string(RunOutcomeSuccess), attrs["outcome"])
	assert.Equal(t, string(RunExecutionQuiescent), attrs["execution_state"])
	assert.NotEmpty(t, attrs["run_id"])
}

func TestHarnessSpanEndsAtPendingCleanupWithUnknownAttributes(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	agent := &controlledHarnessAgent{started: make(chan struct{}), release: release, ignoreCancel: true}
	exporter := &recordingOwnerSpanExporter{}
	tracer := tracing.NewTracer("owner-pending-span", exporter)
	harness := NewHarnessImpl(agent, WithHarnessTracer(tracer), WithRunCleanupTimeout(20*time.Millisecond))
	stream, err := harness.Submit(parent, "run")
	require.NoError(t, err)
	<-agent.started
	cancel()
	for range stream.Events() {
	}

	span, exported := exporter.find("harness.start")
	require.True(t, exported, "the span must end when bounded cleanup returns")
	assert.NotEmpty(t, span.EndTime)
	attrs := spanAttributes(span)
	assert.Equal(t, string(RunOutcomeUnknown), attrs["outcome"])
	assert.Equal(t, string(RunExecutionCleanupPending), attrs["execution_state"])

	close(release)
	statusSource, ok := stream.(RunStatusSource)
	require.True(t, ok)
	waitForOwnerState(t, statusSource.RunStatusHandle(), RunExecutionQuiescent)
	assert.Equal(t, string(RunExecutionCleanupPending), attrs["execution_state"], "exported span attributes are an immutable result snapshot")
}

func spanAttributes(span tracing.SpanData) map[string]any {
	attrs := make(map[string]any, len(span.Attributes))
	for _, attr := range span.Attributes {
		attrs[attr.Key] = attr.Value
	}
	return attrs
}

func TestRunOwnerPanickedCallbackQuarantinesClaim(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	result, err := owner.run(time.Second, func(context.Context) (Result, error) {
		panic("secret panic payload")
	})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret panic payload")
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	assert.Equal(t, RunExecutionQuarantined, result.ExecutionState)
	assert.Equal(t, 1, guard.releaseCount(), "the callback has joined even though durable state is quarantined")
}

type invalidCanonicalLoop struct{}

func (invalidCanonicalLoop) Run(context.Context, Submission, ...EventStream) ([]AgentEvent, error) {
	return nil, nil
}

func (invalidCanonicalLoop) RunCanonical(context.Context, Submission, ...EventStream) (CanonicalRunResult, error) {
	return CanonicalRunResult{Transcript: []TranscriptEntry{{Message: AgentMessage{Role: "assistant", Content: "not the submitted user input"}}}}, nil
}

func TestInvalidCanonicalDeltaQuarantinesRunClaim(t *testing.T) {
	guard := newCountingRunSlotGuard()
	runner := NewEinoTurnRunner(invalidCanonicalLoop{})
	runner.SetAgent(NewAgentImpl("invalid", invalidCanonicalLoop{}))
	runner.SetRunSlotGuard(guard)

	result, err := runner.RunTurn(context.Background(), Submission{Content: "hello"})

	require.ErrorIs(t, err, ErrCanonicalTranscriptInvalid)
	assert.Empty(t, result.Message)
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	assert.Equal(t, RunExecutionQuarantined, result.StatusHandle.Snapshot().ExecutionState)
	assert.Equal(t, 1, guard.releaseCount(), "the failed canonical commit quarantines durable state but releases the joined local claim")
}

type pendingHarnessAgent struct {
	started chan struct{}
	release <-chan struct{}
}

func (agent *pendingHarnessAgent) Name() string { return "pending-harness" }

func (agent *pendingHarnessAgent) Run(ctx context.Context, _ Submission, _ ...EventStream) (Result, error) {
	workerDone := registerRunWorker(ctx, "call-1", "pending")
	go func() {
		close(agent.started)
		<-agent.release
		workerDone()
	}()
	<-ctx.Done()
	return Result{}, ctx.Err()
}

func TestHarnessPublishesResultBeforeClosingOnPendingCleanup(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newCountingRunSlotGuard()
	release := make(chan struct{})
	var releaseOnce sync.Once
	finishWorker := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(finishWorker)
	agent := &pendingHarnessAgent{started: make(chan struct{}), release: release}
	harness := NewHarnessImpl(agent, WithRunCleanupTimeout(30*time.Millisecond), WithRunSlotGuard(guard))
	stream, err := harness.Submit(parent, "run")
	require.NoError(t, err)
	<-agent.started
	cancel()

	for range stream.Events() {
		// The terminal event may be buffered ahead of the close.
	}
	_, resultErr := stream.Result()
	require.Error(t, resultErr, "a closed stream must already carry the timeout result")
	assert.ErrorIs(t, resultErr, context.Canceled)
	statusSource, ok := stream.(RunStatusSource)
	require.True(t, ok)
	status := statusSource.RunStatusHandle().Snapshot()
	assert.Equal(t, RunOutcomeUnknown, status.Outcome)
	assert.Equal(t, 0, guard.releaseCount(), "stream closure does not release a live worker's claim")

	finishWorker()
	waitForOwnerState(t, statusSource.RunStatusHandle(), RunExecutionQuiescent)
	assert.Equal(t, RunOutcomeUnknown, statusSource.RunStatusHandle().Snapshot().Outcome)
	assert.Equal(t, 1, guard.releaseCount())
}

func waitForOwnerState(t *testing.T, handle RunStatusHandle, want RunExecutionState) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if handle.Snapshot().ExecutionState == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.Equal(t, want, handle.Snapshot().ExecutionState)
}

func TestToolOutcomeUnknownErrorUnwrapsCause(t *testing.T) {
	wrapped := toolOutcomeUnknownError{cause: context.DeadlineExceeded}
	assert.True(t, wrapped.OutcomeUnknown())
	assert.ErrorIs(t, wrapped, context.DeadlineExceeded)
	assert.False(t, errors.Is(wrapped, context.Canceled))
}
