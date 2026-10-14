package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type leaseHandoffRunSlot struct {
	mu          sync.Mutex
	events      []string
	claimErr    error
	registerErr error
	statusErr   error
}

func (g *leaseHandoffRunSlot) record(event string) {
	g.mu.Lock()
	g.events = append(g.events, event)
	g.mu.Unlock()
}

func (g *leaseHandoffRunSlot) eventIndex(event string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, current := range g.events {
		if current == event {
			return i
		}
	}
	return -1
}

func (g *leaseHandoffRunSlot) ClaimRun(context.Context) (RunClaim, error) {
	g.record("claim")
	if g.claimErr != nil {
		return RunClaim{}, g.claimErr
	}
	return RunClaim{ID: "lease-handoff-claim"}, nil
}

func (*leaseHandoffRunSlot) ExecuteClaimedRun(_ RunClaim, fn func() error) error { return fn() }

func (g *leaseHandoffRunSlot) Release(RunClaim) { g.record("claim-release") }

func (g *leaseHandoffRunSlot) RegisterRunWrapper(RunClaim, RunStatusHandle) (func(), error) {
	g.record("wrapper-register")
	if g.registerErr != nil {
		return nil, g.registerErr
	}
	return func() { g.record("wrapper-done") }, nil
}

func (g *leaseHandoffRunSlot) SetRunWrapperStatus(RunClaim, RunStatusHandle) error {
	g.record("wrapper-status")
	return g.statusErr
}

func (*leaseHandoffRunSlot) ActiveRunStatus() (RunStatus, bool) { return RunStatus{}, false }

type leaseHandoffSink struct {
	*runCommitLeaseTestSink
	slot *leaseHandoffRunSlot
}

func newLeaseHandoffSink(slot *leaseHandoffRunSlot) *leaseHandoffSink {
	return &leaseHandoffSink{runCommitLeaseTestSink: &runCommitLeaseTestSink{}, slot: slot}
}

func (sink *leaseHandoffSink) CloseRunCommit() {
	if sink.slot != nil {
		sink.slot.record("lease-close")
	}
	sink.runCommitLeaseTestSink.CloseRunCommit()
}

func leaseHandoffContext(sink RunCommitSink) context.Context {
	return WithRunCommit(context.Background(), sink, RunCommitBinding{
		SessionID:   "lease-handoff-session",
		WorkspaceID: "workspace-v1-lease-handoff",
	})
}

func TestTurnRunnerNilLoopReturnsLeaseAndPreservesLeasePanic(t *testing.T) {
	t.Run("normal release", func(t *testing.T) {
		sink := newLeaseHandoffSink(nil)
		result, err := NewEinoTurnRunner(nil).RunTurn(leaseHandoffContext(sink), Submission{Content: "hello"})
		require.ErrorIs(t, err, errNilModel)
		require.Empty(t, result.RunID, "pre-owner rejection must not invent a run identity")
		require.EqualValues(t, 1, sink.closeCalls.Load())
	})

	t.Run("release panic is typed and joined", func(t *testing.T) {
		sink := newLeaseHandoffSink(nil)
		sink.panicClose = true
		result, err := NewEinoTurnRunner(nil).RunTurn(leaseHandoffContext(sink), Submission{Content: "hello"})
		require.ErrorIs(t, err, errNilModel)
		require.ErrorIs(t, err, ErrRunCommitLeaseClosePanicked)
		require.Empty(t, result.RunID)
		require.EqualValues(t, 1, sink.closeCalls.Load())
	})
}

func TestEntrypointsReturnLeaseAfterClaimFailure(t *testing.T) {
	claimErr := errors.New("claim rejected")

	t.Run("harness", func(t *testing.T) {
		slot := &leaseHandoffRunSlot{claimErr: claimErr}
		sink := newLeaseHandoffSink(slot)
		harness := NewHarnessImpl(&runCommitCapturingAgent{}, WithRunSlotGuard(slot))
		stream, err := harness.Submit(leaseHandoffContext(sink), "hello")
		require.Nil(t, stream)
		require.ErrorIs(t, err, claimErr)
		require.EqualValues(t, 1, sink.closeCalls.Load())
		require.Equal(t, slot.eventIndex("claim")+1, slot.eventIndex("lease-close"))
	})

	t.Run("turn runner", func(t *testing.T) {
		slot := &leaseHandoffRunSlot{claimErr: claimErr}
		sink := newLeaseHandoffSink(slot)
		runner := NewEinoTurnRunner(&stubLoop{})
		runner.SetRunSlotGuard(slot)
		result, err := runner.RunTurn(leaseHandoffContext(sink), Submission{Content: "hello"})
		require.ErrorIs(t, err, claimErr)
		require.EqualValues(t, 1, sink.closeCalls.Load())
		require.Equal(t, slot.eventIndex("claim")+1, slot.eventIndex("lease-close"))
		require.Empty(t, result.RunID)
	})

	t.Run("lease close panic preserves claim error", func(t *testing.T) {
		slot := &leaseHandoffRunSlot{claimErr: claimErr}
		sink := newLeaseHandoffSink(slot)
		sink.panicClose = true
		harness := NewHarnessImpl(&runCommitCapturingAgent{}, WithRunSlotGuard(slot))
		stream, err := harness.Submit(leaseHandoffContext(sink), "hello")
		require.Nil(t, stream)
		require.ErrorIs(t, err, claimErr)
		require.ErrorIs(t, err, ErrRunCommitLeaseClosePanicked)
		require.EqualValues(t, 1, sink.closeCalls.Load())
	})
}

func TestEntrypointsReleaseLeaseAfterWrapperAndClaimCleanup(t *testing.T) {
	for _, stage := range []string{"wrapper registration", "wrapper status"} {
		for _, entrypoint := range []string{"harness", "turn runner"} {
			t.Run(entrypoint+"/"+stage, func(t *testing.T) {
				stageErr := errors.New(stage + " rejected")
				slot := &leaseHandoffRunSlot{}
				if stage == "wrapper registration" {
					slot.registerErr = stageErr
				} else {
					slot.statusErr = stageErr
				}
				sink := newLeaseHandoffSink(slot)
				if entrypoint == "harness" {
					harness := NewHarnessImpl(&runCommitCapturingAgent{}, WithRunSlotGuard(slot))
					stream, err := harness.Submit(leaseHandoffContext(sink), "hello")
					require.Nil(t, stream)
					require.ErrorIs(t, err, stageErr)
				} else {
					runner := NewEinoTurnRunner(&stubLoop{})
					runner.SetRunSlotGuard(slot)
					_, err := runner.RunTurn(leaseHandoffContext(sink), Submission{Content: "hello"})
					require.ErrorIs(t, err, stageErr)
				}
				require.EqualValues(t, 1, sink.closeCalls.Load())
				closeAt := slot.eventIndex("lease-close")
				if slot.eventIndex("wrapper-done") >= 0 {
					require.Greater(t, closeAt, slot.eventIndex("wrapper-done"), "lease release must follow ordinary wrapper cleanup")
				}
				require.Greater(t, closeAt, slot.eventIndex("claim-release"), "lease release must follow the claim cleanup")
			})
		}
	}
}

type leaseHandoffBlockingAgent struct {
	started chan struct{}
	release chan struct{}
}

func (*leaseHandoffBlockingAgent) Name() string { return "lease-handoff-blocking-agent" }

func (agent *leaseHandoffBlockingAgent) Run(ctx context.Context, _ Submission, _ ...EventStream) (Result, error) {
	markRunCommitTerminalSynced(ctx)
	close(agent.started)
	<-ctx.Done()
	<-agent.release
	return Result{}, ctx.Err()
}

func TestTurnRunnerTransfersLeaseToOwnerUntilCallbackJoins(t *testing.T) {
	sink := newLeaseHandoffSink(nil)
	slot := NewDefaultRunSlotGuard()
	runner := NewEinoTurnRunner(&stubLoop{})
	runner.SetAgent(&leaseHandoffBlockingAgent{started: make(chan struct{}), release: make(chan struct{})})
	agent := runner.agent.(*leaseHandoffBlockingAgent)
	runner.SetRunSlotGuard(slot)
	runner.SetRunCleanupTimeout(15 * time.Millisecond)

	type outcome struct {
		result Result
		err    error
	}
	returned := make(chan outcome, 1)
	go func() {
		result, err := runner.RunTurn(leaseHandoffContext(sink), Submission{Content: "hello"})
		returned <- outcome{result: result, err: err}
	}()
	select {
	case <-agent.started:
	case <-time.After(time.Second):
		t.Fatal("owner callback did not start")
	}
	require.NoError(t, runner.Cancel(context.Background(), "turn-1"))
	select {
	case got := <-returned:
		require.ErrorIs(t, got.err, errRunCleanupPending)
		require.Equal(t, RunExecutionCleanupPending, got.result.ExecutionState)
	case <-time.After(time.Second):
		t.Fatal("RunTurn did not return its bounded pending observation")
	}
	require.Zero(t, sink.closeCalls.Load(), "a pending live owner still owns its borrowed session")
	close(agent.release)
	select {
	case <-slot.CloseAdmission():
	case <-time.After(time.Second):
		t.Fatal("run owner and entrypoint wrapper did not release after callback join")
	}
	require.EqualValues(t, 1, sink.closeCalls.Load())
}

func TestTurnRunnerOwnerAdoptsEntryLeaseOnce(t *testing.T) {
	sink := newLeaseHandoffSink(nil)
	runner := NewEinoTurnRunner(&stubLoop{})
	runner.SetAgent(&runCommitCapturingAgent{})
	result, err := runner.RunTurn(leaseHandoffContext(sink), Submission{Content: "hello"})
	require.NoError(t, err)
	require.True(t, result.Success)
	require.EqualValues(t, 1, sink.closeCalls.Load(), "entrypoint adoption and runOwner context fallback must not double-release")
}

func TestHarnessTransfersLeaseToOwnerUntilCallbackJoins(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })

	sink := newLeaseHandoffSink(nil)
	closeStarted := make(chan struct{})
	sink.runCommitLeaseTestSink.closeStarted = closeStarted
	slot := NewDefaultRunSlotGuard()
	agent := &leaseHandoffBlockingAgent{started: started, release: release}
	harness := NewHarnessImpl(agent,
		WithRunSlotGuard(slot),
		WithRunCleanupTimeout(15*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = WithRunCommit(ctx, sink, RunCommitBinding{
		SessionID:   "lease-handoff-session",
		WorkspaceID: "workspace-v1-lease-handoff",
	})

	stream, err := harness.Submit(ctx, "hello")
	require.NoError(t, err)
	require.NotNil(t, stream)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("owner callback did not start")
	}

	cancel()
	idle := slot.CloseAdmission()
	drained := make(chan struct{})
	go func() {
		for range stream.Events() {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Second):
		t.Fatal("accepted harness run did not return its bounded pending observation")
	}
	_, streamErr := stream.Result()
	require.ErrorIs(t, streamErr, errRunCleanupPending)
	require.Zero(t, sink.closeCalls.Load(), "a pending live owner still owns its borrowed session")
	select {
	case <-idle:
		t.Fatal("run slot became idle before the accepted callback joined")
	default:
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("run owner did not return its lease after the callback joined")
	}
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("run owner did not release its run slot after the callback joined")
	}
	require.EqualValues(t, 1, sink.closeCalls.Load())
}
