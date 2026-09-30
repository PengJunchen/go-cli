package approval

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/privatefile"
)

func TestProjectTrustGenerationInvalidatesAcrossManagersAndRegrant(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storePath := privateTrustStorePath(t)
	snapshot := testProjectSnapshot(t, root, []byte("project config"), nil, nil)
	first := NewDefaultTrustManager(NewFileTrustStore(storePath)).(*DefaultTrustManager)
	second := NewDefaultTrustManager(NewFileTrustStore(storePath)).(*DefaultTrustManager)

	require.NoError(t, first.TrustProjectSourceSnapshot(ctx, snapshot))
	old, err := second.CheckProjectSourceSnapshot(ctx, snapshot)
	require.NoError(t, err)
	require.Equal(t, uint64(1), old.TrustGeneration())

	require.NoError(t, second.RevokeTrust(ctx, snapshot.CanonicalRoot()))
	marked := false
	err = old.CommitIfCurrent(ctx, func() error {
		marked = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, marked, "a revoke serialized first must prevent the commit marker")

	require.NoError(t, first.TrustProjectSourceSnapshot(ctx, snapshot))
	current, err := second.CheckProjectSourceSnapshot(ctx, snapshot)
	require.NoError(t, err)
	require.Equal(t, uint64(3), current.TrustGeneration())

	marked = false
	err = old.CommitIfCurrent(ctx, func() error {
		marked = true
		return nil
	})
	require.Error(t, err, "same manifest bytes after regrant must not make the old capability current")
	assert.False(t, marked)
	require.NoError(t, current.CommitIfCurrent(ctx, func() error {
		marked = true
		return nil
	}))
	assert.True(t, marked)
}

func TestProjectTrustCheckTreatsLegacyManifestAsGenerationOneWithoutRewrite(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storePath := privateTrustStorePath(t)
	snapshot := testProjectSnapshot(t, root, []byte("legacy full manifest"), nil, nil)
	store := NewFileTrustStore(storePath)
	entry := TrustEntry{
		Path:                     snapshot.CanonicalRoot(),
		CanonicalRoot:            snapshot.CanonicalRoot(),
		ManifestVersion:          snapshot.manifestVersion,
		ProjectSources:           snapshot.SourceRecords(),
		ProjectSourceDirectories: snapshot.DirectoryRecords(),
		ManifestDigest:           snapshot.ManifestDigest(),
	}
	encoded, err := json.Marshal(map[string]TrustEntry{entry.Path: entry})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(storePath, encoded, 0o600))
	before, err := os.ReadFile(storePath)
	require.NoError(t, err)

	manager := NewDefaultTrustManager(store).(*DefaultTrustManager)
	verified, err := manager.CheckProjectSourceSnapshot(ctx, snapshot)
	require.NoError(t, err)
	require.Equal(t, uint64(1), verified.TrustGeneration())
	after, err := os.ReadFile(storePath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "lazy generation-1 migration must not rewrite on read")
}

func TestProjectTrustGenerationRevokeWritesTombstoneAndLegacyTrustCannotMintBinding(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	snapshot := testProjectSnapshot(t, root, []byte("project config"), nil, nil)
	store := NewInMemoryTrustStore()
	manager := NewDefaultTrustManager(store).(*DefaultTrustManager)

	require.NoError(t, store.Add(snapshot.CanonicalRoot(), TrustEntry{Path: snapshot.CanonicalRoot(), Fingerprint: "legacy"}))
	_, err := manager.CheckProjectSourceSnapshot(ctx, snapshot)
	require.ErrorIs(t, err, ErrProjectSourceLegacy)

	require.NoError(t, manager.TrustProjectSourceSnapshot(ctx, snapshot))
	entries, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, uint64(1), entries[snapshot.CanonicalRoot()].TrustGeneration)

	require.NoError(t, manager.RevokeTrust(ctx, snapshot.CanonicalRoot()))
	entries, err = store.Load()
	require.NoError(t, err)
	tombstone := entries[snapshot.CanonicalRoot()]
	assert.Equal(t, uint64(2), tombstone.TrustGeneration)
	assert.True(t, tombstone.Revoked)
	assert.Equal(t, snapshot.CanonicalRoot(), tombstone.Path)
	assert.Equal(t, snapshot.CanonicalRoot(), tombstone.CanonicalRoot)
	assert.Equal(t, 0, tombstone.ManifestVersion)
	assert.Empty(t, tombstone.ManifestDigest)
	expiry, parseErr := time.Parse(time.RFC3339, tombstone.ExpiresAt)
	require.NoError(t, parseErr)
	assert.True(t, expiry.Before(time.Now()), "tombstone expiry remains compatible with old readers")
	assert.NotContains(t, manager.TrustedProjects(), snapshot.CanonicalRoot())

	require.NoError(t, manager.TrustProjectSourceSnapshot(ctx, snapshot))
	entries, err = store.Load()
	require.NoError(t, err)
	assert.Equal(t, uint64(3), entries[snapshot.CanonicalRoot()].TrustGeneration)
	assert.False(t, entries[snapshot.CanonicalRoot()].Revoked)
}

func TestVersionedTrustStoreOperationsPreserveMonotonicEpochs(t *testing.T) {
	root := "/repo/versioned"
	store := NewInMemoryTrustStore()
	active := TrustEntry{Path: root, CanonicalRoot: root, ManifestVersion: projectSourceManifestVersion, ManifestDigest: "a", TrustGeneration: 4}
	require.NoError(t, store.Add(root, active))

	changedAtSameGeneration := active
	changedAtSameGeneration.ManifestDigest = "b"
	assert.Error(t, store.Add(root, changedAtSameGeneration), "same-generation replacement must be rejected")
	assert.Error(t, store.Add(root, TrustEntry{Path: root, TrustGeneration: 3}), "lower-generation replacement must be rejected")
	assert.NoError(t, store.Add(root, active), "identical same-generation replay is idempotent")

	require.NoError(t, store.Remove(root))
	entries, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, uint64(5), entries[root].TrustGeneration)
	assert.True(t, entries[root].Revoked)

	assert.Error(t, store.Save(map[string]TrustEntry{}), "whole-store replace cannot erase a generation tombstone")
	assert.Error(t, store.Add(root, TrustEntry{Path: root, TrustGeneration: 4}), "generation cannot be lowered over tombstone")
}

func TestTrustRevokeCanonicalizesAliasAndDeletedRoot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	alias := filepath.Join(t.TempDir(), "workspace-link")
	require.NoError(t, os.Symlink(root, alias))
	snapshot := testProjectSnapshot(t, root, []byte("config"), nil, nil)
	store := NewInMemoryTrustStore()
	manager := NewDefaultTrustManager(store).(*DefaultTrustManager)
	require.NoError(t, manager.TrustProjectSourceSnapshot(ctx, snapshot))

	entries, err := store.Load()
	require.NoError(t, err)
	entry := TrustEntry{Path: alias, Fingerprint: "legacy alias"}
	require.NoError(t, store.Add(alias, entry))
	require.NoError(t, manager.RevokeTrust(ctx, alias))
	entries, err = store.Load()
	require.NoError(t, err)
	assert.NotContains(t, entries, alias, "legacy caller-path aliases must be removed transactionally")
	assert.True(t, entries[snapshot.CanonicalRoot()].Revoked)
	assert.Equal(t, uint64(2), entries[snapshot.CanonicalRoot()].TrustGeneration)

	deletedParent, err := canonicalProjectRoot(t.TempDir())
	require.NoError(t, err)
	deletedRoot := filepath.Join(deletedParent, "removed-project")
	require.NoError(t, os.Mkdir(deletedRoot, 0o700))
	deletedSnapshot := testProjectSnapshot(t, deletedRoot, []byte("deleted config"), nil, nil)
	require.NoError(t, manager.TrustProjectSourceSnapshot(ctx, deletedSnapshot))
	require.NoError(t, os.RemoveAll(deletedRoot))
	require.NoError(t, manager.RevokeTrust(ctx, deletedRoot))
	entries, err = store.Load()
	require.NoError(t, err)
	assert.True(t, entries[deletedRoot].Revoked)
	assert.Equal(t, uint64(2), entries[deletedRoot].TrustGeneration)
}

func TestFileTrustStoreUpdateSerializesIndependentProcesses(t *testing.T) {
	if path := os.Getenv("GO_CLI_TRUST_STORE_HELPER_PATH"); path != "" {
		root := os.Getenv("GO_CLI_TRUST_STORE_HELPER_ROOT")
		if err := NewFileTrustStore(path).Add(root, TrustEntry{Path: root}); err != nil {
			_, _ = os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		os.Exit(0)
	}

	storePath := privateTrustStorePath(t)
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- privatefile.WithExclusiveLock(context.Background(), storePath, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	startHelper := func(root string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestFileTrustStoreUpdateSerializesIndependentProcesses$")
		cmd.Env = append(os.Environ(), "GO_CLI_TRUST_STORE_HELPER_PATH="+storePath, "GO_CLI_TRUST_STORE_HELPER_ROOT="+root)
		return cmd
	}
	first := startHelper("/repo/first")
	second := startHelper("/repo/second")
	require.NoError(t, first.Start())
	require.NoError(t, second.Start())

	completedBeforeRelease := make(chan string, 2)
	go func() { _ = first.Wait(); completedBeforeRelease <- "first" }()
	go func() { _ = second.Wait(); completedBeforeRelease <- "second" }()
	select {
	case process := <-completedBeforeRelease:
		release <- struct{}{}
		<-lockDone
		t.Fatalf("%s store write bypassed the stable OS lock", process)
	case <-time.After(120 * time.Millisecond):
	}

	release <- struct{}{}
	require.NoError(t, <-lockDone)
	completed := map[string]bool{}
	for range 2 {
		select {
		case process := <-completedBeforeRelease:
			completed[process] = true
		case <-time.After(5 * time.Second):
			t.Fatal("subprocess trust-store updates did not finish after lock release")
		}
	}
	entries, err := NewFileTrustStore(storePath).Load()
	require.NoError(t, err)
	assert.Contains(t, entries, "/repo/first")
	assert.Contains(t, entries, "/repo/second")
}

func TestVerifiedProjectCommitCancellationWhileWaitingForSidecarLock(t *testing.T) {
	storePath := privateTrustStorePath(t)
	root := t.TempDir()
	snapshot := testProjectSnapshot(t, root, []byte("commit cancellation"), nil, nil)
	manager := NewDefaultTrustManager(NewFileTrustStore(storePath)).(*DefaultTrustManager)
	require.NoError(t, manager.TrustProjectSourceSnapshot(context.Background(), snapshot))
	binding, err := manager.CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.NoError(t, err)

	locked := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan struct{})
	var holderErr error
	go func() {
		holderErr = privatefile.WithExclusiveLock(context.Background(), storePath, func() error {
			close(locked)
			<-release
			return nil
		})
		close(holderDone)
	}()
	<-locked
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
		<-holderDone
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var marked bool
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- binding.CommitIfCurrent(ctx, func() error {
			marked = true
			return nil
		})
	}()
	// The holder already owns the sidecar, so even if scheduling delays the
	// waiter the callback cannot run. Cancellation must release that waiter.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-commitDone:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("CommitIfCurrent did not cancel while waiting for the trust-store sidecar")
	}
	assert.False(t, marked, "a canceled lock wait cannot run the dispatch marker")
	close(release)
	released = true
	<-holderDone
	require.NoError(t, holderErr)
}

func TestMalformedAndOverflowTrustGenerationsFailClosed(t *testing.T) {
	root := t.TempDir()
	snapshot := testProjectSnapshot(t, root, []byte("config"), nil, nil)
	store := NewInMemoryTrustStore()
	manager := NewDefaultTrustManager(store).(*DefaultTrustManager)
	require.NoError(t, store.Add(snapshot.CanonicalRoot(), TrustEntry{
		Path: snapshot.CanonicalRoot(), CanonicalRoot: snapshot.CanonicalRoot(),
		ManifestVersion: snapshot.manifestVersion, ProjectSources: snapshot.SourceRecords(),
		ProjectSourceDirectories: snapshot.DirectoryRecords(), ManifestDigest: snapshot.ManifestDigest(),
		TrustGeneration: math.MaxUint64,
	}))
	_, err := manager.CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.NoError(t, err, "maximum nonzero epoch remains readable until an advancing operation is requested")
	assert.Error(t, manager.TrustProjectSourceSnapshot(context.Background(), snapshot))
	assert.Error(t, manager.RevokeTrust(context.Background(), snapshot.CanonicalRoot()))

	entries, err := store.Load()
	require.NoError(t, err)
	assert.Equal(t, uint64(math.MaxUint64), entries[snapshot.CanonicalRoot()].TrustGeneration)
	assert.False(t, entries[snapshot.CanonicalRoot()].Revoked)

	storePath := privateTrustStorePath(t)
	malformed := TrustEntry{Path: snapshot.CanonicalRoot(), CanonicalRoot: snapshot.CanonicalRoot(), Revoked: true}
	encoded, err := json.Marshal(map[string]TrustEntry{snapshot.CanonicalRoot(): malformed})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(storePath, encoded, 0o600))
	_, err = NewDefaultTrustManager(NewFileTrustStore(storePath)).(*DefaultTrustManager).CheckProjectSourceSnapshot(context.Background(), snapshot)
	require.Error(t, err)
	assert.False(t, strings.Contains(strings.ToLower(err.Error()), "panic"))
	assert.False(t, errors.Is(err, context.Canceled))
}
