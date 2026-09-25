package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/session"
)

var (
	errManagerTestClose   = errors.New("manager test close failure")
	errManagerTestPrepare = errors.New("manager test prepare failure")
	errManagerTestPoison  = errors.New("manager test poisoned snapshot")
)

func TestResolveWorkspaceIDUsesCanonicalSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "workspace")
	link := filepath.Join(root, "workspace-link")
	require.NoError(t, os.Mkdir(target, 0o700))
	require.NoError(t, os.Symlink(target, link))

	workspaceID, err := resolveWorkspaceID(func() (string, error) { return link, nil })
	require.NoError(t, err)

	canonical, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	digest := sha256.Sum256([]byte(workspaceIdentityDomain + filepath.Clean(canonical)))
	assert.Equal(t, "workspace-v1-"+hex.EncodeToString(digest[:]), workspaceID)

	otherID, err := resolveWorkspaceID(func() (string, error) { return target, nil })
	require.NoError(t, err)
	assert.Equal(t, workspaceID, otherID)
	otherRoot := filepath.Join(root, "other-workspace")
	require.NoError(t, os.Mkdir(otherRoot, 0o700))
	differentID, err := resolveWorkspaceID(func() (string, error) { return otherRoot, nil })
	require.NoError(t, err)
	assert.NotEqual(t, workspaceID, differentID)
}

func TestResolveWorkspaceIDFailsClosedOnGetwdAndCanonicalizationErrors(t *testing.T) {
	getwdErr := errors.New("getwd failed")
	_, err := resolveWorkspaceID(func() (string, error) { return "", getwdErr })
	assert.ErrorIs(t, err, getwdErr)

	_, err = resolveWorkspaceID(func() (string, error) { return "", nil })
	require.Error(t, err)

	_, err = resolveWorkspaceID(func() (string, error) {
		return filepath.Join(t.TempDir(), "missing"), nil
	})
	assert.Error(t, err)
}

func TestResolveWorkspaceIDIsAcceptedByDurableIndexAndSessionPreparer(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("HOME", configRoot)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(configRoot, ".config"))
	ctx := context.Background()
	workspaceID, err := resolveWorkspaceID(os.Getwd)
	require.NoError(t, err)
	index, err := session.NewWorkspaceQuarantineIndex()
	require.NoError(t, err)
	require.NoError(t, index.EnsureWorkspace(ctx, workspaceID))

	prepared, err := session.NewDurableJSONLSessionPreparer(filepath.Join(t.TempDir(), "session-store"), index).PrepareNew(ctx, "workspace-index-check")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Close()) })
	snapshot, err := prepared.Snapshot()
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.SessionID)
}

func TestDurableSessionManagerSameIDSameBranchDoesNotPrepareResume(t *testing.T) {
	prepared := newManagerTestPrepared("session-a", "branch-main")
	preparer := newManagerTestPreparer(prepared)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))

	require.NoError(t, manager.selectResume(context.Background(), "session-a", "branch-main"))
	assert.Empty(t, preparer.resumeCalls())
	assert.Equal(t, 0, prepared.closeCount())

	lease, err := manager.acquireRunLease()
	require.NoError(t, err)
	assert.Same(t, prepared, lease.PreparedSession())
	assert.Equal(t, "session-a", lease.SessionID())
	assert.Equal(t, "branch-main", lease.BranchID())
	assert.Equal(t, "workspace-v1-test", lease.WorkspaceID())
	lease.CloseRunCommit()
	lease.CloseRunCommit()
	require.NoError(t, manager.requestClose(context.Background()))
	assert.Equal(t, 1, prepared.closeCount())
}

func TestDurableSessionManagerRejectsUnsupportedBranchWithoutReopeningSameFile(t *testing.T) {
	prepared := newManagerTestPrepared("session-a", "branch-main")
	preparer := newManagerTestPreparer(prepared)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))

	err := manager.selectResume(context.Background(), "session-a", "branch-other")
	assert.ErrorIs(t, err, ErrPreparedSessionBranchSelectionUnsupported)
	assert.Empty(t, preparer.resumeCalls())
	assert.Equal(t, 0, prepared.closeCount())

	lease, err := manager.acquireRunLease()
	require.NoError(t, err)
	assert.Equal(t, "branch-main", lease.BranchID())
	lease.CloseRunCommit()
	require.NoError(t, manager.requestClose(context.Background()))
}

func TestDurableSessionManagerFailedDifferentSessionPreparationKeepsCurrent(t *testing.T) {
	preparedA := newManagerTestPrepared("session-a", "branch-a")
	preparer := newManagerTestPreparer(preparedA)
	preparer.setResumeError("session-b", errManagerTestPrepare)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))

	err := manager.selectResume(context.Background(), "session-b", "")
	assert.ErrorIs(t, err, errManagerTestPrepare)
	var appliedErr *SelectionAppliedCleanupError
	assert.False(t, errors.As(err, &appliedErr), "a failed target prepare must leave the old selection in place")
	assert.Equal(t, 0, preparedA.closeCount())

	lease, err := manager.acquireRunLease()
	require.NoError(t, err)
	assert.Equal(t, "session-a", lease.SessionID())
	lease.CloseRunCommit()
	require.NoError(t, manager.requestClose(context.Background()))
	assert.Equal(t, 1, preparedA.closeCount())
}

func TestDurableSessionManagerRejectsNonActiveBranchOnNewPhysicalSession(t *testing.T) {
	preparedA := newManagerTestPrepared("session-a", "branch-a")
	preparedB := newManagerTestPrepared("session-b", "branch-b")
	preparer := newManagerTestPreparer(preparedA)
	preparer.setResume("session-b", preparedB)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))

	err := manager.selectResume(context.Background(), "session-b", "branch-other")
	assert.ErrorIs(t, err, ErrPreparedSessionBranchSelectionUnsupported)
	assert.Equal(t, 0, preparedA.closeCount(), "failed target validation must preserve the current handle")
	assert.Equal(t, 1, preparedB.closeCount(), "the rejected target handle must be released")

	lease, err := manager.acquireRunLease()
	require.NoError(t, err)
	assert.Equal(t, "session-a", lease.SessionID())
	lease.CloseRunCommit()
	require.NoError(t, manager.requestClose(context.Background()))
}

func TestDurableSessionManagerSwapDrainsOldLeaseOnRequesterPath(t *testing.T) {
	preparedA := newManagerTestPrepared("session-a", "branch-a")
	preparedB := newManagerTestPrepared("session-b", "branch-b")
	preparer := newManagerTestPreparer(preparedA)
	preparer.setResume("session-b", preparedB)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))
	oldLease, err := manager.acquireRunLease()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	preparer.setAfterResume(func(string) { cancel() })
	err = manager.selectResume(ctx, "session-b", "")
	assert.ErrorIs(t, err, ErrDurableSessionClosePending)
	var appliedErr *SelectionAppliedCleanupError
	require.ErrorAs(t, err, &appliedErr)
	assert.Equal(t, "session-b", appliedErr.SelectedPhysicalSessionID)
	assert.Equal(t, "branch-b", appliedErr.SelectedBranchID)
	assert.Equal(t, 0, preparedA.closeCount(), "a live lease must keep the old prepared handle open")

	newLease, err := manager.acquireRunLease()
	require.NoError(t, err)
	assert.Same(t, preparedB, newLease.PreparedSession())
	assert.Equal(t, "session-b", newLease.SessionID())

	oldLease.CloseRunCommit()
	assert.Equal(t, 0, preparedA.closeCount(), "lease release only returns the reference count")
	newLease.CloseRunCommit()
	require.NoError(t, manager.requestClose(context.Background()))
	assert.Equal(t, 1, preparedA.closeCount())
	assert.Equal(t, 1, preparedB.closeCount())
}

func TestDurableSessionManagerSwapReportsAppliedSelectionOnCloseFailure(t *testing.T) {
	preparedA := newManagerTestPrepared("session-a", "branch-a")
	preparedA.closeErr = errManagerTestClose
	preparedB := newManagerTestPrepared("session-b", "branch-b")
	preparer := newManagerTestPreparer(preparedA)
	preparer.setResume("session-b", preparedB)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))

	err := manager.selectResume(context.Background(), "session-b", "")
	var appliedErr *SelectionAppliedCleanupError
	require.ErrorAs(t, err, &appliedErr)
	assert.ErrorIs(t, err, errManagerTestClose)
	assert.Equal(t, "session-b", appliedErr.SelectedPhysicalSessionID)
	assert.Equal(t, "branch-b", appliedErr.SelectedBranchID)

	lease, err := manager.acquireRunLease()
	require.NoError(t, err)
	assert.Equal(t, "session-b", lease.SessionID())
	lease.CloseRunCommit()
	assert.ErrorIs(t, manager.requestClose(context.Background()), errManagerTestClose)
	assert.Equal(t, 1, preparedA.closeCount())
	assert.Equal(t, 1, preparedB.closeCount())
}

func TestDurableSessionManagerRequestCloseDeadlineClosesAdmissionDuringSelectionDrain(t *testing.T) {
	preparedA := newManagerTestPrepared("session-a", "branch-a")
	preparedB := newManagerTestPrepared("session-b", "branch-b")
	preparer := newManagerTestPreparer(preparedA)
	preparer.setResume("session-b", preparedB)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))
	oldLease, err := manager.acquireRunLease()
	require.NoError(t, err)

	selectionResult := make(chan error, 1)
	go func() { selectionResult <- manager.selectResume(context.Background(), "session-b", "") }()
	waitForManagerLeaseSession(t, manager, "session-b")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	closeResult := make(chan error, 1)
	started := time.Now()
	go func() { closeResult <- manager.requestClose(ctx) }()

	var closeErr error
	returnedBeforeRelease := false
	select {
	case closeErr = <-closeResult:
		returnedBeforeRelease = true
	case <-time.After(500 * time.Millisecond):
	}
	if returnedBeforeRelease {
		assert.ErrorIs(t, closeErr, ErrDurableSessionClosePending)
		assert.ErrorIs(t, closeErr, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 500*time.Millisecond)
		_, acquireErr := manager.acquireRunLease()
		assert.ErrorIs(t, acquireErr, ErrDurableSessionManagerClosing)
	}

	oldLease.CloseRunCommit()
	select {
	case selectErr := <-selectionResult:
		assert.NoError(t, selectErr)
	case <-time.After(5 * time.Second):
		t.Fatal("selection did not finish after the old run lease was returned")
	}
	if !returnedBeforeRelease {
		select {
		case closeErr = <-closeResult:
		case <-time.After(5 * time.Second):
			t.Fatal("close request remained blocked after selection completed")
		}
	}

	require.True(t, returnedBeforeRelease, "requestClose ignored its deadline while waiting for selection")
	require.ErrorIs(t, closeErr, ErrDurableSessionClosePending)
	require.NoError(t, manager.requestClose(context.Background()))
	assert.Equal(t, 1, preparedA.closeCount())
	assert.Equal(t, 1, preparedB.closeCount())
}

func TestDurableSessionManagerRequestCloseDeadlineIsNotBlockedBySnapshot(t *testing.T) {
	prepared := newManagerTestPrepared("session-a", "branch-a")
	manager := newManagerTestManager(t, newManagerTestPreparer(prepared))
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))

	snapshotEntered := make(chan struct{})
	snapshotRelease := make(chan struct{})
	prepared.setSnapshotGate(snapshotEntered, snapshotRelease)
	type acquireResult struct {
		lease *durableSessionRunLease
		err   error
	}
	acquired := make(chan acquireResult, 1)
	go func() {
		lease, err := manager.acquireRunLease()
		acquired <- acquireResult{lease: lease, err: err}
	}()
	select {
	case <-snapshotEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("run lease snapshot did not reach its test gate")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	closed := make(chan error, 1)
	started := time.Now()
	go func() { closed <- manager.requestClose(ctx) }()

	var closeErr error
	returnedBeforeRelease := false
	select {
	case closeErr = <-closed:
		returnedBeforeRelease = true
	case <-time.After(500 * time.Millisecond):
	}
	if returnedBeforeRelease {
		assert.ErrorIs(t, closeErr, ErrDurableSessionClosePending)
		assert.ErrorIs(t, closeErr, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 500*time.Millisecond)
		assert.Equal(t, 0, prepared.closeCount(), "the temporary snapshot borrow must keep the handle open")
		_, err := manager.acquireRunLease()
		assert.ErrorIs(t, err, ErrDurableSessionManagerClosing)
	}

	close(snapshotRelease)
	result := <-acquired
	if result.lease != nil {
		result.lease.CloseRunCommit()
	}
	if !returnedBeforeRelease {
		select {
		case closeErr = <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("close request remained blocked after Snapshot returned")
		}
	}
	require.True(t, returnedBeforeRelease, "requestClose must honor its deadline while Snapshot is blocked")
	assert.Nil(t, result.lease)
	assert.ErrorIs(t, result.err, ErrDurableSessionManagerClosing)
	assert.ErrorIs(t, closeErr, ErrDurableSessionClosePending)
	assert.ErrorIs(t, closeErr, context.DeadlineExceeded)
	require.NoError(t, manager.requestClose(context.Background()))
	assert.Equal(t, 1, prepared.closeCount())
}

func TestDurableSessionManagerDiscardsLatePreparedSessionAfterCloseRequest(t *testing.T) {
	preparedA := newManagerTestPrepared("session-a", "branch-a")
	preparedB := newManagerTestPrepared("session-b", "branch-b")
	preparedB.closeErr = errManagerTestClose
	preparer := newManagerTestPreparer(preparedA)
	preparer.setResume("session-b", preparedB)
	resumeEntered := make(chan struct{})
	resumeRelease := make(chan struct{})
	preparer.setResumeGate("session-b", resumeEntered, resumeRelease)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))
	oldLease, err := manager.acquireRunLease()
	require.NoError(t, err)

	selectionResult := make(chan error, 1)
	go func() { selectionResult <- manager.selectResume(context.Background(), "session-b", "") }()
	select {
	case <-resumeEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("resume preparation did not reach its test gate")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	closeResult := make(chan error, 1)
	go func() { closeResult <- manager.requestClose(ctx) }()
	var closeErr error
	closeReturned := false
	select {
	case closeErr = <-closeResult:
		closeReturned = true
	case <-time.After(500 * time.Millisecond):
	}
	var probe *durableSessionRunLease
	probe, acquireErr := manager.acquireRunLease()
	if probe != nil {
		probe.CloseRunCommit()
	}
	if closeReturned {
		assert.ErrorIs(t, closeErr, ErrDurableSessionClosePending)
		assert.ErrorIs(t, closeErr, context.DeadlineExceeded)
		assert.ErrorIs(t, acquireErr, ErrDurableSessionManagerClosing)
	}

	close(resumeRelease)
	oldLease.CloseRunCommit()
	select {
	case selectionErr := <-selectionResult:
		if closeReturned {
			assert.ErrorIs(t, selectionErr, ErrDurableSessionManagerClosing)
			assert.ErrorIs(t, selectionErr, errManagerTestClose)
		} else {
			assert.NoError(t, selectionErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("selection did not release its uncommitted prepared handle")
	}
	if !closeReturned {
		select {
		case closeErr = <-closeResult:
		case <-time.After(5 * time.Second):
			t.Fatal("close request remained blocked after its transition completed")
		}
	}
	manager.mu.Lock()
	selectedID := ""
	if manager.current != nil {
		selectedID = manager.current.physical
	}
	manager.mu.Unlock()
	if closeReturned {
		assert.Equal(t, "session-a", selectedID, "a late prepared target must not replace the current session")
		assert.Equal(t, 0, preparedA.closeCount())
		assert.Equal(t, 1, preparedB.closeCount(), "the unpublished handle must be closed on the preparing caller path")
		require.ErrorIs(t, manager.requestClose(context.Background()), errManagerTestClose)
	} else {
		assert.ErrorIs(t, closeErr, ErrDurableSessionClosePending)
	}
	assert.Equal(t, 1, preparedA.closeCount())
	assert.Equal(t, 1, preparedB.closeCount(), "a failed close is retained and not retried")
}

func TestDurableSessionManagerCloseRequestRejectsNewLeasesAndReturnsCloseError(t *testing.T) {
	prepared := newManagerTestPrepared("session-a", "branch-main")
	prepared.closeErr = errManagerTestClose
	preparer := newManagerTestPreparer(prepared)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))
	lease, err := manager.acquireRunLease()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = manager.requestClose(ctx)
	assert.ErrorIs(t, err, ErrDurableSessionClosePending)
	assert.Equal(t, 0, prepared.closeCount())
	_, err = manager.acquireRunLease()
	assert.ErrorIs(t, err, ErrDurableSessionManagerClosing)

	lease.CloseRunCommit()
	lease.CloseRunCommit()
	assert.Equal(t, 0, prepared.closeCount(), "CloseRunCommit must not close the manager-owned handle")
	err = manager.requestClose(context.Background())
	assert.ErrorIs(t, err, errManagerTestClose)
	assert.Equal(t, 1, prepared.closeCount())

	err = manager.requestClose(context.Background())
	assert.ErrorIs(t, err, errManagerTestClose)
	assert.Equal(t, 1, prepared.closeCount(), "PreparedSession.Close must be called once")
}

func TestDurableSessionManagerWaitsForLeasesBeforeClosingOnRequesterPath(t *testing.T) {
	prepared := newManagerTestPrepared("session-a", "branch-main")
	prepared.closeErr = errManagerTestClose
	manager := newManagerTestManager(t, newManagerTestPreparer(prepared))
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))
	lease, err := manager.acquireRunLease()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	closeResult := make(chan error, 1)
	go func() { closeResult <- manager.requestClose(ctx) }()

	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		probe, probeErr := manager.acquireRunLease()
		if errors.Is(probeErr, ErrDurableSessionManagerClosing) {
			break
		}
		if probeErr != nil {
			t.Fatalf("unexpected lease probe error: %v", probeErr)
		}
		probe.CloseRunCommit()
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("close request did not reject new run leases")
		}
	}
	assert.Equal(t, 0, prepared.closeCount(), "the requester must wait while a run lease is live")
	lease.CloseRunCommit()

	select {
	case err := <-closeResult:
		assert.ErrorIs(t, err, errManagerTestClose)
	case <-deadline.C:
		t.Fatal("close requester did not resume after the run lease returned")
	}
	assert.Equal(t, 1, prepared.closeCount())
}

func TestDurableSessionManagerPoisonRequiresExactReopenBeforeNextLease(t *testing.T) {
	prepared := newManagerTestPrepared("session-a", "branch-main")
	reopened := newManagerTestPrepared("session-a", "branch-main")
	preparer := newManagerTestPreparer(prepared)
	preparer.setResume("session-a", reopened)
	manager := newManagerTestManager(t, preparer)
	require.NoError(t, manager.prepareNew(context.Background(), "display label"))
	prepared.setSnapshotError(errManagerTestPoison)

	_, err := manager.acquireRunLease()
	assert.ErrorIs(t, err, ErrDurableSessionUnavailable)
	assert.Equal(t, 0, prepared.closeCount())

	err = manager.selectResume(context.Background(), "session-a", "branch-main")
	assert.ErrorIs(t, err, ErrDurableSessionUnavailable)
	assert.Empty(t, preparer.resumeCalls(), "same-ID selection must not self-deadlock by reacquiring its lock")

	require.NoError(t, manager.reopenPoisoned(context.Background()))
	assert.Equal(t, []string{"session-a"}, preparer.resumeCalls())
	assert.Equal(t, 1, prepared.closeCount())

	lease, err := manager.acquireRunLease()
	require.NoError(t, err)
	assert.Same(t, reopened, lease.PreparedSession())
	lease.CloseRunCommit()
	require.NoError(t, manager.requestClose(context.Background()))
}

func newManagerTestManager(t *testing.T, preparer session.SessionPreparer) *durableSessionManager {
	t.Helper()
	manager, err := newDurableSessionManager(preparer, "workspace-v1-test")
	require.NoError(t, err)
	return manager
}

func newManagerTestPrepared(sessionID, branchID string) *managerTestPrepared {
	return &managerTestPrepared{snapshot: session.PreparedSessionSnapshot{
		SessionID:      sessionID,
		ActiveBranchID: branchID,
	}}
}

type managerTestPrepared struct {
	mu           sync.Mutex
	snapshot     session.PreparedSessionSnapshot
	snapshotErr  error
	snapshotGate *managerTestSnapshotGate
	closeErr     error
	closeCalls   int
}

type managerTestSnapshotGate struct {
	entered chan struct{}
	release <-chan struct{}
}

func (prepared *managerTestPrepared) View() session.SessionView {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	return prepared.snapshot.PublishedView.Clone()
}

func (prepared *managerTestPrepared) Snapshot() (session.PreparedSessionSnapshot, error) {
	prepared.mu.Lock()
	snapshot := prepared.snapshot.Clone()
	err := prepared.snapshotErr
	gate := prepared.snapshotGate
	prepared.snapshotGate = nil
	prepared.mu.Unlock()
	if gate != nil {
		close(gate.entered)
		<-gate.release
	}
	if err != nil {
		return session.PreparedSessionSnapshot{}, err
	}
	return snapshot, nil
}

func (*managerTestPrepared) DurableWriter() session.DurableSessionWriter { return nil }

func (prepared *managerTestPrepared) Close() error {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	prepared.closeCalls++
	return prepared.closeErr
}

func (prepared *managerTestPrepared) setSnapshotError(err error) {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	prepared.snapshotErr = err
}

func (prepared *managerTestPrepared) setSnapshotGate(entered chan struct{}, release <-chan struct{}) {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	prepared.snapshotGate = &managerTestSnapshotGate{entered: entered, release: release}
}

func (prepared *managerTestPrepared) closeCount() int {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	return prepared.closeCalls
}

type managerTestPreparer struct {
	mu             sync.Mutex
	newSession     session.PreparedSession
	newErr         error
	resumeSessions map[string]session.PreparedSession
	resumeErrors   map[string]error
	resumeCallLog  []string
	afterResume    func(string)
	resumeGate     managerTestResumeGate
}

type managerTestResumeGate struct {
	sessionID string
	entered   chan struct{}
	release   <-chan struct{}
}

func newManagerTestPreparer(newSession session.PreparedSession) *managerTestPreparer {
	return &managerTestPreparer{
		newSession:     newSession,
		resumeSessions: make(map[string]session.PreparedSession),
		resumeErrors:   make(map[string]error),
	}
}

func (preparer *managerTestPreparer) PrepareNew(context.Context, string) (session.PreparedSession, error) {
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	return preparer.newSession, preparer.newErr
}

func (preparer *managerTestPreparer) PrepareResume(ctx context.Context, physicalSessionID string) (session.PreparedSession, error) {
	preparer.mu.Lock()
	preparer.resumeCallLog = append(preparer.resumeCallLog, physicalSessionID)
	if err := preparer.resumeErrors[physicalSessionID]; err != nil {
		preparer.mu.Unlock()
		return nil, err
	}
	prepared := preparer.resumeSessions[physicalSessionID]
	afterResume := preparer.afterResume
	gate := preparer.resumeGate
	preparer.mu.Unlock()
	if prepared == nil {
		return nil, errors.New("unexpected physical session ID")
	}
	if afterResume != nil {
		afterResume(physicalSessionID)
	}
	if gate.sessionID == physicalSessionID {
		close(gate.entered)
		select {
		case <-gate.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return prepared, nil
}

func (preparer *managerTestPreparer) setResume(physicalSessionID string, prepared session.PreparedSession) {
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	preparer.resumeSessions[physicalSessionID] = prepared
}

func (preparer *managerTestPreparer) setResumeError(physicalSessionID string, err error) {
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	preparer.resumeErrors[physicalSessionID] = err
}

func (preparer *managerTestPreparer) setAfterResume(afterResume func(string)) {
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	preparer.afterResume = afterResume
}

func (preparer *managerTestPreparer) setResumeGate(sessionID string, entered chan struct{}, release <-chan struct{}) {
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	preparer.resumeGate = managerTestResumeGate{sessionID: sessionID, entered: entered, release: release}
}

func (preparer *managerTestPreparer) resumeCalls() []string {
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	return append([]string(nil), preparer.resumeCallLog...)
}

func waitForManagerLeaseSession(t *testing.T, manager *durableSessionManager, sessionID string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		lease, err := manager.acquireRunLease()
		if err == nil {
			if lease.SessionID() == sessionID {
				lease.CloseRunCommit()
				return
			}
			lease.CloseRunCommit()
		} else if !errors.Is(err, ErrNoDurableSessionSelected) {
			t.Fatalf("failed to acquire lease while waiting for %q: %v", sessionID, err)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("session %q was not selected before the deadline", sessionID)
		}
	}
}
