package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
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
// claim the slot before running; an optional wrapper lease can extend the
// claim through post-owner entrypoint work.
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

// ErrRunAdmissionClosed indicates that a run slot no longer accepts claims.
var ErrRunAdmissionClosed = errors.New("run_slot: admission is closed")

// ErrRunSlotBusy indicates that a non-blocking claim could not acquire the
// active run slot.
var ErrRunSlotBusy = errors.New("run_slot: slot is busy")

// RunSlotTryClaimer is an optional capability for guards that support an
// atomic, non-blocking attempt to claim the run slot.
type RunSlotTryClaimer interface {
	// TryClaimRun returns ErrRunAdmissionClosed after admission has been closed,
	// or ErrRunSlotBusy when another claim currently owns the slot.
	TryClaimRun() (RunClaim, error)
}

// RunAdmissionCloser is an optional capability for guards that can permanently
// stop admitting claims and report when all accepted claims have been released.
type RunAdmissionCloser interface {
	// CloseAdmission rejects future claims and returns a stable idle signal.
	// The signal closes when every accepted claim and wrapper lease completes.
	CloseAdmission() <-chan struct{}
}

// RunSlotWrapperRegistrar is an optional guard capability used by public run
// entrypoints to keep a claim active until their post-owner work is complete.
// The returned completion callback must be called after all tracing, terminal
// events, and lifecycle hooks owned by that entrypoint have finished.
type RunSlotWrapperRegistrar interface {
	// RegisterRunWrapper attaches the single wrapper lease for a current claim.
	RegisterRunWrapper(claim RunClaim, status RunStatusHandle) (done func(), err error)
	// SetRunWrapperStatus attaches its owner status after the wrapper is claimed.
	SetRunWrapperStatus(claim RunClaim, status RunStatusHandle) error
	// ActiveRunStatus returns an owner status snapshot while a claim is active.
	// The bool reports that an accepted claim still exists; the owner snapshot
	// does not prove the entrypoint wrapper has finished. Teardown must wait on
	// RunAdmissionCloser.CloseAdmission's idle channel, which includes wrapper
	// completion.
	ActiveRunStatus() (RunStatus, bool)
}

func registerRunWrapper(guard RunSlotGuard, claim RunClaim, status RunStatusHandle) (func(), error) {
	registrar, ok := guard.(RunSlotWrapperRegistrar)
	if !ok {
		return func() {}, nil
	}
	return registrar.RegisterRunWrapper(claim, status)
}

func setRunWrapperStatus(guard RunSlotGuard, claim RunClaim, status RunStatusHandle) error {
	registrar, ok := guard.(RunSlotWrapperRegistrar)
	if !ok {
		return nil
	}
	return registrar.SetRunWrapperStatus(claim, status)
}

// linkRunLifetime combines a caller context with an optional Assembly-owned
// lifetime. Canceling either context stops run input, while the owner remains
// responsible for draining any workers it already owns.
func linkRunLifetime(caller, lifetime context.Context) (context.Context, context.CancelFunc, func()) {
	if caller == nil {
		caller = context.Background()
	}
	ctx, cancel := context.WithCancel(caller)
	if lifetime == nil {
		return ctx, cancel, func() {}
	}
	if lifetime.Err() != nil {
		cancel()
		return ctx, cancel, func() {}
	}
	stop := context.AfterFunc(lifetime, cancel)
	return ctx, cancel, func() { stop() }
}

var (
	// ErrRunSlotStaleClaim indicates that a wrapper registration does not match
	// the guard's currently active claim.
	ErrRunSlotStaleClaim = errors.New("run_slot: stale claim")
	// ErrRunWrapperAlreadyRegistered indicates that a claim already has its
	// single allowed public-entrypoint wrapper.
	ErrRunWrapperAlreadyRegistered = errors.New("run_slot: wrapper already registered")
	ErrRunWrapperStatusAlreadySet  = errors.New("run_slot: wrapper status already set")
)

type activeRunClaim struct {
	claim             RunClaim
	ownerReleased     bool
	wrapperRegistered bool
	wrapperFinished   bool
	statusAttached    bool
	status            RunStatusHandle
}

// DefaultRunSlotGuard is a thread-safe RunSlotGuard that admits one active
// claim at a time. Claim publication and release are linearized under mu.
type DefaultRunSlotGuard struct {
	mu          sync.Mutex
	current     *activeRunClaim
	availableCh chan struct{}
	closedCh    chan struct{}
	idleCh      chan struct{}
	closed      bool
}

var _ RunSlotGuard = (*DefaultRunSlotGuard)(nil)
var _ RunAdmissionCloser = (*DefaultRunSlotGuard)(nil)
var _ RunSlotTryClaimer = (*DefaultRunSlotGuard)(nil)
var _ RunSlotWrapperRegistrar = (*DefaultRunSlotGuard)(nil)

// NewDefaultRunSlotGuard returns a DefaultRunSlotGuard ready to issue claims.
func NewDefaultRunSlotGuard() *DefaultRunSlotGuard {
	return &DefaultRunSlotGuard{
		availableCh: make(chan struct{}),
		closedCh:    make(chan struct{}),
		idleCh:      make(chan struct{}),
	}
}

// CloseAdmission permanently rejects new claims and returns the guard's stable
// idle signal. The signal closes immediately if no claim is active, or when
// the accepted claim is actually released.
func (g *DefaultRunSlotGuard) CloseAdmission() <-chan struct{} {
	g.mu.Lock()
	g.ensureNotificationsLocked()
	if !g.closed {
		g.closed = true
		close(g.closedCh)
		if g.current == nil {
			close(g.idleCh)
		}
	}
	idle := g.idleCh
	g.mu.Unlock()
	return idle
}

func (g *DefaultRunSlotGuard) ensureNotificationsLocked() {
	if g.availableCh == nil {
		g.availableCh = make(chan struct{})
	}
	if g.closedCh == nil {
		g.closedCh = make(chan struct{})
	}
	if g.idleCh == nil {
		g.idleCh = make(chan struct{})
	}
}

// ClaimRun acquires the single run slot. It blocks until the slot is available
// or the context is canceled. Closing admission wakes blocked claimants and
// makes them fail without waiting for the current claim to be released.
func (g *DefaultRunSlotGuard) ClaimRun(ctx context.Context) (RunClaim, error) {
	if err := ctx.Err(); err != nil {
		return RunClaim{}, fmt.Errorf("run_slot: claim canceled: %w", err)
	}
	claimID := newClaimID()
	for {
		if err := ctx.Err(); err != nil {
			slog.Debug("core.run_slot.claim_canceled", "err", err)
			return RunClaim{}, fmt.Errorf("run_slot: claim canceled: %w", err)
		}

		g.mu.Lock()
		g.ensureNotificationsLocked()
		if g.closed {
			g.mu.Unlock()
			if err := ctx.Err(); err != nil {
				slog.Debug("core.run_slot.claim_canceled", "err", err)
				return RunClaim{}, fmt.Errorf("run_slot: claim canceled: %w", err)
			}
			return RunClaim{}, fmt.Errorf("run_slot: claim rejected: %w", ErrRunAdmissionClosed)
		}
		if g.current == nil {
			claim := RunClaim{ID: claimID, AcquiredAt: time.Now()}
			g.current = &activeRunClaim{claim: claim}
			g.mu.Unlock()

			slog.Info("core.run_slot.claimed", "id", claim.ID)
			return claim, nil
		}
		availableCh := g.availableCh
		closedCh := g.closedCh
		g.mu.Unlock()

		select {
		case <-availableCh:
		case <-closedCh:
		case <-ctx.Done():
			err := ctx.Err()
			slog.Debug("core.run_slot.claim_canceled", "err", err)
			return RunClaim{}, fmt.Errorf("run_slot: claim canceled: %w", err)
		}
	}
}

// TryClaimRun atomically claims an idle run slot without waiting. Closed
// admission takes precedence over the busy result so callers can distinguish
// shutdown from an active run.
func (g *DefaultRunSlotGuard) TryClaimRun() (RunClaim, error) {
	claimID := newClaimID()
	g.mu.Lock()
	g.ensureNotificationsLocked()
	if g.closed {
		g.mu.Unlock()
		return RunClaim{}, fmt.Errorf("run_slot: try claim rejected: %w", ErrRunAdmissionClosed)
	}
	if g.current != nil {
		g.mu.Unlock()
		return RunClaim{}, fmt.Errorf("run_slot: try claim unavailable: %w", ErrRunSlotBusy)
	}
	claim := RunClaim{ID: claimID, AcquiredAt: time.Now()}
	g.current = &activeRunClaim{claim: claim}
	g.mu.Unlock()

	slog.Info("core.run_slot.claimed", "id", claim.ID)
	return claim, nil
}

// ExecuteClaimedRun validates that the claim is the current holder, executes
// fn, and releases the slot. The slot is always released via defer so a panic
// in fn does not leak the slot.
func (g *DefaultRunSlotGuard) ExecuteClaimedRun(claim RunClaim, fn func() error) error {
	g.mu.Lock()
	if g.current == nil || g.current.claim.ID != claim.ID {
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
	if g.current == nil || g.current.claim.ID != claim.ID {
		g.mu.Unlock()
		slog.Debug("core.run_slot.release_noop", "id", claim.ID)
		return
	}
	g.current.ownerReleased = true
	if g.current.wrapperRegistered && !g.current.wrapperFinished {
		g.mu.Unlock()
		return
	}
	finalized := g.finalizeReleaseLocked(claim.ID)
	g.mu.Unlock()
	if finalized {
		slog.Info("core.run_slot.released", "id", claim.ID)
	}
}

func (g *DefaultRunSlotGuard) finalizeReleaseLocked(claimID string) bool {
	if g.current == nil || g.current.claim.ID != claimID || !g.current.ownerReleased {
		return false
	}
	g.current = nil
	close(g.availableCh)
	g.availableCh = make(chan struct{})
	if g.closed {
		close(g.idleCh)
	}
	return true
}

// RegisterRunWrapper attaches the one public-entrypoint completion lease to
// the current claim. The claim remains unavailable until both the run owner
// releases it and the returned callback is invoked.
func (g *DefaultRunSlotGuard) RegisterRunWrapper(claim RunClaim, status RunStatusHandle) (func(), error) {
	g.mu.Lock()
	if g.current == nil || g.current.claim.ID != claim.ID {
		g.mu.Unlock()
		return nil, fmt.Errorf("run_slot: register wrapper: %w", ErrRunSlotStaleClaim)
	}
	if g.current.wrapperRegistered {
		g.mu.Unlock()
		return nil, fmt.Errorf("run_slot: register wrapper: %w", ErrRunWrapperAlreadyRegistered)
	}
	g.current.wrapperRegistered = true
	g.current.status = status
	g.current.statusAttached = status != nil
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.current == nil || g.current.claim.ID != claim.ID {
				g.mu.Unlock()
				slog.Debug("core.run_slot.wrapper_done_stale", "id", claim.ID)
				return
			}
			g.current.wrapperFinished = true
			finalized := g.current.ownerReleased && g.finalizeReleaseLocked(claim.ID)
			g.mu.Unlock()
			if finalized {
				slog.Info("core.run_slot.released", "id", claim.ID)
			}
		})
	}, nil
}

// SetRunWrapperStatus attaches the live owner handle after the entrypoint has
// registered its wrapper lease and created the owner. This lets the lease be
// established before borrowed tracing producers are started.
func (g *DefaultRunSlotGuard) SetRunWrapperStatus(claim RunClaim, status RunStatusHandle) error {
	if status == nil {
		return fmt.Errorf("run_slot: set wrapper status: nil status handle")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.current == nil || g.current.claim.ID != claim.ID || !g.current.wrapperRegistered || g.current.wrapperFinished {
		return fmt.Errorf("run_slot: set wrapper status: %w", ErrRunSlotStaleClaim)
	}
	if g.current.statusAttached {
		return fmt.Errorf("run_slot: set wrapper status: %w", ErrRunWrapperStatusAlreadySet)
	}
	g.current.status = status
	g.current.statusAttached = true
	return nil
}

// ActiveRunStatus returns a fresh snapshot from the active claim's status
// handle. It never calls the handle while holding the guard mutex.
func (g *DefaultRunSlotGuard) ActiveRunStatus() (RunStatus, bool) {
	g.mu.Lock()
	if g.current == nil {
		g.mu.Unlock()
		return RunStatus{}, false
	}
	handle := g.current.status
	claimID := g.current.claim.ID
	g.mu.Unlock()
	if handle == nil {
		return RunStatus{RunID: claimID, ExecutionState: RunExecutionRunning}, true
	}
	return handle.Snapshot(), true
}

// noopRunSlotGuard is a RunSlotGuard that performs no synchronization. It is
// the zero-config default used when no guard is explicitly configured, so
// callers that don't need single-run enforcement pay no overhead.
type noopRunSlotGuard struct{}

var defaultRunSlotGuard RunSlotGuard = noopRunSlotGuard{}

// ClaimRun returns a no-op claim immediately.
func (noopRunSlotGuard) ClaimRun(ctx context.Context) (RunClaim, error) {
	if err := ctx.Err(); err != nil {
		return RunClaim{}, fmt.Errorf("run_slot: claim canceled: %w", err)
	}
	return RunClaim{ID: newClaimID(), AcquiredAt: time.Now()}, nil
}

// ExecuteClaimedRun runs fn directly without any slot enforcement.
func (noopRunSlotGuard) ExecuteClaimedRun(_ RunClaim, fn func() error) error {
	return fn()
}

// Release is a no-op.
func (noopRunSlotGuard) Release(_ RunClaim) {}

// newClaimID generates a unique random hex identifier for a claim.
func newClaimID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a timestamp-based ID if crypto/rand fails.
		return fmt.Sprintf("claim-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
