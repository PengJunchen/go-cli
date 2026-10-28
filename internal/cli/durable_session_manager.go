package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/session"
)

var (
	// ErrDurableWorkspaceIdentity indicates that the invocation root could not
	// be converted into a stable workspace identity.
	ErrDurableWorkspaceIdentity = errors.New("cli: invalid durable workspace identity")
	// ErrNoDurableSessionSelected indicates that the manager has no current handle.
	ErrNoDurableSessionSelected = errors.New("cli: no durable session is selected")
	// ErrPreparedSessionBranchSelectionUnsupported indicates that the requested
	// branch is not the branch selected by the prepared handle.
	ErrPreparedSessionBranchSelectionUnsupported = errors.New("cli: prepared session cannot select a non-active branch")
	// ErrDurableSessionClosePending indicates that run leases still pin a handle.
	ErrDurableSessionClosePending = errors.New("cli: durable session close is pending active run leases")
	// ErrDurableSessionManagerClosing indicates that the manager rejects new work.
	ErrDurableSessionManagerClosing = errors.New("cli: durable session manager is closing")
	// ErrDurableSessionUnavailable indicates that a prepared session needs recovery.
	ErrDurableSessionUnavailable = errors.New("cli: durable session is unavailable")
	// ErrDurableSessionIdentityConflict indicates that a prepared handle did not
	// match the exact physical session requested by its caller.
	ErrDurableSessionIdentityConflict = errors.New("cli: durable session identity conflicts with the selected session")
)

// SelectionAppliedCleanupError reports that the selected physical session is
// active even though closing the previously selected session failed or timed out.
type SelectionAppliedCleanupError struct {
	SelectedPhysicalSessionID string
	SelectedBranchID          string
	Cause                     error
}

// Error describes the selected session and the failed cleanup cause.
func (err *SelectionAppliedCleanupError) Error() string {
	if err == nil {
		return "cli: selection applied with prior-session cleanup error"
	}
	return fmt.Sprintf("cli: selected session %q branch %q; prior-session cleanup failed: %v", err.SelectedPhysicalSessionID, err.SelectedBranchID, err.Cause)
}

// Unwrap returns the prior-session cleanup error.
func (err *SelectionAppliedCleanupError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

// resolveWorkspaceID captures and canonicalizes one explicit invocation root
// before deriving a domain-separated identity for workspace quarantine.
func resolveWorkspaceID(getwd func() (string, error)) (string, error) {
	_, workspaceID, err := resolveWorkspaceIdentity(getwd)
	return workspaceID, err
}

func resolveWorkspaceIdentity(getwd func() (string, error)) (string, string, error) {
	if getwd == nil {
		return "", "", fmt.Errorf("%w: working-directory source is nil", ErrDurableWorkspaceIdentity)
	}
	root, err := getwd()
	if err != nil {
		return "", "", fmt.Errorf("%w: get invocation working directory: %w", ErrDurableWorkspaceIdentity, err)
	}
	if root == "" {
		return "", "", fmt.Errorf("%w: invocation working directory is empty", ErrDurableWorkspaceIdentity)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", "", fmt.Errorf("%w: make workspace root absolute: %w", ErrDurableWorkspaceIdentity, err)
	}
	clean := filepath.Clean(absolute)
	canonical, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve workspace root symlinks: %w", ErrDurableWorkspaceIdentity, err)
	}
	if !filepath.IsAbs(canonical) {
		return "", "", fmt.Errorf("%w: canonical workspace root is not absolute", ErrDurableWorkspaceIdentity)
	}
	workspaceID, err := session.WorkspaceIDForCanonicalRoot(canonical)
	if err != nil {
		return "", "", fmt.Errorf("%w: derive canonical workspace identity: %w", ErrDurableWorkspaceIdentity, err)
	}
	return canonical, workspaceID, nil
}

// durableSessionManager owns one prepared session handle and lends counted,
// identity-pinned run leases to the prompt or interactive run owner.
type durableSessionManager struct {
	preparer  session.SessionPreparer
	workspace string
	root      string

	transition chan struct{}
	mu         sync.Mutex
	current    *managedPreparedSession
	retired    map[*managedPreparedSession]struct{}
	closing    bool
}

type managedPreparedSession struct {
	prepared         session.PreparedSession
	physical         string
	branch           string
	leases           int
	admin            bool
	changed          chan struct{}
	closeReq         bool
	closed           bool
	closeErr         error
	poisoned         bool
	historyUncertain bool
}

// durableSessionRunLease borrows one prepared handle. Its release method only
// returns this borrow; it never closes the manager-owned session.
type durableSessionRunLease struct {
	manager   *durableSessionManager
	slot      *managedPreparedSession
	prepared  session.PreparedSession
	physical  string
	branch    string
	workspace string
	root      string
	exclusive bool
	once      sync.Once
}

// newDurableSessionManager creates an empty manager for one workspace identity.
func newDurableSessionManager(preparer session.SessionPreparer, workspaceID string) (*durableSessionManager, error) {
	return newDurableSessionManagerForWorkspace(preparer, "", workspaceID)
}

func newDurableSessionManagerForWorkspace(preparer session.SessionPreparer, canonicalRoot, workspaceID string) (*durableSessionManager, error) {
	if preparer == nil {
		return nil, errors.New("cli: durable session preparer is required")
	}
	if workspaceID == "" || strings.ContainsRune(workspaceID, '\x00') {
		return nil, fmt.Errorf("%w: workspace ID must be non-empty and contain no NUL", ErrDurableWorkspaceIdentity)
	}
	if canonicalRoot != "" {
		derivedWorkspaceID, err := session.WorkspaceIDForCanonicalRoot(canonicalRoot)
		if err != nil || derivedWorkspaceID != workspaceID {
			return nil, fmt.Errorf("%w: canonical workspace root does not match workspace ID", ErrDurableWorkspaceIdentity)
		}
	}
	transition := make(chan struct{}, 1)
	transition <- struct{}{}
	return &durableSessionManager{
		preparer:   preparer,
		workspace:  workspaceID,
		root:       canonicalRoot,
		transition: transition,
		retired:    make(map[*managedPreparedSession]struct{}),
	}, nil
}

// prepareNew selects a newly prepared physical session after its snapshot and
// active branch have been validated.
func (manager *durableSessionManager) prepareNew(ctx context.Context, label string) error {
	ctx = normalizeSessionContext(ctx)
	if err := manager.enterTransition(ctx); err != nil {
		return fmt.Errorf("cli: wait to prepare new durable session: %w", err)
	}
	defer manager.leaveTransition()
	if err := manager.checkOpenForSelection(); err != nil {
		return err
	}

	prepared, err := manager.preparer.PrepareNew(ctx, label)
	if err != nil {
		return fmt.Errorf("cli: prepare new durable session: %w", err)
	}
	slot, err := newManagedPreparedSession(prepared)
	if err != nil {
		return manager.closeRejectedPrepared(ctx, prepared, fmt.Errorf("cli: validate new durable session: %w", err))
	}
	return manager.installPreparedSession(ctx, slot)
}

// selectResume selects one exact physical session and its active branch.
// PreparedSession currently has no safe operation for selecting another branch,
// so an alternate branch is rejected before changing the current selection.
func (manager *durableSessionManager) selectResume(ctx context.Context, physicalSessionID, branchID string) error {
	ctx = normalizeSessionContext(ctx)
	if err := validateSessionIdentity("physical session ID", physicalSessionID); err != nil {
		return err
	}
	if err := manager.enterTransition(ctx); err != nil {
		return fmt.Errorf("cli: wait to select durable session %q: %w", physicalSessionID, err)
	}
	defer manager.leaveTransition()
	if err := manager.checkOpenForSelection(); err != nil {
		return err
	}

	manager.mu.Lock()
	current := manager.current
	if current != nil && current.physical == physicalSessionID {
		requestedBranch := branchID
		if requestedBranch == "" {
			requestedBranch = current.branch
		}
		if requestedBranch != current.branch {
			manager.mu.Unlock()
			return fmt.Errorf("%w: session %q has active branch %q; requested %q", ErrPreparedSessionBranchSelectionUnsupported, physicalSessionID, current.branch, requestedBranch)
		}
		if current.poisoned || current.closed || current.closeReq || current.prepared == nil {
			manager.mu.Unlock()
			return fmt.Errorf("%w: session %q requires exact reopen", ErrDurableSessionUnavailable, physicalSessionID)
		}
		prepared := current.prepared
		manager.mu.Unlock()
		snapshot, err := prepared.Snapshot()
		manager.mu.Lock()
		if err != nil {
			current.poisoned = true
			manager.signalSlotLocked(current)
			closing := manager.closing
			manager.mu.Unlock()
			if closing {
				return errors.Join(ErrDurableSessionManagerClosing, fmt.Errorf("%w: inspect session %q before reuse: %w", ErrDurableSessionUnavailable, physicalSessionID, err))
			}
			return fmt.Errorf("%w: inspect session %q before reuse: %w", ErrDurableSessionUnavailable, physicalSessionID, err)
		}
		if manager.closing {
			manager.mu.Unlock()
			return ErrDurableSessionManagerClosing
		}
		if manager.current != current || current.closeReq || current.closed || current.poisoned {
			manager.mu.Unlock()
			return fmt.Errorf("%w: session %q changed while checking reuse", ErrDurableSessionUnavailable, physicalSessionID)
		}
		if snapshot.SessionID != current.physical || snapshot.ActiveBranchID != current.branch {
			current.poisoned = true
			manager.signalSlotLocked(current)
			manager.mu.Unlock()
			return fmt.Errorf("%w: session %q identity or active branch changed", ErrDurableSessionUnavailable, physicalSessionID)
		}
		manager.mu.Unlock()
		return nil
	}
	retired := manager.findRetiredLocked(physicalSessionID)
	manager.mu.Unlock()

	if retired != nil {
		if err := manager.closeSlot(ctx, retired); err != nil {
			return err
		}
	}
	if err := manager.checkOpenForSelection(); err != nil {
		return err
	}

	prepared, err := manager.preparer.PrepareResume(ctx, physicalSessionID)
	if err != nil {
		return fmt.Errorf("cli: prepare exact durable session %q: %w", physicalSessionID, err)
	}
	slot, err := newManagedPreparedSession(prepared)
	if err != nil {
		return manager.closeRejectedPrepared(ctx, prepared, fmt.Errorf("cli: validate resumed durable session %q: %w", physicalSessionID, err))
	}
	if slot.physical != physicalSessionID {
		return manager.closeRejectedSlot(ctx, slot, fmt.Errorf("%w: requested %q but preparer returned %q", ErrDurableSessionIdentityConflict, physicalSessionID, slot.physical))
	}
	if branchID != "" && branchID != slot.branch {
		return manager.closeRejectedSlot(ctx, slot, fmt.Errorf("%w: session %q has active branch %q; requested %q", ErrPreparedSessionBranchSelectionUnsupported, physicalSessionID, slot.branch, branchID))
	}
	return manager.installPreparedSession(ctx, slot)
}

// acquireRunLease pins the current prepared handle and its physical identity.
func (manager *durableSessionManager) acquireRunLease() (*durableSessionRunLease, error) {
	return manager.acquireLease(false)
}

// acquireReconciliationLease exclusively pins the selected session for a
// user-requested durable recovery operation. It refuses to overlap any run or
// other lease and blocks subsequent run leases until CloseRunCommit.
func (manager *durableSessionManager) acquireReconciliationLease() (*durableSessionRunLease, error) {
	return manager.acquireLease(true)
}

func (manager *durableSessionManager) acquireLease(exclusive bool) (*durableSessionRunLease, error) {
	manager.mu.Lock()
	if manager.closing {
		manager.mu.Unlock()
		return nil, ErrDurableSessionManagerClosing
	}
	slot := manager.current
	if slot == nil {
		manager.mu.Unlock()
		return nil, ErrNoDurableSessionSelected
	}
	if slot.closeReq || slot.closed || slot.prepared == nil {
		if slot.poisoned {
			manager.mu.Unlock()
			return nil, fmt.Errorf("%w: session %q requires exact reopen", ErrDurableSessionUnavailable, slot.physical)
		}
		manager.mu.Unlock()
		return nil, fmt.Errorf("%w: session %q is closing or closed", ErrDurableSessionManagerClosing, slot.physical)
	}
	if slot.poisoned {
		manager.mu.Unlock()
		return nil, fmt.Errorf("%w: session %q requires exact reopen", ErrDurableSessionUnavailable, slot.physical)
	}
	if exclusive {
		if slot.leases != 0 || slot.admin {
			manager.mu.Unlock()
			return nil, errors.New("cli: durable session is busy with another owner")
		}
		slot.admin = true
	} else if slot.admin {
		manager.mu.Unlock()
		return nil, errors.New("cli: durable session is held by an exclusive recovery operation")
	}
	prepared := slot.prepared
	physical, branch := slot.physical, slot.branch
	slot.leases++ // A temporary borrow protects Snapshot from requester-path Close.
	manager.signalSlotLocked(slot)
	manager.mu.Unlock()

	snapshot, err := prepared.Snapshot()

	manager.mu.Lock()
	if err != nil {
		if manager.current == slot {
			slot.poisoned = true
			manager.signalSlotLocked(slot)
		}
		if exclusive {
			slot.admin = false
		}
		manager.releaseLeaseLocked(slot)
		closing := manager.closing
		manager.mu.Unlock()
		unavailableErr := fmt.Errorf("%w: session %q snapshot failed: %w", ErrDurableSessionUnavailable, physical, err)
		if closing {
			return nil, errors.Join(ErrDurableSessionManagerClosing, unavailableErr)
		}
		return nil, unavailableErr
	}
	if manager.closing {
		if exclusive {
			slot.admin = false
		}
		manager.releaseLeaseLocked(slot)
		manager.mu.Unlock()
		return nil, ErrDurableSessionManagerClosing
	}
	if manager.current != slot || slot.closeReq || slot.closed || slot.poisoned {
		if exclusive {
			slot.admin = false
		}
		manager.releaseLeaseLocked(slot)
		manager.mu.Unlock()
		return nil, fmt.Errorf("%w: session %q changed while checking the run lease", ErrDurableSessionUnavailable, physical)
	}
	if snapshot.SessionID != physical || snapshot.ActiveBranchID != branch {
		slot.poisoned = true
		manager.signalSlotLocked(slot)
		if exclusive {
			slot.admin = false
		}
		manager.releaseLeaseLocked(slot)
		manager.mu.Unlock()
		return nil, fmt.Errorf("%w: session %q identity or active branch changed", ErrDurableSessionUnavailable, physical)
	}
	manager.mu.Unlock()
	return &durableSessionRunLease{
		manager:   manager,
		slot:      slot,
		prepared:  prepared,
		physical:  physical,
		branch:    branch,
		workspace: manager.workspace,
		root:      manager.root,
		exclusive: exclusive,
	}, nil
}

// requestClose prevents future borrows and closes each handle on the caller's
// path after its outstanding run leases have returned.
func (manager *durableSessionManager) requestClose(ctx context.Context) error {
	ctx = normalizeSessionContext(ctx)
	manager.mu.Lock()
	manager.closing = true
	manager.mu.Unlock()
	if err := manager.enterTransition(ctx); err != nil {
		return fmt.Errorf("%w: wait for durable session transition: %w", ErrDurableSessionClosePending, err)
	}
	defer manager.leaveTransition()

	manager.mu.Lock()
	if manager.current != nil {
		manager.retireLocked(manager.current)
		manager.current = nil
	}
	slots := make([]*managedPreparedSession, 0, len(manager.retired))
	for slot := range manager.retired {
		slot.closeReq = true
		manager.signalSlotLocked(slot)
		slots = append(slots, slot)
	}
	manager.mu.Unlock()

	var closeErrors []error
	for _, slot := range slots {
		if err := manager.closeSlot(ctx, slot); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	return errors.Join(closeErrors...)
}

// reopenPoisoned closes a poisoned handle after every run lease returns, then
// recovers the exact same physical ID before permitting another run.
func (manager *durableSessionManager) reopenPoisoned(ctx context.Context) error {
	ctx = normalizeSessionContext(ctx)
	if err := manager.enterTransition(ctx); err != nil {
		return fmt.Errorf("cli: wait to recover durable session: %w", err)
	}
	defer manager.leaveTransition()

	manager.mu.Lock()
	if manager.closing {
		manager.mu.Unlock()
		return ErrDurableSessionManagerClosing
	}
	slot := manager.current
	if slot == nil {
		manager.mu.Unlock()
		return ErrNoDurableSessionSelected
	}
	if slot.historyUncertain {
		manager.mu.Unlock()
		return fmt.Errorf("%w: administrative sync was uncertain; close this invocation and resume exact physical session %s", ErrDurableSessionUnavailable, slot.physical)
	}
	if !slot.poisoned {
		if slot.closed || slot.prepared == nil || slot.closeReq {
			manager.mu.Unlock()
			return fmt.Errorf("%w: session %q is not available for reopen", ErrDurableSessionUnavailable, slot.physical)
		}
		prepared, physicalID, branchID := slot.prepared, slot.physical, slot.branch
		manager.mu.Unlock()

		snapshot, snapshotErr := prepared.Snapshot()

		manager.mu.Lock()
		if manager.closing {
			manager.mu.Unlock()
			return ErrDurableSessionManagerClosing
		}
		if manager.current != slot || slot.closed || slot.closeReq {
			manager.mu.Unlock()
			return fmt.Errorf("%w: session %q changed while checking recovery", ErrDurableSessionUnavailable, physicalID)
		}
		if snapshotErr == nil && snapshot.SessionID == physicalID && snapshot.ActiveBranchID == branchID && !slot.poisoned {
			manager.mu.Unlock()
			return nil
		}
		slot.poisoned = true
		manager.signalSlotLocked(slot)
	}
	slot.closeReq = true
	manager.signalSlotLocked(slot)
	physicalID, branchID := slot.physical, slot.branch
	manager.mu.Unlock()

	if err := manager.closeSlot(ctx, slot); err != nil {
		return err
	}
	if err := manager.checkOpenForSelection(); err != nil {
		return err
	}
	prepared, err := manager.preparer.PrepareResume(ctx, physicalID)
	if err != nil {
		return fmt.Errorf("%w: recover exact session %q: %w", ErrDurableSessionUnavailable, physicalID, err)
	}
	reopened, err := newManagedPreparedSession(prepared)
	if err != nil {
		return manager.closeRejectedPrepared(ctx, prepared, fmt.Errorf("%w: validate reopened session %q: %w", ErrDurableSessionUnavailable, physicalID, err))
	}
	if reopened.physical != physicalID || reopened.branch != branchID {
		return manager.closeRejectedSlot(ctx, reopened, fmt.Errorf("%w: reopened session %q changed physical identity or active branch", ErrDurableSessionUnavailable, physicalID))
	}

	manager.mu.Lock()
	if manager.closing || manager.current != slot || !slot.closed || slot.physical != physicalID || slot.branch != branchID {
		closing := manager.closing
		manager.mu.Unlock()
		cause := fmt.Errorf("%w: exact reopen for session %q lost its selection slot", ErrDurableSessionUnavailable, physicalID)
		if closing {
			cause = errors.Join(ErrDurableSessionManagerClosing, cause)
		}
		return manager.closeRejectedSlot(ctx, reopened, cause)
	}
	manager.current = reopened
	manager.mu.Unlock()
	return nil
}

func (manager *durableSessionManager) installPreparedSession(ctx context.Context, slot *managedPreparedSession) error {
	manager.mu.Lock()
	if manager.closing {
		manager.mu.Unlock()
		return manager.closeRejectedSlot(ctx, slot, ErrDurableSessionManagerClosing)
	}
	old := manager.current
	if old != nil && old.admin {
		manager.mu.Unlock()
		return manager.closeRejectedSlot(ctx, slot, errors.New("cli: selected durable session is held by an exclusive recovery operation"))
	}
	if old != nil && old.physical == slot.physical {
		manager.mu.Unlock()
		return manager.closeRejectedSlot(ctx, slot, fmt.Errorf("%w: %q", ErrDurableSessionIdentityConflict, slot.physical))
	}
	if retired := manager.findRetiredLocked(slot.physical); retired != nil {
		manager.mu.Unlock()
		return manager.closeRejectedSlot(ctx, slot, fmt.Errorf("%w: %q still has a retired prepared handle", ErrDurableSessionIdentityConflict, slot.physical))
	}
	if old != nil {
		manager.retireLocked(old)
	}
	manager.current = slot
	manager.mu.Unlock()
	if old != nil {
		if err := manager.closeSlot(ctx, old); err != nil {
			return &SelectionAppliedCleanupError{
				SelectedPhysicalSessionID: slot.physical,
				SelectedBranchID:          slot.branch,
				Cause:                     err,
			}
		}
	}
	return nil
}

// closeSlot is called by a transition-gate owner. The run lease release path
// only changes the reference count and wakes this caller. PreparedSession.Close
// has no context support: cancellation bounds gate and lease waits, but cannot
// interrupt a Close call once it has started.
func (manager *durableSessionManager) closeSlot(ctx context.Context, slot *managedPreparedSession) error {
	ctx = normalizeSessionContext(ctx)
	for {
		manager.mu.Lock()
		if slot.closed {
			err := slot.closeErr
			manager.mu.Unlock()
			if err != nil {
				return fmt.Errorf("cli: close durable session %q: %w", slot.physical, err)
			}
			return nil
		}
		if slot.leases > 0 {
			changed := slot.changed
			manager.mu.Unlock()
			select {
			case <-ctx.Done():
				return closePendingError(slot.physical, ctx.Err())
			case <-changed:
				continue
			}
		}
		if err := ctx.Err(); err != nil {
			manager.mu.Unlock()
			return closePendingError(slot.physical, err)
		}
		prepared := slot.prepared
		manager.mu.Unlock()

		var closeErr error
		if prepared != nil {
			closeErr = prepared.Close()
		}

		manager.mu.Lock()
		slot.closed = true
		slot.closeErr = closeErr
		manager.signalSlotLocked(slot)
		if closeErr == nil {
			delete(manager.retired, slot)
		}
		manager.mu.Unlock()
		if closeErr != nil {
			return fmt.Errorf("cli: close durable session %q: %w", slot.physical, closeErr)
		}
		return nil
	}
}

func (manager *durableSessionManager) releaseRunLease(slot *managedPreparedSession, exclusive bool) {
	manager.mu.Lock()
	if exclusive {
		slot.admin = false
	}
	manager.releaseLeaseLocked(slot)
	manager.mu.Unlock()
}

func (manager *durableSessionManager) releaseLeaseLocked(slot *managedPreparedSession) {
	if slot.leases > 0 {
		slot.leases--
		manager.signalSlotLocked(slot)
	}
}

func (manager *durableSessionManager) enterTransition(ctx context.Context) error {
	ctx = normalizeSessionContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-manager.transition:
	}
	if err := ctx.Err(); err != nil {
		manager.leaveTransition()
		return err
	}
	return nil
}

func (manager *durableSessionManager) leaveTransition() {
	manager.transition <- struct{}{}
}

func (manager *durableSessionManager) checkOpenForSelection() error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closing {
		return ErrDurableSessionManagerClosing
	}
	return nil
}

func (manager *durableSessionManager) retireLocked(slot *managedPreparedSession) {
	slot.closeReq = true
	manager.signalSlotLocked(slot)
	manager.retired[slot] = struct{}{}
}

func (manager *durableSessionManager) findRetiredLocked(physicalID string) *managedPreparedSession {
	for slot := range manager.retired {
		if slot.physical == physicalID {
			return slot
		}
	}
	return nil
}

func (manager *durableSessionManager) signalSlotLocked(slot *managedPreparedSession) {
	if slot.changed == nil {
		slot.changed = make(chan struct{})
		return
	}
	close(slot.changed)
	slot.changed = make(chan struct{})
}

func (manager *durableSessionManager) closeRejectedPrepared(ctx context.Context, prepared session.PreparedSession, cause error) error {
	if prepared == nil {
		return cause
	}
	slot := &managedPreparedSession{
		prepared: prepared,
		changed:  make(chan struct{}),
		closeReq: true,
	}
	return manager.closeRejectedSlot(ctx, slot, cause)
}

func (manager *durableSessionManager) closeRejectedSlot(ctx context.Context, slot *managedPreparedSession, cause error) error {
	if slot == nil || slot.prepared == nil {
		return cause
	}
	manager.mu.Lock()
	slot.closeReq = true
	manager.signalSlotLocked(slot)
	manager.retired[slot] = struct{}{}
	manager.mu.Unlock()

	if err := manager.closeSlot(ctx, slot); err != nil {
		return errors.Join(cause, fmt.Errorf("cli: close rejected prepared session: %w", err))
	}
	return cause
}

func newManagedPreparedSession(prepared session.PreparedSession) (*managedPreparedSession, error) {
	if prepared == nil {
		return nil, errors.New("prepared handle is nil")
	}
	snapshot, err := prepared.Snapshot()
	if err != nil {
		return nil, err
	}
	if err := validateSessionIdentity("prepared physical session ID", snapshot.SessionID); err != nil {
		return nil, err
	}
	if err := validateSessionIdentity("active branch ID", snapshot.ActiveBranchID); err != nil {
		return nil, err
	}
	return &managedPreparedSession{
		prepared: prepared,
		physical: snapshot.SessionID,
		branch:   snapshot.ActiveBranchID,
		changed:  make(chan struct{}),
	}, nil
}

func closePendingError(physicalID string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: session %q", ErrDurableSessionClosePending, physicalID)
	}
	return fmt.Errorf("%w: session %q: %w", ErrDurableSessionClosePending, physicalID, cause)
}

func normalizeSessionContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func validateSessionIdentity(field, value string) error {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("cli: %s must be non-empty and contain no NUL", field)
	}
	return nil
}

// PreparedSession returns the locked handle pinned by this run lease.
func (lease *durableSessionRunLease) PreparedSession() session.PreparedSession {
	if lease == nil {
		return nil
	}
	return lease.prepared
}

// SessionID returns the physical session ID pinned by this run lease.
func (lease *durableSessionRunLease) SessionID() string {
	if lease == nil {
		return ""
	}
	return lease.physical
}

// BranchID returns the active branch pinned by this run lease.
func (lease *durableSessionRunLease) BranchID() string {
	if lease == nil {
		return ""
	}
	return lease.branch
}

// WorkspaceID returns the stable workspace identity bound to this run lease.
func (lease *durableSessionRunLease) WorkspaceID() string {
	if lease == nil {
		return ""
	}
	return lease.workspace
}

// CanonicalWorkspaceRoot returns the canonical workspace path captured by the
// manager for this run lease.
func (lease *durableSessionRunLease) CanonicalWorkspaceRoot() string {
	if lease == nil {
		return ""
	}
	return lease.root
}

// CloseRunCommit returns this run's manager borrow exactly once. It does not
// perform durable writes or close the prepared session handle.
func (lease *durableSessionRunLease) CloseRunCommit() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		if lease.manager != nil && lease.slot != nil {
			lease.manager.releaseRunLease(lease.slot, lease.exclusive)
		}
	})
}

var _ interface{ CloseRunCommit() } = (*durableSessionRunLease)(nil)
