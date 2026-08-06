package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// RunClaim represents a successfully acquired run slot. It carries a unique ID
// and the time the slot was claimed.
type RunClaim struct {
	// ID uniquely identifies this claim.
	ID string
	// AcquiredAt records when the slot was claimed.
	AcquiredAt time.Time
}

// RunSlotGuard ensures only one primary model run executes at a time. Callers
// claim the slot before running and release it when done.
type RunSlotGuard interface {
	// ClaimRun acquires the run slot, blocking until the slot is free or the
	// context is canceled. It returns a RunClaim identifying the holder.
	ClaimRun(ctx context.Context) (RunClaim, error)
	// ExecuteClaimedRun validates the claim, executes fn, and releases the
	// slot. The slot is released even if fn returns an error or panics.
	ExecuteClaimedRun(claim RunClaim, fn func() error) error
	// Release releases the slot held by the claim. It is a no-op if the claim
	// does not match the current holder.
	Release(claim RunClaim)
}

// DefaultRunSlotGuard is a thread-safe RunSlotGuard using a channel-based
// semaphore of capacity 1 so that at most one run is active at a time.
type DefaultRunSlotGuard struct {
	sem     chan struct{}
	mu      sync.Mutex
	current *RunClaim
}

var _ RunSlotGuard = (*DefaultRunSlotGuard)(nil)

// NewDefaultRunSlotGuard returns a DefaultRunSlotGuard ready to issue claims.
func NewDefaultRunSlotGuard() *DefaultRunSlotGuard {
	return &DefaultRunSlotGuard{
		sem: make(chan struct{}, 1),
	}
}

// ClaimRun acquires the single run slot. It blocks until the slot is available
// or the context is canceled.
func (g *DefaultRunSlotGuard) ClaimRun(ctx context.Context) (RunClaim, error) {
	select {
	case g.sem <- struct{}{}:
		// Slot acquired.
	case <-ctx.Done():
		slog.Debug("core.run_slot.claim_canceled", "err", ctx.Err())
		return RunClaim{}, fmt.Errorf("run_slot: claim canceled: %w", ctx.Err())
	}

	claim := RunClaim{
		ID:         newClaimID(),
		AcquiredAt: time.Now(),
	}
	g.mu.Lock()
	g.current = &claim
	g.mu.Unlock()

	slog.Info("core.run_slot.claimed", "id", claim.ID)
	return claim, nil
}

// ExecuteClaimedRun validates that the claim is the current holder, executes
// fn, and releases the slot. The slot is always released via defer so a panic
// in fn does not leak the slot.
func (g *DefaultRunSlotGuard) ExecuteClaimedRun(claim RunClaim, fn func() error) error {
	g.mu.Lock()
	if g.current == nil || g.current.ID != claim.ID {
		g.mu.Unlock()
		return fmt.Errorf("run_slot: invalid or stale claim %q", claim.ID)
	}
	g.mu.Unlock()

	defer g.Release(claim)
	slog.Debug("core.run_slot.execute", "id", claim.ID)
	return fn()
}

// Release releases the slot held by the claim. If the claim does not match the
// current holder it is a no-op (logged at debug level).
func (g *DefaultRunSlotGuard) Release(claim RunClaim) {
	g.mu.Lock()
	if g.current == nil || g.current.ID != claim.ID {
		g.mu.Unlock()
		slog.Debug("core.run_slot.release_noop", "id", claim.ID)
		return
	}
	g.current = nil
	g.mu.Unlock()

	// Drain the semaphore token to free the slot.
	select {
	case <-g.sem:
	default:
	}
	slog.Info("core.run_slot.released", "id", claim.ID)
}

// newClaimID generates a unique random hex identifier for a claim.
func newClaimID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a timestamp-based ID if crypto/rand fails.
		return fmt.Sprintf("claim-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
