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
	assert.Contains(t, err.Error(), "cancelled")

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
		_ = g.ExecuteClaimedRun(claim, func() error {
			panic("kaboom")
		})
	})

	// The slot should be released even after a panic.
	claim2, err := g.ClaimRun(context.Background())
	require.NoError(t, err)
	assert.NotEqual(t, claim.ID, claim2.ID)
	g.Release(claim2)
}
