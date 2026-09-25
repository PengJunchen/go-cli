package core

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type runOwnerCompletionTestAPI interface {
	RegisterCompletionTask(ActionIntent, tools.PreparedCompletionSignal) uint64
	AttachPendingExecution(uint64, <-chan struct{}) bool
	HandoffCompletionTask(uint64)
	finishCompletionTaskHandoff(uint64, <-chan struct{}, bool, tools.PreparedCompletionProof)
	completionTaskSnapshot(uint64) (runCompletionTaskState, tools.PreparedCompletionProof, bool)
	waitForCompletionTasks(context.Context, []uint64) (bool, error)
	CleanupCommitContext(context.Context) context.Context
	markRunCommitTerminalSynced()
	runCommitTerminalSynced() bool
	markCompletionReceiptDurable(uint64)
	markCompletionProofExecutionDurable(uint64)
}

type runCommitLeaseTestSink struct {
	closeCalls   atomic.Int32
	panicClose   bool
	closeStarted chan struct{}
	closeRelease <-chan struct{}
	startOnce    sync.Once
}

type runCommitLatePanicTestSink struct {
	*runCommitLeaseTestSink
	panicOperation string
}

func (sink *runCommitLatePanicTestSink) RecordExecution(context.Context, ActionExecution) error {
	if sink.panicOperation == "record_execution" {
		panic("untrusted sink panic payload")
	}
	return nil
}

func (sink *runCommitLatePanicTestSink) ResolveExecution(context.Context, ExecutionResolution) error {
	if sink.panicOperation == "resolve_execution" {
		panic("untrusted sink panic payload")
	}
	return nil
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
	if sink.closeStarted != nil {
		sink.startOnce.Do(func() { close(sink.closeStarted) })
	}
	if sink.closeRelease != nil {
		<-sink.closeRelease
	}
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

func completionTestDone(mode string) (<-chan struct{}, chan struct{}) {
	if mode == "nil" {
		return nil, nil
	}
	done := make(chan struct{})
	if mode == "closed" {
		close(done)
	}
	return done, done
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

func TestRunOwnerCompletionTaskJoinsPreparedAndPendingResourcesAsOneGroup(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		signalMode   string
		pendingSeen  bool
		pendingMode  string
		wantUnproven bool
	}{
		{name: "wrapped distinct channels", signalMode: "open", pendingSeen: true, pendingMode: "open"},
		{name: "closed signal waits for open pending resource", signalMode: "closed", pendingSeen: true, pendingMode: "open"},
		{name: "open signal waits for closed pending resource", signalMode: "open", pendingSeen: true, pendingMode: "closed"},
		{name: "same channel is counted once", signalMode: "open", pendingSeen: true, pendingMode: "same"},
		{name: "nil signal tracks pending resource", signalMode: "nil", pendingSeen: true, pendingMode: "open"},
		{name: "nil pending marks signal unproven", signalMode: "open", pendingSeen: true, pendingMode: "nil", wantUnproven: true},
		{name: "closed signal plus nil pending keeps marker unproven", signalMode: "closed", pendingSeen: true, pendingMode: "nil", wantUnproven: true},
		{name: "nil signal and nil pending has no fake resource", signalMode: "nil", pendingSeen: true, pendingMode: "nil", wantUnproven: true},
		{name: "nil signal without pending error has no fake resource", signalMode: "nil"},
		{name: "both distinct channels already closed", signalMode: "closed", pendingSeen: true, pendingMode: "closed"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			guard := newCountingRunSlotGuard()
			owner := newRunOwner(context.Background(), guard, guard.claim, nil)
			ctx := withRunID(owner.ctx, owner.claim.ID)
			api := completionTestAPI(t, owner)
			signalDone, signalChannel := completionTestDone(testCase.signalMode)
			pendingDone, pendingChannel := completionTestDone(testCase.pendingMode)
			if testCase.pendingMode == "same" {
				pendingDone = signalDone
				pendingChannel = signalChannel
			}
			taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, "group"), tools.PreparedCompletionSignal{})
			require.NotZero(t, taskID)
			if testCase.pendingSeen {
				require.True(t, api.AttachPendingExecution(taskID, pendingDone))
			}
			api.finishCompletionTaskHandoff(taskID, signalDone, true, tools.CompletionInlinePipelineJoined)

			openChannels := make([]chan struct{}, 0, 2)
			if signalChannel != nil && testCase.signalMode == "open" {
				openChannels = append(openChannels, signalChannel)
			}
			if pendingChannel != nil && testCase.pendingMode == "open" && pendingChannel != signalChannel {
				openChannels = append(openChannels, pendingChannel)
			}
			if len(openChannels) > 0 {
				state, _, ok := api.completionTaskSnapshot(taskID)
				require.True(t, ok)
				assert.Equal(t, runCompletionTaskResourceOpen, state, "worker ownership is transferred to one combined resource group")
				assert.Equal(t, 1, owner.Snapshot().PendingWorkers, "two underlying channels still count as one action resource")
				close(openChannels[0])
				if len(openChannels) > 1 {
					time.Sleep(2 * time.Millisecond)
					state, _, _ = api.completionTaskSnapshot(taskID)
					assert.Equal(t, runCompletionTaskResourceOpen, state, "one channel cannot claim the whole group joined")
					assert.Equal(t, 1, owner.Snapshot().PendingWorkers)
					close(openChannels[1])
				}
				require.Eventually(t, func() bool {
					state, _, ok := api.completionTaskSnapshot(taskID)
					return ok && state == runCompletionTaskReady
				}, time.Second, time.Millisecond)
			} else {
				state, _, ok := api.completionTaskSnapshot(taskID)
				require.True(t, ok)
				if testCase.signalMode == "nil" && (!testCase.pendingSeen || testCase.pendingMode == "nil") {
					assert.Equal(t, runCompletionTaskNoProof, state)
				} else {
					assert.Equal(t, runCompletionTaskReady, state)
				}
			}

			details, ok := owner.completionDetails(taskID)
			require.True(t, ok)
			assert.Equal(t, testCase.wantUnproven, details.pendingResourceUnproven)
			if testCase.signalMode == "nil" {
				assert.Empty(t, details.proof, "joining the underlying resource does not manufacture prepared-signal proof")
			} else if details.state == runCompletionTaskReady {
				assert.Equal(t, tools.CompletionInlinePipelineJoined, details.proof)
			}
			wantPending := 0
			if details.state == runCompletionTaskResourceOpen {
				wantPending = 1
			}
			assert.Equal(t, wantPending, owner.Snapshot().PendingWorkers)
		})
	}
}

func TestRunOwnerCompletionTaskCancellationKeepsCombinedGroupOwned(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	owner.setCleanupTimeout(35 * time.Millisecond)
	signalDone := make(chan struct{})
	pendingDone := make(chan struct{})
	registered := make(chan struct{})
	cancelObserved := make(chan struct{})
	allowHandoff := make(chan struct{})
	handedOff := make(chan struct{})
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(35*time.Millisecond, func(ctx context.Context) (Result, error) {
			api := completionTestAPI(t, owner)
			taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, "cancel-group"), tools.PreparedCompletionSignal{})
			require.NotZero(t, taskID)
			require.True(t, api.AttachPendingExecution(taskID, pendingDone))
			close(registered)
			<-ctx.Done()
			close(cancelObserved)
			<-allowHandoff
			assert.Equal(t, 1, owner.Snapshot().PendingWorkers, "the placeholder remains owned while Dispatch worker has not handed off")
			api.finishCompletionTaskHandoff(taskID, signalDone, true, tools.CompletionInlinePipelineJoined)
			assert.Equal(t, 1, owner.Snapshot().PendingWorkers, "worker-to-resource transfer has no quiescent gap")
			close(handedOff)
			return Result{Outcome: RunOutcomeUnknown}, ctx.Err()
		})
	}()
	<-registered
	cancel()
	<-cancelObserved
	assert.Equal(t, 1, owner.Snapshot().PendingWorkers, "cancellation cannot release the in-flight completion placeholder")
	close(allowHandoff)
	<-handedOff
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("bounded owner cleanup did not return while both underlying channels remained open")
	}
	require.ErrorIs(t, runErr, context.Canceled)
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	assert.Equal(t, 1, result.StatusHandle.Snapshot().PendingWorkers)
	assert.Zero(t, guard.releaseCount(), "the combined group still has live resources")

	close(signalDone)
	time.Sleep(2 * time.Millisecond)
	assert.Equal(t, 1, result.StatusHandle.Snapshot().PendingWorkers, "closing the prepared wrapper alone cannot release the underlying execution")
	close(pendingDone)
	select {
	case <-owner.done:
	case <-time.After(time.Second):
		t.Fatal("owner did not release the group after both local resources joined")
	}
	assert.Zero(t, result.StatusHandle.Snapshot().PendingWorkers)
	assert.Equal(t, 1, guard.releaseCount())
}

func TestRunOwnerCompletionWaitRechecksWholeResourceGroup(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	ctx, cancel := context.WithCancel(withRunID(owner.ctx, owner.claim.ID))
	defer cancel()
	api := completionTestAPI(t, owner)
	signalDone := make(chan struct{})
	pendingDone := make(chan struct{})
	taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, "wait-group"), tools.PreparedCompletionSignal{})
	require.NotZero(t, taskID)
	require.True(t, api.AttachPendingExecution(taskID, pendingDone))
	api.finishCompletionTaskHandoff(taskID, signalDone, true, tools.CompletionInlinePipelineJoined)

	close(signalDone)
	assert.Equal(t, runCompletionTaskResourceOpen, func() runCompletionTaskState {
		state, _, _ := api.completionTaskSnapshot(taskID)
		return state
	}())
	assert.Equal(t, 1, owner.Snapshot().PendingWorkers)

	cancel()
	ready, err := api.waitForCompletionTasks(ctx, []uint64{taskID})
	assert.False(t, ready, "cancellation recheck still evaluates the complete channel group")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, owner.Snapshot().PendingWorkers)

	close(pendingDone)
	ready, err = api.waitForCompletionTasks(context.Background(), []uint64{taskID})
	assert.NoError(t, err)
	assert.True(t, ready, "proof is exposed only after both local resources have joined")
	state, proof, ok := api.completionTaskSnapshot(taskID)
	require.True(t, ok)
	assert.Equal(t, runCompletionTaskReady, state)
	assert.Equal(t, tools.CompletionInlinePipelineJoined, proof)
	assert.Zero(t, owner.Snapshot().PendingWorkers)
}

func TestRunOwnerCompletionWaitDoesNotTurnNilPendingHandleIntoProof(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	ctx := withRunID(owner.ctx, owner.claim.ID)
	api := completionTestAPI(t, owner)
	done := make(chan struct{})
	close(done)
	taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, "nil-pending-wait"), tools.PreparedCompletionSignal{})
	require.NotZero(t, taskID)
	require.True(t, api.AttachPendingExecution(taskID, nil))
	api.finishCompletionTaskHandoff(taskID, done, true, tools.CompletionInlinePipelineJoined)

	ready, err := api.waitForCompletionTasks(context.Background(), []uint64{taskID})
	assert.NoError(t, err)
	assert.False(t, ready, "local signal proof cannot override an explicitly nil pending-resource handle")
	state, proof, ok := api.completionTaskSnapshot(taskID)
	require.True(t, ok)
	assert.Equal(t, runCompletionTaskReady, state, "the local channel joined, but its group remains proof-gated")
	assert.Equal(t, tools.CompletionInlinePipelineJoined, proof, "the original signal proof remains inspectable")
	details, ok := owner.completionDetails(taskID)
	require.True(t, ok)
	assert.True(t, details.pendingResourceUnproven)
	assert.Zero(t, owner.Snapshot().PendingWorkers, "nil pending handles do not create fake never-ending resource ownership")
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

func TestRunOwnerEarlyCancelRebindsCleanupContextWithoutExtendingDeadline(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	owner.setCleanupTimeout(120 * time.Millisecond)
	sink := &runCommitLeaseTestSink{}
	bindTestRunCommitSink(owner, sink)
	cancelAt := time.Now()
	owner.MarkCancelRequested()
	cancel()
	time.Sleep(15 * time.Millisecond)

	result, err := owner.run(time.Second, func(context.Context) (Result, error) {
		t.Fatal("a canceled owner must not enter the callback")
		return Result{}, nil
	})
	require.ErrorIs(t, err, context.Canceled)
	api := completionTestAPI(t, owner)
	cleanupCtx := api.CleanupCommitContext(owner.callbackCtx)
	require.NotNil(t, cleanupCtx)
	assert.Equal(t, owner.claim.ID, RunIDFromContext(cleanupCtx), "delayed rebinding restores the run identity")
	gotSink, binding, ok := RunCommitFromContext(cleanupCtx)
	require.True(t, ok, "delayed rebinding restores only the validated atomic sink binding")
	assert.Same(t, sink, gotSink)
	assert.Equal(t, "session-test", binding.SessionID)
	assert.Equal(t, "workspace-test", binding.WorkspaceID)
	assert.Equal(t, owner.ownerGeneration, binding.OwnerGeneration)
	deadline, ok := cleanupCtx.Deadline()
	require.True(t, ok)
	assert.Less(t, deadline, cancelAt.Add(owner.cleanupTimeout+10*time.Millisecond), "late callback-context installation must not restart the first cleanup budget")
	assert.Equal(t, RunOutcomeCancelled, result.Outcome)
}

func TestRunOwnerEarlyCancelDoesNotInventInvalidBinding(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(parent, guard, guard.claim, nil)
	sink := &runCommitLeaseTestSink{}
	owner.ctx = WithRunCommit(owner.ctx, sink, RunCommitBinding{SessionID: "missing-workspace"})
	owner.setCleanupTimeout(100 * time.Millisecond)
	owner.MarkCancelRequested()
	cancel()
	_, err := owner.run(time.Second, func(context.Context) (Result, error) {
		t.Fatal("an invalid bound run must fail before the callback")
		return Result{}, nil
	})
	require.ErrorIs(t, err, ErrInvalidRunCommitBinding)
	cleanupCtx := completionTestAPI(t, owner).CleanupCommitContext(owner.callbackCtx)
	assert.Equal(t, owner.claim.ID, RunIDFromContext(cleanupCtx), "the owner still retains its local run identity")
	gotSink, binding, ok := RunCommitFromContext(cleanupCtx)
	assert.False(t, ok, "an invalid descriptor is cleared instead of becoming a fabricated cleanup binding")
	assert.Nil(t, gotSink)
	assert.Equal(t, RunCommitBinding{}, binding)
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

func TestRunOwnerLateCompletionSinkPanicsAreContained(t *testing.T) {
	for _, operation := range []string{"record_execution", "resolve_execution"} {
		t.Run(operation, func(t *testing.T) {
			guard := newCountingRunSlotGuard()
			owner := newRunOwner(context.Background(), guard, guard.claim, nil)
			sink := &runCommitLatePanicTestSink{
				runCommitLeaseTestSink: &runCommitLeaseTestSink{},
				panicOperation:         operation,
			}
			ctx := withRunID(WithRunCommit(context.Background(), sink, RunCommitBinding{
				SessionID:   "session-test",
				WorkspaceID: "workspace-test",
			}), owner.claim.ID)
			intent := completionTestIntent(ctx, owner, operation)
			taskID := completionTestAPI(t, owner).RegisterCompletionTask(intent, tools.PreparedCompletionSignal{})
			require.NotZero(t, taskID)
			done := make(chan struct{})
			close(done)
			api := completionTestAPI(t, owner)
			api.finishCompletionTaskHandoff(taskID, done, true, tools.CompletionInlinePipelineJoined)
			api.markRunCommitTerminalSynced()
			api.markCompletionReceiptDurable(taskID)
			if operation == "resolve_execution" {
				api.markCompletionProofExecutionDurable(taskID)
			}
			owner.mu.Lock()
			owner.callbackCtx = ctx
			owner.mu.Unlock()

			var drainErr error
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				drainErr = owner.drainLateCompletionTasks()
			}()
			assert.False(t, panicked, "late sink panic must be contained so cleanup can finish")
			require.ErrorIs(t, drainErr, ErrRunCommitSinkPanicked)
			assert.NotContains(t, drainErr.Error(), "untrusted sink panic payload")
		})
	}
}

func TestRunOwnerQuarantinesLateCompletionSinkPanicsAndReleasesAfterJoin(t *testing.T) {
	for _, operation := range []string{"record_execution", "resolve_execution"} {
		t.Run(operation, func(t *testing.T) {
			guard := newCountingRunSlotGuard()
			owner := newRunOwner(context.Background(), guard, guard.claim, nil)
			sink := &runCommitLatePanicTestSink{
				runCommitLeaseTestSink: &runCommitLeaseTestSink{},
				panicOperation:         operation,
			}
			bindTestRunCommitSink(owner, sink)

			result, runErr := owner.run(time.Second, func(ctx context.Context) (Result, error) {
				api := completionTestAPI(t, owner)
				taskID := api.RegisterCompletionTask(completionTestIntent(ctx, owner, operation), tools.PreparedCompletionSignal{})
				require.NotZero(t, taskID)
				done := make(chan struct{})
				close(done)
				api.finishCompletionTaskHandoff(taskID, done, true, tools.CompletionInlinePipelineJoined)
				api.markCompletionReceiptDurable(taskID)
				if operation == "resolve_execution" {
					api.markCompletionProofExecutionDurable(taskID)
				}
				api.markRunCommitTerminalSynced()
				return Result{Outcome: RunOutcomeSuccess, Message: "terminal persisted"}, nil
			})

			require.ErrorIs(t, runErr, ErrRunCommitSinkPanicked)
			assert.NotContains(t, runErr.Error(), "untrusted sink panic payload")
			assert.Equal(t, RunOutcomeUnknown, result.Outcome)
			assert.False(t, result.Success, "a failed late sink write cannot produce a success ACK")
			assert.Equal(t, RunExecutionQuarantined, result.ExecutionState)
			assert.Equal(t, RunExecutionQuarantined, owner.Snapshot().ExecutionState)
			assert.Zero(t, owner.Snapshot().PendingWorkers, "joined completion resources are still released")
			assert.Equal(t, 1, guard.releaseCount(), "the run slot is released once after the completion task joined")
			assert.EqualValues(t, 1, sink.closeCalls.Load(), "the sink lease is released once after the completion task joined")

			details, ok := owner.completionDetails(1)
			require.True(t, ok)
			assert.False(t, details.resolutionDurable, "a panicked resolution cannot clear the unresolved marker")
			if operation == "record_execution" {
				assert.False(t, details.proofExecutionDurable, "a panicked execution append is not acknowledged")
			} else {
				assert.True(t, details.proofExecutionDurable, "the prior proof row remains acknowledged")
			}
		})
	}
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

func TestRunOwnerBlockedLeaseCloseRemainsPendingUntilReleaseCompletes(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	resourceDone := make(chan struct{})
	closeStarted := make(chan struct{})
	closeRelease := make(chan struct{})
	sink := &runCommitLeaseTestSink{closeStarted: closeStarted, closeRelease: closeRelease}
	bindTestRunCommitSink(owner, sink)
	runDone := make(chan struct{})
	var result Result
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = owner.run(40*time.Millisecond, func(ctx context.Context) (Result, error) {
			completionTestAPI(t, owner).markRunCommitTerminalSynced()
			owner.RegisterPendingResource("call-lease", "blocked", resourceDone)
			return Result{Outcome: RunOutcomeSuccess, Message: "terminal synced"}, nil
		})
	}()
	for {
		owner.mu.Lock()
		callbackFinished := owner.callbackFinished
		owner.mu.Unlock()
		if callbackFinished {
			break
		}
		select {
		case <-runDone:
			t.Fatal("run returned before the resource joined")
		case <-time.After(time.Millisecond):
		}
	}
	close(resourceDone)
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not begin the one-shot lease release")
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("bounded caller did not return while lease close remained blocked")
	}
	require.Error(t, runErr, "a blocked release cannot be returned as a successful terminal snapshot")
	assert.Equal(t, RunOutcomeUnknown, result.Outcome)
	assert.False(t, result.Success)
	assert.Equal(t, RunExecutionCleanupPending, result.ExecutionState)
	assert.Zero(t, guard.releaseCount(), "the claim stays owned until the lease release completes")
	assert.EqualValues(t, 1, sink.closeCalls.Load())
	close(closeRelease)
	select {
	case <-owner.done:
	case <-time.After(time.Second):
		t.Fatal("owner did not finish its one-shot release after the lease unblocked")
	}
	assert.Equal(t, 1, guard.releaseCount())
	assert.EqualValues(t, 1, sink.closeCalls.Load(), "the release is never retried")
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
