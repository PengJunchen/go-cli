package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/verify"
)

type leaseReleaseTestAgent struct{}

func (leaseReleaseTestAgent) Name() string { return "lease-release-test-agent" }

func (leaseReleaseTestAgent) Run(context.Context, core.Submission, ...core.EventStream) (core.Result, error) {
	return core.Result{Message: "completed"}, nil
}

func TestDurableCLISessionCreatesPhysicalSessionAndBindsRunLease(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	ctx := context.Background()
	storeDirectory := filepath.Join(t.TempDir(), "session-store")
	cliSession, err := newDurableCLISession(ctx, storeDirectory, "display-label", "", "")
	require.NoError(t, err)

	expectedWorkspaceID, err := resolveWorkspaceID(os.Getwd)
	require.NoError(t, err)
	snapshot, err := cliSession.snapshotSelected()
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.SessionID)
	assert.NotEqual(t, "display-label", snapshot.SessionID)
	assert.Equal(t, "display-label", snapshot.SessionLabel)
	assert.Equal(t, "main", snapshot.ActiveBranchID)
	assert.Equal(t, snapshot.SessionID, snapshot.PublishedView.SessionID)
	assert.Equal(t, snapshot.ActiveBranchID, snapshot.PublishedView.BranchID)
	assert.Equal(t, expectedWorkspaceID, cliSession.workspaceID)
	_, err = os.Stat(filepath.Join(storeDirectory, "sessions", snapshot.SessionID+".jsonl"))
	require.NoError(t, err)

	runCtx, sink, err := cliSession.bindRun(ctx)
	require.NoError(t, err)
	require.NotNil(t, sink)
	boundSink, binding, ok := core.RunCommitFromContext(runCtx)
	require.True(t, ok)
	assert.Same(t, sink, boundSink)
	assert.Equal(t, snapshot.SessionID, binding.SessionID)
	assert.Equal(t, expectedWorkspaceID, binding.WorkspaceID)
	assert.Empty(t, binding.OwnerGeneration, "the run owner fills its own generation")
	_, ok = any(sink).(core.RunCommitRunLease)
	assert.True(t, ok, "the run sink must return the helper's lease to the core owner")

	closeCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err = cliSession.requestClose(closeCtx)
	require.ErrorIs(t, err, ErrDurableSessionClosePending)
	_, err = cliSession.manager.acquireRunLease()
	assert.ErrorIs(t, err, ErrDurableSessionManagerClosing)

	probeCtx, probeCancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer probeCancel()
	probe := session.NewDurableJSONLSessionPreparer(storeDirectory, cliSession.index)
	_, err = probe.PrepareResume(probeCtx, snapshot.SessionID)
	assert.ErrorIs(t, err, context.DeadlineExceeded, "pending close must retain the physical session lock")

	sink.CloseRunCommit()
	sink.CloseRunCommit()
	require.NoError(t, cliSession.requestClose(ctx))
	prepared, err := probe.PrepareResume(ctx, snapshot.SessionID)
	require.NoError(t, err, "the prepared handle closes after the run lease returns")
	require.NoError(t, prepared.Close())
}

func TestCoreEntrypointReturnsRealDurableManagerLease(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	ctx := context.Background()
	cliSession, err := newDurableCLISession(ctx, filepath.Join(t.TempDir(), "session-store"), "display-label", "", "")
	require.NoError(t, err)
	runCtx, sink, err := cliSession.bindRun(ctx)
	require.NoError(t, err)
	require.NotNil(t, sink)

	harness := core.NewHarnessImpl(leaseReleaseTestAgent{})
	stream, err := harness.Submit(runCtx, "hello")
	require.NoError(t, err)
	for range stream.Events() {
	}
	_, runErr := stream.Result()
	require.ErrorIs(t, runErr, core.ErrRunCommitTerminalNotSynced,
		"this lease integration test does not claim that a plain Agent is a durable transcript consumer")

	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	require.NoError(t, cliSession.requestClose(closeCtx),
		"the core entrypoint must return the actual manager borrow after owner and wrapper cleanup")
}

func TestDurableCLISessionResumeUsesExactPhysicalIDAndBranch(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	ctx := context.Background()
	storeDirectory := filepath.Join(t.TempDir(), "session-store")

	first, err := newDurableCLISession(ctx, storeDirectory, "first-display", "", "")
	require.NoError(t, err)
	firstSnapshot, err := first.snapshotSelected()
	require.NoError(t, err)
	require.NoError(t, first.requestClose(ctx))

	second, err := newDurableCLISession(ctx, storeDirectory, "second-display", "", "")
	require.NoError(t, err)
	secondSnapshot, err := second.snapshotSelected()
	require.NoError(t, err)
	require.NoError(t, second.requestClose(ctx))
	assert.NotEqual(t, firstSnapshot.SessionID, secondSnapshot.SessionID)

	resumed, err := newDurableCLISession(ctx, storeDirectory, "resume-display", firstSnapshot.SessionID, firstSnapshot.ActiveBranchID)
	require.NoError(t, err)
	resumedSnapshot, err := resumed.snapshotSelected()
	require.NoError(t, err)
	assert.Equal(t, firstSnapshot.SessionID, resumedSnapshot.SessionID)
	assert.Equal(t, firstSnapshot.ActiveBranchID, resumedSnapshot.ActiveBranchID)

	runCtx, sink, err := resumed.bindRun(ctx)
	require.NoError(t, err)
	_, binding, ok := core.RunCommitFromContext(runCtx)
	require.True(t, ok)
	assert.Equal(t, firstSnapshot.SessionID, binding.SessionID)
	sink.CloseRunCommit()
	require.NoError(t, resumed.requestClose(ctx))

	_, err = newDurableCLISession(ctx, storeDirectory, "unsupported-branch", firstSnapshot.SessionID, "branch-other")
	require.ErrorIs(t, err, ErrPreparedSessionBranchSelectionUnsupported)

	reopened, err := newDurableCLISession(ctx, storeDirectory, "reopened-display", firstSnapshot.SessionID, "")
	require.NoError(t, err, "rejected branch selection must release its prepared handle")
	require.NoError(t, reopened.requestClose(ctx))
}

func TestDurableCLISessionReleasesLeaseWhenSinkConstructionFails(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	ctx := context.Background()
	cliSession, err := newDurableCLISession(ctx, filepath.Join(t.TempDir(), "session-store"), "display", "", "")
	require.NoError(t, err)
	lease, err := cliSession.manager.acquireRunLease()
	require.NoError(t, err)

	runCtx, sink, err := bindDurableSessionRun(ctx, lease, nil)
	require.Error(t, err)
	assert.Nil(t, runCtx)
	assert.Nil(t, sink)

	require.NoError(t, cliSession.requestClose(ctx), "sink-construction failure must return the lease")
	prepared, err := session.NewDurableJSONLSessionPreparer(cliSession.storeDirectory, cliSession.index).PrepareResume(ctx, lease.SessionID())
	require.NoError(t, err, "constructor failure must not strand the prepared file lock")
	require.NoError(t, prepared.Close())
}

func TestNewDurableCLISessionRejectsMissingSelectionInputs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	configureDurableCLISessionTest(t)
	_, err := newDurableCLISession(context.Background(), "", "display", "", "")
	require.Error(t, err)

	_, err = newDurableCLISession(context.Background(), t.TempDir(), "display", "", "branch-a")
	require.ErrorIs(t, err, errDurableSessionBranchRequiresPhysicalID)
}

func configureDurableCLISessionTest(t *testing.T) {
	t.Helper()
	configRoot := t.TempDir()
	t.Setenv("HOME", configRoot)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(configRoot, ".config"))
}
