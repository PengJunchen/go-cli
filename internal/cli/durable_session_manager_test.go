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
	assert.Equal(t, "workspace-v1:"+hex.EncodeToString(digest[:]), workspaceID)

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
	assert.Equal(t, "workspace-v1:test", lease.WorkspaceID())
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
	manager, err := newDurableSessionManager(preparer, "workspace-v1:test")
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
	mu          sync.Mutex
	snapshot    session.PreparedSessionSnapshot
	snapshotErr error
	closeErr    error
	closeCalls  int
}

func (prepared *managerTestPrepared) View() session.SessionView {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	return prepared.snapshot.PublishedView.Clone()
}

func (prepared *managerTestPrepared) Snapshot() (session.PreparedSessionSnapshot, error) {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.snapshotErr != nil {
		return session.PreparedSessionSnapshot{}, prepared.snapshotErr
	}
	return prepared.snapshot.Clone(), nil
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

func (preparer *managerTestPreparer) PrepareResume(_ context.Context, physicalSessionID string) (session.PreparedSession, error) {
	preparer.mu.Lock()
	preparer.resumeCallLog = append(preparer.resumeCallLog, physicalSessionID)
	if err := preparer.resumeErrors[physicalSessionID]; err != nil {
		preparer.mu.Unlock()
		return nil, err
	}
	prepared := preparer.resumeSessions[physicalSessionID]
	afterResume := preparer.afterResume
	preparer.mu.Unlock()
	if prepared == nil {
		return nil, errors.New("unexpected physical session ID")
	}
	if afterResume != nil {
		afterResume(physicalSessionID)
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

func (preparer *managerTestPreparer) resumeCalls() []string {
	preparer.mu.Lock()
	defer preparer.mu.Unlock()
	return append([]string(nil), preparer.resumeCallLog...)
}
