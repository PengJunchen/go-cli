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

type contextErrorTool struct{ err error }

func (tool contextErrorTool) Name() string        { return "context-error" }
func (tool contextErrorTool) Description() string { return "returns a context error after dispatch" }
func (tool contextErrorTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
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
	cancel()
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	owner.MarkCancelRequested()
	registry := scriptedRegistry(contextErrorTool{err: knownInterruptedError{cause: context.Canceled}})

	result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		_, _, toolErr := executeSingleTool(ctx, registry, tools.ToolCall{ID: "call-1", Name: "context-error"}, nil)
		return Result{}, toolErr
	})

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
	assert.Equal(t, 0, guard.releaseCount())
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
	assert.Equal(t, 0, guard.releaseCount(), "an invalid delta did not commit canonical history")
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
