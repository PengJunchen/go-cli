package core

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type runOwnerCompletionTestAPI interface {
	RegisterCompletionTask(ActionIntent, tools.PreparedCompletionSignal) uint64
	HandoffCompletionTask(uint64)
	finishCompletionTaskHandoff(uint64, <-chan struct{}, bool, tools.PreparedCompletionProof)
	completionTaskSnapshot(uint64) (runCompletionTaskState, tools.PreparedCompletionProof, bool)
	waitForCompletionTasks(context.Context, []uint64) (bool, error)
	CleanupCommitContext(context.Context) context.Context
	markRunCommitTerminalSynced()
	runCommitTerminalSynced() bool
}

type runCommitLeaseTestSink struct {
	closeCalls atomic.Int32
	panicClose bool
}

func (sink *runCommitLeaseTestSink) CommitInput(context.Context, RunCommitInput) error { return nil }
func (sink *runCommitLeaseTestSink) BeginIntentBatch(context.Context, RunIntentBatch) error {
	return nil
}
func (sink *runCommitLeaseTestSink) RecordExecution(context.Context, ActionExecution) error {
	return nil
}
func (sink *runCommitLeaseTestSink) RecordReceipt(context.Context, RunToolReceipt) error { return nil }
func (sink *runCommitLeaseTestSink) ResolveExecution(context.Context, ExecutionResolution) error {
	return nil
}
func (sink *runCommitLeaseTestSink) CommitCompaction(context.Context, CompactionProjection) error {
	return nil
}
func (sink *runCommitLeaseTestSink) CommitTerminal(context.Context, RunTerminal) error { return nil }
func (sink *runCommitLeaseTestSink) CloseRunCommit() {
	sink.closeCalls.Add(1)
	if sink.panicClose {
		panic("private close failure")
	}
}

func completionTestAPI(t *testing.T, owner *runOwner) runOwnerCompletionTestAPI {
	t.Helper()
	api, ok := any(owner).(runOwnerCompletionTestAPI)
	require.True(t, ok, "runOwner should own prepared completion tasks and cancellation bookkeeping")
	return api
}

func completionTestIntent(ctx context.Context, owner *runOwner, suffix string) ActionIntent {
	return ActionIntent{
		SessionID:                  "session-test",
		WorkspaceID:                "workspace-test",
		RunID:                      RunIDFromContext(ctx),
		ActionBatchID:              "batch-test",
		OwnerGeneration:            owner.ownerGeneration,
		AssistantTranscriptOrdinal: 1,
		ToolCallID:                 "call-" + suffix,
		ActionID:                   "action-" + suffix,
	}
}

func bindTestRunCommitSink(owner *runOwner, sink RunCommitSink) {
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{
		SessionID:   "session-test",
		WorkspaceID: "workspace-test",
	})
}

func TestRunOwnerCompletionTaskHandoffKeepsPendingResourceOwned(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	resourceDone := make(chan struct{})
	started := make(chan struct{})
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(20*time.Millisecond, func(ctx context.Context) (Result, error) {
			api := completionTestAPI(t, owner)
			taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, "handoff"), tools.PreparedCompletionSignal{})
			require.NotZero(t, taskID)
			assert.Equal(t, 1, owner.Snapshot().PendingWorkers, "the placeholder itself holds worker ownership")
			close(started)
			api.finishCompletionTaskHandoff(taskID, resourceDone, false, "")
			assert.Equal(t, 1, owner.Snapshot().PendingWorkers, "handoff transfers the worker count to the open resource atomically")
			return Result{}, nil
		})
	}()
	<-started
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("owner did not return its bounded cleanup observation")
	}
	require.ErrorIs(t, runErr, errRunCleanupPending)
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	assert.Equal(t, 1, result.StatusHandle.Snapshot().PendingWorkers)
	assert.Zero(t, guard.releaseCount(), "an open completion resource keeps the run slot owned")

	close(resourceDone)
	waitForOwnerState(t, result.StatusHandle, RunExecutionQuiescent)
	assert.Zero(t, result.StatusHandle.Snapshot().PendingWorkers)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerCompletionTaskDistinguishesNoProofFromEmptyProof(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	var noProofID, emptyProofID uint64
	result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		api := completionTestAPI(t, owner)
		wrongOwner := completionTestIntent(ctx, owner, "wrong-owner")
		wrongOwner.OwnerGeneration = "stale-owner"
		assert.Zero(t, api.RegisterCompletionTask(wrongOwner, tools.PreparedCompletionSignal{}), "a completion task cannot cross run-owner generations")
		assert.Zero(t, owner.Snapshot().PendingWorkers)
		noProofID = api.RegisterCompletionTask(completionTestIntent(ctx, owner, "nil"), tools.PreparedCompletionSignal{})
		api.HandoffCompletionTask(noProofID)
		state, proof, ok := api.completionTaskSnapshot(noProofID)
		require.True(t, ok)
		assert.Equal(t, runCompletionTaskNoProof, state, "nil after final handoff is not an already-closed signal")
		assert.Empty(t, proof)

		emptyProofID = api.RegisterCompletionTask(completionTestIntent(ctx, owner, "empty"), tools.PreparedCompletionSignal{})
		closed := make(chan struct{})
		close(closed)
		api.finishCompletionTaskHandoff(emptyProofID, closed, true, "")
		state, proof, ok = api.completionTaskSnapshot(emptyProofID)
		require.True(t, ok)
		assert.Equal(t, runCompletionTaskReady, state, "a non-nil closed signal proves resource join even when its proof enum is empty")
		assert.Empty(t, proof, "resource join is distinct from trusted completion evidence")
		assert.Zero(t, owner.Snapshot().PendingWorkers)
		return Result{}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, RunOutcomeSuccess, result.Outcome)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerPreparedSignalCanBeRegisteredBeforeDoneIsAttached(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	started := make(chan struct{})
	var registeredProof tools.PreparedCompletionProof
	result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		invocation, closeQueue := prepareCompletionTestWrite(ctx, t)
		defer closeQueue()
		signal := invocation.CompletionSignal()
		assert.Nil(t, signal.Done(), "Prepare returns the fixed handle before Dispatch attaches Done")
		api := completionTestAPI(t, owner)
		taskID := registerRunCompletionTask(ctx, completionTestIntent(ctx, owner, "dynamic"), signal)
		require.NotZero(t, taskID)
		assert.Equal(t, 1, owner.Snapshot().PendingWorkers)
		close(started)
		_, dispatchErr := invocation.Dispatch(ctx)
		require.NoError(t, dispatchErr)
		assert.True(t, invocation.DispatchCommitted())
		assert.NotNil(t, signal.Done(), "Dispatch attaches the final channel to the same prepared handle")
		select {
		case <-signal.Done():
		default:
			t.Fatal("the synchronous prepared write should have joined before Dispatch returned")
		}
		registeredProof = signal.Proof()
		require.NotEmpty(t, registeredProof)
		handoffRunCompletionTask(ctx, taskID)
		state, proof, ok := api.completionTaskSnapshot(taskID)
		require.True(t, ok)
		assert.Equal(t, runCompletionTaskReady, state)
		assert.Equal(t, registeredProof, proof)
		return Result{}, nil
	})
	<-started
	require.NoError(t, err)
	assert.Equal(t, tools.CompletionQueuedJobJoined, registeredProof)
	assert.Equal(t, RunExecutionQuiescent, result.ExecutionState)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerWaitRechecksExactSignalAfterCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	resourceDone := make(chan struct{})
	waiting := make(chan struct{})
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(25*time.Millisecond, func(ctx context.Context) (Result, error) {
			api := completionTestAPI(t, owner)
			taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, "cancel-open"), tools.PreparedCompletionSignal{})
			api.finishCompletionTaskHandoff(taskID, resourceDone, false, "")
			close(waiting)
			ready, waitErr := api.waitForCompletionTasks(ctx, []uint64{taskID})
			assert.False(t, ready)
			assert.ErrorIs(t, waitErr, context.Canceled, "still-open Done stops the bounded wait")
			return Result{Outcome: RunOutcomeUnknown}, waitErr
		})
	}()
	<-waiting
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("canceled completion wait did not return within the existing owner bound")
	}
	require.ErrorIs(t, runErr, context.Canceled)
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	assert.Equal(t, 1, result.StatusHandle.Snapshot().PendingWorkers)
	assert.Zero(t, guard.releaseCount(), "cancellation cannot release an open resource")

	close(resourceDone)
	waitForOwnerState(t, result.StatusHandle, RunExecutionQuiescent)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerWaitAcceptsDoneClosedAtCancellationBoundary(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		api := completionTestAPI(t, owner)
		taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, "cancel-boundary"), tools.PreparedCompletionSignal{})
		done := make(chan struct{})
		api.finishCompletionTaskHandoff(taskID, done, true, tools.CompletionQueuedJobJoined)
		close(done)
		cancel()
		ready, waitErr := api.waitForCompletionTasks(ctx, []uint64{taskID})
		assert.NoError(t, waitErr, "a signal already closed at the cancellation boundary wins the select race")
		assert.True(t, ready, "the observed proof remains available after cancellation")
		api.HandoffCompletionTask(taskID)
		return Result{}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, RunExecutionQuiescent, result.ExecutionState)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerCleanupCommitContextSharesDeadlineAndBinding(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	sink := &runCommitLeaseTestSink{}
	bindTestRunCommitSink(owner, sink)
	var cleanupContext context.Context
	result, err := owner.run(200*time.Millisecond, func(ctx context.Context) (Result, error) {
		api := completionTestAPI(t, owner)
		assert.Same(t, ctx, api.CleanupCommitContext(ctx), "live callback uses its original context")
		cancel()
		cleanupContext = api.CleanupCommitContext(ctx)
		require.NotNil(t, cleanupContext)
		require.NoError(t, cleanupContext.Err(), "cleanup context drops cancellation for bookkeeping")
		assert.Equal(t, RunIDFromContext(ctx), RunIDFromContext(cleanupContext))
		gotSink, gotBinding, ok := RunCommitFromContext(cleanupContext)
		require.True(t, ok)
		assert.Same(t, sink, gotSink)
		assert.Equal(t, "session-test", gotBinding.SessionID)
		assert.Equal(t, "workspace-test", gotBinding.WorkspaceID)
		assert.Equal(t, owner.ownerGeneration, gotBinding.OwnerGeneration)
		deadline, ok := cleanupContext.Deadline()
		require.True(t, ok, "cleanup bookkeeping is bounded by the existing per-run timeout")
		time.Sleep(5 * time.Millisecond)
		again := api.CleanupCommitContext(ctx)
		require.Same(t, cleanupContext, again, "all cleanup writes share one context")
		deadlineAgain, ok := again.Deadline()
		require.True(t, ok)
		assert.Equal(t, deadline, deadlineAgain, "later calls cannot extend the cleanup budget")
		api.markRunCommitTerminalSynced()
		return Result{}, ctx.Err()
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, RunOutcomeCancelled, result.Outcome)
	assert.ErrorIs(t, cleanupContext.Err(), context.Canceled, "the owner cancels the cleanup context after local join")
}

func TestRunOwnerReturnsPoisonedSinkLeaseOnlyAfterResourceJoin(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	sink := &runCommitLeaseTestSink{}
	bindTestRunCommitSink(owner, sink)
	resourceDone := make(chan struct{})
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(20*time.Millisecond, func(ctx context.Context) (Result, error) {
			api := completionTestAPI(t, owner)
			taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, "poison"), tools.PreparedCompletionSignal{})
			api.finishCompletionTaskHandoff(taskID, resourceDone, false, "")
			assert.False(t, api.runCommitTerminalSynced(), "a failed terminal write cannot authorize a late proof writer")
			return Result{}, errors.New("poisoned durable writer")
		})
	}()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("owner did not return cleanup_pending while the resource stayed open")
	}
	require.ErrorContains(t, runErr, "poisoned durable writer")
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	assert.Zero(t, sink.closeCalls.Load(), "lease remains held while a resource is live")
	assert.Zero(t, guard.releaseCount())

	close(resourceDone)
	select {
	case <-owner.done:
	case <-time.After(time.Second):
		t.Fatal("owner did not release the joined run after the poisoned sink result")
	}
	assert.Equal(t, RunOutcomeUnknown, result.StatusHandle.Snapshot().Outcome)
	assert.False(t, result.StatusHandle.Snapshot().PendingWorkers > 0)
	assert.Equal(t, RunExecutionQuarantined, result.StatusHandle.Snapshot().ExecutionState, "poisoned terminal persistence never becomes a successful durable ACK")
	assert.EqualValues(t, 1, sink.closeCalls.Load(), "poisoned writer still releases its per-run lease once after join")
	assert.Equal(t, 1, guard.releaseCount(), "the local run slot is released after resource join")
}

func TestRunOwnerQuarantinesPanickedLeaseReleaseWithoutRetrying(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	sink := &runCommitLeaseTestSink{panicClose: true}
	bindTestRunCommitSink(owner, sink)
	result, err := owner.run(time.Second, func(context.Context) (Result, error) {
		return Result{Message: "committed"}, nil
	})

	require.ErrorIs(t, err, ErrRunCommitLeaseClosePanicked)
	assert.Equal(t, RunExecutionQuarantined, result.ExecutionState)
	assert.EqualValues(t, 1, sink.closeCalls.Load(), "an uncertain panic is not retried")
	assert.Equal(t, 1, guard.releaseCount(), "lease panic cannot crash the supervisor or leak the local claim")
}

func TestRunOwnerWithBoundSinkCannotAcknowledgeUnsyncedTerminal(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	sink := &runCommitLeaseTestSink{}
	bindTestRunCommitSink(owner, sink)
	result, err := owner.run(time.Second, func(context.Context) (Result, error) {
		return Result{Outcome: RunOutcomeSuccess, Message: "not durably terminal"}, nil
	})

	require.ErrorIs(t, err, ErrRunCommitTerminalNotSynced)
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	assert.False(t, result.Success, "a bound run cannot ACK success before CommitTerminal sync")
	assert.Equal(t, RunExecutionQuarantined, result.ExecutionState)
	assert.EqualValues(t, 1, sink.closeCalls.Load(), "durable failure still returns the joined run lease")
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerPanicStillHandsOffTaskAndReleasesAfterJoin(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	sink := &runCommitLeaseTestSink{}
	bindTestRunCommitSink(owner, sink)
	resourceDone := make(chan struct{})
	result, err := owner.run(20*time.Millisecond, func(ctx context.Context) (Result, error) {
		api := completionTestAPI(t, owner)
		taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, "panic"), tools.PreparedCompletionSignal{})
		require.NotZero(t, taskID)
		defer api.finishCompletionTaskHandoff(taskID, resourceDone, false, "")
		panic("untrusted panic value")
	})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "untrusted panic value")
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	assert.Equal(t, 1, result.StatusHandle.Snapshot().PendingWorkers)
	assert.Zero(t, sink.closeCalls.Load(), "the status update cannot release a live completion resource")
	assert.Zero(t, guard.releaseCount())
	close(resourceDone)
	select {
	case <-owner.done:
	case <-time.After(time.Second):
		t.Fatal("panicked callback did not release after its resource joined")
	}
	assert.Equal(t, RunExecutionQuarantined, result.StatusHandle.Snapshot().ExecutionState)
	assert.Zero(t, result.StatusHandle.Snapshot().PendingWorkers)
	assert.EqualValues(t, 1, sink.closeCalls.Load(), "deferred handoff precedes one lease release after the join")
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerMarksDurableTerminalSyncOnlyAfterExplicitSuccess(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	sink := &runCommitLeaseTestSink{}
	bindTestRunCommitSink(owner, sink)
	result, err := owner.run(time.Second, func(ctx context.Context) (Result, error) {
		api := completionTestAPI(t, owner)
		assert.False(t, api.runCommitTerminalSynced(), "a bound sink starts without a terminal-sync success fact")
		api.markRunCommitTerminalSynced()
		assert.True(t, api.runCommitTerminalSynced())
		return Result{}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, RunOutcomeSuccess, result.Outcome)
	assert.EqualValues(t, 1, sink.closeCalls.Load())
}

func prepareCompletionTestWrite(ctx context.Context, t *testing.T) (tools.PreparedInvocation, func()) {
	t.Helper()
	root := t.TempDir()
	base, ok := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	require.True(t, ok)
	require.NoError(t, tools.RegisterDefaults(ctx, base,
		tools.WithRegisteredBuiltinWhitelist([]string{"write"}),
		tools.WithRegisteredPathWhitelist([]string{root}),
		tools.WithRegisteredDiffGenerator(tools.NewUnifiedDiffGenerator(0, false)),
	))
	registry := tools.NewMiddlewareToolRegistry(base)
	queue, ok := tools.NewDefaultFileMutationQueue().(*tools.DefaultFileMutationQueue)
	require.True(t, ok)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{MutationQueue: queue}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{
		ID:   "call-dynamic",
		Name: "write",
		Args: map[string]any{
			"path":    filepath.Join(root, "completion.txt"),
			"content": "proof attached after prepare",
		},
	})
	require.NoError(t, err)
	return invocation, func() { queue.Close() }
}
