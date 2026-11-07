package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tools"
)

var (
	// ErrProcessSessionHostClosed indicates that the invocation host no longer
	// accepts process-session managers.
	ErrProcessSessionHostClosed = errors.New("cli: process-session invocation host is closed")
	// ErrProcessSessionHostRelease indicates that the invocation was released
	// before a process-session handoff could be accepted.
	ErrProcessSessionHostReleased = errors.New("cli: process-session invocation host was released")
)

type processSessionHostReleaseCause struct{ ownerID string }

func (cause *processSessionHostReleaseCause) Error() string {
	return "cli: process-session invocation released by host"
}

// ProcessSessionInvocationHost owns session supervisors for one CLI
// invocation and accepts their typed handoff after the assistant final.
type ProcessSessionInvocationHost struct {
	ctx            context.Context
	cancel         context.CancelCauseFunc
	ownerID        string
	releaseCause   *processSessionHostReleaseCause
	workspace      string
	releaseSignal  <-chan os.Signal
	releasedCh     chan struct{}
	watchDone      chan struct{}
	stop           func()
	mu             sync.Mutex
	manager        *tools.LocalProcessSessionManager
	accepted       bool
	released       bool
	releasedByHost bool
	closed         bool
	stopOnce       sync.Once
	closeErr       error
}

// NewProcessSessionInvocationHost creates an invocation owner and registers
// its narrow SIGUSR1 release handler before any handoff can be emitted.
func NewProcessSessionInvocationHost(parent context.Context, workspaceID string) (*ProcessSessionInvocationHost, error) {
	if parent == nil {
		return nil, errors.New("cli: process-session host context is required")
	}
	ownerID, err := newProcessSessionOpaqueID()
	if err != nil {
		return nil, fmt.Errorf("cli: create process-session owner ID: %w", err)
	}
	if workspaceID == "" {
		workspaceID, err = newProcessSessionOpaqueID()
		if err != nil {
			return nil, fmt.Errorf("cli: create process-session workspace ID: %w", err)
		}
	}
	ctx, cancel := context.WithCancelCause(parent)
	releaseSignal, stop := registerProcessSessionReleaseSignal()
	host := &ProcessSessionInvocationHost{
		ctx:           ctx,
		cancel:        cancel,
		ownerID:       ownerID,
		releaseCause:  &processSessionHostReleaseCause{ownerID: ownerID},
		workspace:     workspaceID,
		releaseSignal: releaseSignal,
		releasedCh:    make(chan struct{}),
		watchDone:     make(chan struct{}),
		stop:          stop,
	}
	go host.watchRelease(parent)
	return host, nil
}

// Context returns the host lifetime context for owned process sessions.
func (h *ProcessSessionInvocationHost) Context() context.Context {
	if h == nil {
		return nil
	}
	return h.ctx
}

// OwnerID returns the opaque ID for this invocation owner.
func (h *ProcessSessionInvocationHost) OwnerID() string {
	if h == nil {
		return ""
	}
	return h.ownerID
}

// WorkspaceID returns the opaque workspace identity bound to this invocation.
func (h *ProcessSessionInvocationHost) WorkspaceID() string {
	if h == nil {
		return ""
	}
	return h.workspace
}

// RegisterManager binds one exact manager to the invocation that will own it.
func (h *ProcessSessionInvocationHost) RegisterManager(manager *tools.LocalProcessSessionManager) error {
	if h == nil || manager == nil {
		return errors.New("cli: process-session host and manager are required")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || h.ctx.Err() != nil {
		return ErrProcessSessionHostClosed
	}
	if h.manager != nil && h.manager != manager {
		return errors.New("cli: invocation already owns a process-session manager")
	}
	if manager.OwnerID() != h.ownerID || manager.WorkspaceID() != h.workspace {
		return errors.New("cli: process-session manager identity does not match invocation owner")
	}
	h.manager = manager
	return nil
}

// AcceptHandoff validates and accepts active sessions from the registered
// manager for the exact run that produced the final answer.
func (h *ProcessSessionInvocationHost) AcceptHandoff(manager *tools.LocalProcessSessionManager, runID string, deliveries ...*tools.ProcessSessionDelivery) (tools.ProcessSessionHandoff, bool, error) {
	if h == nil || manager == nil {
		return tools.ProcessSessionHandoff{}, false, errors.New("cli: process-session host and manager are required")
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return tools.ProcessSessionHandoff{}, false, ErrProcessSessionHostClosed
	}
	if h.released || h.ctx.Err() != nil {
		h.mu.Unlock()
		return tools.ProcessSessionHandoff{}, false, nil
	}
	if h.manager != manager || manager.OwnerID() != h.ownerID || manager.WorkspaceID() != h.workspace {
		h.mu.Unlock()
		return tools.ProcessSessionHandoff{}, false, errors.New("cli: process-session manager is not owned by this invocation")
	}
	if h.accepted {
		h.mu.Unlock()
		return tools.ProcessSessionHandoff{}, false, errors.New("cli: process-session handoff was already accepted")
	}
	h.mu.Unlock()

	handoff, active, err := manager.Handoff(runID)
	if err != nil || !active {
		return handoff, active, err
	}
	if manager.RequireDeliveryConfirmation() {
		if len(deliveries) != 1 || deliveries[0] == nil {
			return tools.ProcessSessionHandoff{}, false, errors.New("cli: durable process-session terminal delivery is not confirmed")
		}
		if err := manager.ValidateDelivery(*deliveries[0], runID); err != nil {
			return tools.ProcessSessionHandoff{}, false, err
		}
	}
	if err := manager.ValidateHandoff(handoff); err != nil {
		return tools.ProcessSessionHandoff{}, false, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return tools.ProcessSessionHandoff{}, false, ErrProcessSessionHostClosed
	}
	if h.released || h.ctx.Err() != nil {
		return tools.ProcessSessionHandoff{}, false, nil
	}
	if h.manager != manager || h.accepted {
		return tools.ProcessSessionHandoff{}, false, errors.New("cli: process-session handoff owner changed during acceptance")
	}
	h.accepted = true
	return handoff, true, nil
}

// WaitForRelease keeps the invocation alive until the host receives its
// release signal or the parent invocation is canceled.
func (h *ProcessSessionInvocationHost) WaitForRelease(ctx context.Context) error {
	if h == nil {
		return errors.New("cli: process-session host is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	accepted, released, releasedByHost, closed := h.accepted, h.released, h.releasedByHost, h.closed
	h.mu.Unlock()
	if !accepted {
		if released {
			if releasedByHost {
				return ErrProcessSessionHostReleased
			}
			return processSessionHostContextErr(h.ctx)
		}
		return errors.New("cli: process-session handoff has not been accepted")
	}
	if closed {
		return ErrProcessSessionHostClosed
	}
	if released {
		if releasedByHost {
			return nil
		}
		return processSessionHostContextErr(h.ctx)
	}
	select {
	case <-h.releasedCh:
		h.mu.Lock()
		releasedByHost := h.releasedByHost
		h.mu.Unlock()
		if releasedByHost {
			return nil
		}
		return processSessionHostContextErr(h.ctx)
	case <-h.ctx.Done():
		h.mu.Lock()
		releasedByHost := h.releasedByHost
		h.mu.Unlock()
		if releasedByHost {
			return nil
		}
		return processSessionHostContextErr(h.ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func processSessionHostContextErr(ctx context.Context) error {
	if ctx == nil {
		return context.Canceled
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

// Release explicitly releases the invocation host. Embedded callers may use
// this after accepting a handoff instead of sending SIGUSR1.
func (h *ProcessSessionInvocationHost) Release() {
	if h == nil {
		return
	}
	h.transitionRelease(true, h.releaseCause)
}

// Close releases the host and joins its exact process-session manager.
func (h *ProcessSessionInvocationHost) Close(ctx context.Context) error {
	if h == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Lock()
	h.closed = true
	manager := h.manager
	h.mu.Unlock()
	h.cancel(context.Canceled)
	h.stopOnce.Do(func() {
		if h.stop != nil {
			h.stop()
		}
	})
	if manager != nil {
		closeErr := manager.Close(ctx)
		h.mu.Lock()
		h.closeErr = errors.Join(h.closeErr, closeErr)
		h.mu.Unlock()
	}
	select {
	case <-h.watchDone:
	case <-ctx.Done():
		h.mu.Lock()
		h.closeErr = errors.Join(h.closeErr, ctx.Err())
		h.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closeErr
}

func (h *ProcessSessionInvocationHost) watchRelease(parent context.Context) {
	defer close(h.watchDone)
	select {
	case _, ok := <-h.releaseSignal:
		if !ok {
			return
		}
		h.transitionRelease(true, h.releaseCause)
	case <-parent.Done():
		cause := context.Cause(parent)
		if cause == nil {
			cause = parent.Err()
		}
		h.transitionRelease(false, cause)
	case <-h.ctx.Done():
		cause := context.Cause(h.ctx)
		// Release sets released/releasedByHost while holding mu before it
		// cancels this context. Therefore a watcher that observes ctx.Done
		// after an explicit release will see the already-completed transition.
		// Otherwise this path is a parent/close cancellation, even if its cause
		// happens to wrap or equal the private host-release sentinel.
		h.transitionRelease(false, cause)
	}
}

func (h *ProcessSessionInvocationHost) transitionRelease(byHost bool, cause error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return
	}
	if cause != nil && h.cancel != nil {
		h.cancel(cause)
	}
	actualCause := context.Cause(h.ctx)
	h.released = true
	h.releasedByHost = byHost && h.releaseCause != nil && actualCause == h.releaseCause
	close(h.releasedCh)
}

func newProcessSessionOpaqueID() (string, error) {
	var data [24]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}
