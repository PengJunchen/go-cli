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
)

func TestDefaultRunSlotGuard_SatisfiesInterface(t *testing.T) {
	var _ RunSlotGuard = (*DefaultRunSlotGuard)(nil)
}

func TestRunSlotGuard_ClaimAndRelease(t *testing.T) {
	g := NewDefaultRunSlotGuard()

	claim, err := g.ClaimRun(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, claim.ID)
	assert.False(t, claim.AcquiredAt.IsZero())

	// A second claim should block; use a timeout context to verify.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = g.ClaimRun(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	// After release, a new claim should succeed.
	g.Release(claim)
	claim2, err := g.ClaimRun(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, claim.ID, claim2.ID)
	g.Release(claim2)
}

func TestRunSlotGuard_ExecuteClaimedRun(t *testing.T) {
	g := NewDefaultRunSlotGuard()

	claim, err := g.ClaimRun(context.Background())
	require.NoError(t, err)

	var executed atomic.Bool
	err = g.ExecuteClaimedRun(claim, func() error {
		executed.Store(true)
		return nil
	})
	require.NoError(t, err)
	assert.True(t, executed.Load(), "fn should have been executed")

	// After ExecuteClaimedRun, the slot should be released (a new claim works).
	claim2, err := g.ClaimRun(context.Background())
	require.NoError(t, err)
	g.Release(claim2)
}

func TestRunSlotGuard_ExecuteClaimedRun_PropagatesError(t *testing.T) {
	g := NewDefaultRunSlotGuard()
	claim, err := g.ClaimRun(context.Background())
	require.NoError(t, err)

	wantErr := errors.New("boom")
	err = g.ExecuteClaimedRun(claim, func() error {
		return wantErr
	})
	assert.ErrorIs(t, err, wantErr)

	// Slot should still be released after an error.
	_, err = g.ClaimRun(context.Background())
	require.NoError(t, err)
}

func TestRunSlotGuard_ExecuteClaimedRun_InvalidClaim(t *testing.T) {
	g := NewDefaultRunSlotGuard()

	stale := RunClaim{ID: "nonexistent", AcquiredAt: time.Now()}
	err := g.ExecuteClaimedRun(stale, func() error {
		t.Fatal("fn should not run for an invalid claim")
		return nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid or stale claim")
}

func TestRunSlotGuard_Release_InvalidClaim_NoOp(t *testing.T) {
	g := NewDefaultRunSlotGuard()
	claim, err := g.ClaimRun(context.Background())
	require.NoError(t, err)

	// Releasing a wrong claim should be a no-op.
	g.Release(RunClaim{ID: "wrong"})
	// The slot should still be held.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = g.ClaimRun(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)

	g.Release(claim)
}

func TestRunSlotGuard_ClaimRun_CancelledContext(t *testing.T) {
	g := NewDefaultRunSlotGuard()
	claim, err := g.ClaimRun(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	_, err = g.ClaimRun(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "canceled")

	g.Release(claim)
}

func TestRunSlotGuard_AlreadyCanceledContextNeverGetsClaim(t *testing.T) {
	g := NewDefaultRunSlotGuard()
	for attempt := 0; attempt < 100; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		claim, err := g.ClaimRun(ctx)
		if err == nil {
			g.Release(claim)
		}
		require.ErrorIs(t, err, context.Canceled, "attempt %d", attempt)
	}
}

type cancelAfterFirstErrCheckContext struct {
	context.Context
	done  chan struct{}
	calls atomic.Int32
	once  sync.Once
}

func newCancelAfterFirstErrCheckContext() *cancelAfterFirstErrCheckContext {
	return &cancelAfterFirstErrCheckContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *cancelAfterFirstErrCheckContext) Done() <-chan struct{} { return c.done }

func (c *cancelAfterFirstErrCheckContext) Err() error {
	if c.calls.Add(1) < 2 {
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.Canceled
}

func TestRunSlotGuard_ClaimRunCancellationBeforeClaimPublicationLeavesSlotAvailable(t *testing.T) {
	g := NewDefaultRunSlotGuard()
	ctx := newCancelAfterFirstErrCheckContext()
	_, err := g.ClaimRun(ctx)
	require.ErrorIs(t, err, context.Canceled)

	claim, err := g.ClaimRun(context.Background())
	require.NoError(t, err, "a canceled claim must leave the run slot available")
	g.Release(claim)
}

func TestRunSlotGuard_ConcurrentAccess(t *testing.T) {
	g := NewDefaultRunSlotGuard()
	var wg sync.WaitGroup
	var successCount atomic.Int32

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			claim, err := g.ClaimRun(ctx)
			if err != nil {
				return
			}
			successCount.Add(1)
			time.Sleep(5 * time.Millisecond)
			g.Release(claim)
		}()
	}
	wg.Wait()

	// All goroutines should have acquired the slot at some point.
	assert.Equal(t, int32(10), successCount.Load(), "all goroutines should have claimed the slot")
}

func TestRunSlotGuard_ExecuteClaimedRun_ReleasesOnPanic(t *testing.T) {
	g := NewDefaultRunSlotGuard()
	claim, err := g.ClaimRun(context.Background())
	require.NoError(t, err)

	assert.Panics(t, func() {
		_ = g.ExecuteClaimedRun(claim, func() error { //nolint:errcheck
			panic("kaboom")
		})
	})

	// The slot should be released even after a panic.
	claim2, err := g.ClaimRun(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, claim.ID, claim2.ID)
	g.Release(claim2)
}

type runSlotWaitObservedContext struct {
	context.Context
	done         chan struct{}
	waitObserved chan struct{}
	waitOnce     sync.Once
}

func (c *runSlotWaitObservedContext) Done() <-chan struct{} {
	c.waitOnce.Do(func() { close(c.waitObserved) })
	return c.done
}

func TestDefaultRunSlotGuardCloseWakesBlockedClaim(t *testing.T) {
	g := NewDefaultRunSlotGuard()
	active, err := g.ClaimRun(context.Background())
	require.NoError(t, err)

	ctx := &runSlotWaitObservedContext{
		Context:      context.Background(),
		done:         make(chan struct{}),
		waitObserved: make(chan struct{}),
	}
	claimResult := make(chan error, 1)
	go func() {
		_, claimErr := g.ClaimRun(ctx)
		claimResult <- claimErr
	}()

	select {
	case <-ctx.waitObserved:
	case <-time.After(time.Second):
		t.Fatal("second claim did not reach the wait state")
	}

	idle := g.CloseAdmission()
	select {
	case claimErr := <-claimResult:
		require.ErrorIs(t, claimErr, ErrRunAdmissionClosed)
	case <-time.After(time.Second):
		t.Fatal("closing admission did not wake the blocked claim")
	}

	select {
	case <-idle:
		t.Fatal("idle signal closed while the original claim was still active")
	default:
	}

	g.Release(active)
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("idle signal did not close after the real claim release")
	}
	require.Equal(t, idle, g.CloseAdmission(), "repeated close must return the same idle signal")
}

func TestDefaultRunSlotGuardCloseIsStableWhenIdle(t *testing.T) {
	g := NewDefaultRunSlotGuard()
	idle := g.CloseAdmission()

	select {
	case <-idle:
	default:
		t.Fatal("closing an idle guard must close the idle signal immediately")
	}
	require.Equal(t, idle, g.CloseAdmission(), "repeated close must return the same idle signal")

	_, err := g.ClaimRun(context.Background())
	require.ErrorIs(t, err, ErrRunAdmissionClosed)
}

func TestDefaultRunSlotGuardCloseRacesClaimAndRelease(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		g := NewDefaultRunSlotGuard()
		start := make(chan struct{})
		type claimResult struct {
			claim RunClaim
			err   error
		}
		claimed := make(chan claimResult, 1)
		idleResult := make(chan (<-chan struct{}), 1)
		go func() {
			<-start
			claim, err := g.ClaimRun(context.Background())
			claimed <- claimResult{claim: claim, err: err}
		}()
		go func() {
			<-start
			idleResult <- g.CloseAdmission()
		}()
		close(start)

		result := <-claimed
		idle := <-idleResult
		if result.err == nil {
			select {
			case <-idle:
				t.Fatalf("attempt %d: accepted claim was not retained until release", attempt)
			default:
			}
			g.Release(result.claim)
		} else {
			require.ErrorIs(t, result.err, ErrRunAdmissionClosed, "attempt %d", attempt)
		}
		select {
		case <-idle:
		case <-time.After(time.Second):
			t.Fatalf("attempt %d: guard never became idle", attempt)
		}
		_, err := g.ClaimRun(context.Background())
		require.ErrorIs(t, err, ErrRunAdmissionClosed, "attempt %d: claim succeeded after close", attempt)
	}

	for attempt := 0; attempt < 100; attempt++ {
		g := NewDefaultRunSlotGuard()
		active, err := g.ClaimRun(context.Background())
		require.NoError(t, err)

		start := make(chan struct{})
		released := make(chan struct{})
		idleResult := make(chan (<-chan struct{}), 1)
		go func() {
			<-start
			g.Release(active)
			close(released)
		}()
		go func() {
			<-start
			idleResult <- g.CloseAdmission()
		}()
		close(start)
		<-released
		idle := <-idleResult
		select {
		case <-idle:
		case <-time.After(time.Second):
			t.Fatalf("release/close attempt %d: guard never became idle", attempt)
		}
		_, err = g.ClaimRun(context.Background())
		require.ErrorIs(t, err, ErrRunAdmissionClosed, "release/close attempt %d: claim succeeded after close", attempt)
	}
}
