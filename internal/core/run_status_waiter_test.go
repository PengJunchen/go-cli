package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunStatusWaiterReturnsFreshPublicationAfterCleanupPending(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	owner.setCleanupTimeout(15 * time.Millisecond)
	bindTestRunCommitSink(owner, &runCommitLeaseTestSink{})

	resourceDone := make(chan struct{})
	callbackDone := make(chan struct{})
	var initial Result
	var initialErr error
	go func() {
		defer close(callbackDone)
		initial, initialErr = owner.run(15*time.Millisecond, func(ctx context.Context) (Result, error) {
			registerRunPendingResource(ctx, "call", "tool", resourceDone)
			owner.markRunCommitTerminalSynced()
			return Result{}, nil
		})
	}()

	select {
	case <-callbackDone:
	case <-time.After(time.Second):
		t.Fatal("owner did not return its bounded cleanup-pending result")
	}
	require.ErrorIs(t, initialErr, errRunCleanupPending)
	require.Equal(t, RunExecutionCleanupPending, initial.ExecutionState)
	require.Equal(t, DurablePublicationPending, initial.DurablePublication.State)

	waiter, ok := initial.StatusHandle.(RunStatusWaiter)
	require.True(t, ok, "the result status handle must expose an owner-joined wait")

	type completion struct {
		status      RunStatus
		publication DurablePublication
		err         error
	}
	completed := make(chan completion, 1)
	go func() {
		status, publication, err := waiter.WaitForCompletion(context.Background())
		completed <- completion{status: status, publication: publication, err: err}
	}()

	select {
	case got := <-completed:
		t.Fatalf("waiter reported completion before the resource joined: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}

	close(resourceDone)
	select {
	case got := <-completed:
		require.NoError(t, got.err)
		require.Equal(t, RunExecutionQuiescent, got.status.ExecutionState)
		require.Equal(t, RunOutcomeSuccess, got.status.Outcome)
		require.Equal(t, DurablePublicationSynced, got.publication.State)
		require.Equal(t, owner.claim.ID, got.publication.RunID)
		require.Equal(t, "session-test", got.publication.SessionID)
		require.Equal(t, owner.ownerGeneration, got.publication.OwnerGeneration)
	case <-time.After(time.Second):
		t.Fatal("owner waiter did not observe the quiescent publication")
	}
}

func TestRunStatusWaiterCancellationReturnsCurrentPendingPublication(t *testing.T) {
	guard := newCountingRunSlotGuard()
	owner := newRunOwner(context.Background(), guard, guard.claim, nil)
	bindTestRunCommitSink(owner, &runCommitLeaseTestSink{})

	resourceDone := make(chan struct{})
	callbackReady := make(chan struct{})
	allowCallbackReturn := make(chan struct{})
	runDone := make(chan error, 1)
	go func() {
		_, err := owner.run(50*time.Millisecond, func(ctx context.Context) (Result, error) {
			registerRunPendingResource(ctx, "call", "tool", resourceDone)
			owner.markRunCommitTerminalSynced()
			close(callbackReady)
			<-allowCallbackReturn
			return Result{}, nil
		})
		runDone <- err
	}()
	select {
	case <-callbackReady:
	case <-time.After(time.Second):
		t.Fatal("owner callback did not start")
	}

	waiter := RunStatusWaiter(owner)
	ctx, cancel := context.WithCancel(context.Background())
	waitResult := make(chan struct {
		status      RunStatus
		publication DurablePublication
		err         error
	}, 1)
	go func() {
		status, publication, err := waiter.WaitForCompletion(ctx)
		waitResult <- struct {
			status      RunStatus
			publication DurablePublication
			err         error
		}{status: status, publication: publication, err: err}
	}()
	cancel()
	select {
	case got := <-waitResult:
		require.ErrorIs(t, got.err, context.Canceled)
		require.NotEqual(t, RunExecutionQuiescent, got.status.ExecutionState)
		require.Equal(t, DurablePublicationPending, got.publication.State)
	case <-time.After(time.Second):
		t.Fatal("canceled owner wait did not return a pending snapshot")
	}

	close(allowCallbackReturn)
	select {
	case err := <-runDone:
		require.ErrorIs(t, err, errRunCleanupPending)
	case <-time.After(time.Second):
		t.Fatal("owner did not return a cleanup-pending result")
	}

	close(resourceDone)
	finalCtx, finalCancel := context.WithTimeout(context.Background(), time.Second)
	defer finalCancel()
	status, publication, err := waiter.WaitForCompletion(finalCtx)
	require.NoError(t, err)
	require.Equal(t, RunExecutionQuiescent, status.ExecutionState)
	require.Equal(t, DurablePublicationSynced, publication.State)
}
