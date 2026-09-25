package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestWorkspaceIndexEnsureCreatesSecureFiles(t *testing.T) {
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	if err := index.EnsureWorkspace(context.Background(), "workspace-test"); err != nil {
		t.Fatal(err)
	}
	indexDir := filepath.Join(index.root, "go-cli", "workspace-quarantine")
	for _, name := range []string{"workspace-test.lock", "workspace-test.jsonl"} {
		info, err := os.Stat(filepath.Join(indexDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %04o, want 0600", name, got)
		}
	}
	if info, err := os.Stat(indexDir); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("workspace index directory mode = %04o, want 0700", got)
	}
}

func TestWorkspaceIndexRejectsSymlinkAndRelaxedModeFiles(t *testing.T) {
	t.Run("index symlink", func(t *testing.T) {
		ctx := context.Background()
		index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
		workspaceID := "workspace-symlink-index"
		if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.jsonl")
		data, err := os.ReadFile(index.indexPath(workspaceID))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(index.indexPath(workspaceID)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, index.indexPath(workspaceID)); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		ref := workspaceTestRef(workspaceID, "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
		if _, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{ref}); !errors.Is(err, ErrWorkspaceIndexUnavailable) {
			t.Fatalf("InspectDispatchMarkers followed index symlink: %v", err)
		}
	})

	t.Run("index permissions", func(t *testing.T) {
		ctx := context.Background()
		index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
		workspaceID := "workspace-relaxed-index"
		if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(index.indexPath(workspaceID), 0o644); err != nil {
			t.Fatal(err)
		}
		ref := workspaceTestRef(workspaceID, "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
		if _, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{ref}); !errors.Is(err, ErrWorkspaceIndexUnavailable) {
			t.Fatalf("InspectDispatchMarkers accepted a relaxed file mode: %v", err)
		}
	})

	t.Run("lock symlink", func(t *testing.T) {
		ctx := context.Background()
		index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
		workspaceID := "workspace-symlink-lock"
		if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside.lock")
		if err := os.WriteFile(outside, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(index.lockPath(workspaceID)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, index.lockPath(workspaceID)); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		ref := workspaceTestRef(workspaceID, "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
		if _, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{ref}); !errors.Is(err, ErrWorkspaceIndexUnavailable) {
			t.Fatalf("InspectDispatchMarkers followed lock symlink: %v", err)
		}
	})
}

func TestWorkspaceIndexMissingAfterLockFailsClosed(t *testing.T) {
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	ctx := context.Background()
	if err := index.EnsureWorkspace(ctx, "workspace-test"); err != nil {
		t.Fatal(err)
	}
	indexPath := index.indexPath("workspace-test")
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := index.EnsureWorkspace(ctx, "workspace-test"); !errors.Is(err, ErrWorkspaceIndexUnavailable) {
		t.Fatalf("EnsureWorkspace after witnessed index loss = %v, want unavailable", err)
	}
	if _, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{workspaceTestRef("workspace-test", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")}); !errors.Is(err, ErrWorkspaceIndexUnavailable) {
		t.Fatalf("InspectDispatchMarkers with missing index = %v, want unavailable", err)
	}
	if got, err := index.BeginDispatchBatch(ctx, workspaceTestBatch("workspace-test", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")); !errors.Is(err, ErrWorkspaceIndexUnavailable) || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("BeginDispatchBatch with missing index = (%v, %v), want fail closed", got, err)
	}
	if _, err := os.Stat(indexPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing index was recreated: stat error = %v", err)
	}
}

func TestWorkspaceIndexInspectionDoesNotBootstrapMissingAuthority(t *testing.T) {
	root := t.TempDir()
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	ref := workspaceTestRef("workspace-missing", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if _, err := index.InspectDispatchMarkers(context.Background(), []PendingActionRef{ref}); !errors.Is(err, ErrWorkspaceIndexUnavailable) {
		t.Fatalf("InspectDispatchMarkers without EnsureWorkspace = %v, want unavailable", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go-cli")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read path implicitly created workspace authority: stat error = %v", err)
	}
}

func TestWorkspaceIndexEnsureRejectsIndexWithoutLockWitness(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	if err := index.EnsureWorkspace(ctx, "workspace-orphan"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(index.lockPath("workspace-orphan")); err != nil {
		t.Fatal(err)
	}
	if err := index.EnsureWorkspace(ctx, "workspace-orphan"); !errors.Is(err, ErrWorkspaceIndexUnavailable) {
		t.Fatalf("EnsureWorkspace with orphaned index = %v, want unavailable", err)
	}
	if _, err := os.Stat(index.lockPath("workspace-orphan")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("EnsureWorkspace recreated missing lock witness: stat error = %v", err)
	}
}

func TestWorkspaceIndexMarkerRoundTripAndResolutionIdempotent(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	batch := workspaceTestBatch("workspace-test", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if err := index.EnsureWorkspace(ctx, batch.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	result, err := index.BeginDispatchBatch(ctx, batch)
	if err != nil || result != WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch = (%v, %v), want newly committed", result, err)
	}
	if got, err := index.BeginDispatchBatch(ctx, batch); err != nil || got != WorkspaceDispatchAlreadyExists {
		t.Fatalf("duplicate BeginDispatchBatch = (%v, %v), want already exists", got, err)
	}

	resolution := workspaceTestResolution(batch.Actions[0].Action, "checkpoint-1", 19, core.ResolutionLocalResourcesStopped, "execution-1")
	if err := index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err != nil {
		t.Fatal(err)
	}
	reopened := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	if err := reopened.EnsureWorkspace(ctx, batch.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err != nil {
		t.Fatalf("identical resolution retry after reopen: %v", err)
	}
	decisions, err := reopened.InspectDispatchMarkers(ctx, []PendingActionRef{batch.Actions[0].Action})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != batch.Actions[0].Action || decisions[0].MarkerState != DispatchMarkerResolved {
		t.Fatalf("resolution decision = %#v, want exact resolved marker", decisions)
	}
	rows, err := readWorkspaceIndexFile(reopened.indexPath(batch.WorkspaceID), filepath.Dir(reopened.indexPath(batch.WorkspaceID)), batch.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(rows); got != 3 {
		t.Fatalf("index has %d rows after duplicate retries, want genesis + marker + one resolution", got)
	}
}

func TestWorkspaceIndexAlreadyExistsNeverGrantsExecutorPermit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	batch := workspaceTestBatch("workspace-no-replay", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if err := index.EnsureWorkspace(ctx, batch.WorkspaceID); err != nil {
		t.Fatal(err)
	}

	executorCalls := 0
	begin := func(index *WorkspaceQuarantineIndex) WorkspaceDispatchResult {
		result, err := index.BeginDispatchBatch(ctx, batch)
		if err != nil {
			t.Fatalf("BeginDispatchBatch: %v", err)
		}
		if result == WorkspaceDispatchCommittedNow {
			// Model the caller's only allowed dispatch branch. The R4 prepared
			// invocation final fence remains a second required gate.
			executorCalls++
		}
		return result
	}
	if got := begin(index); got != WorkspaceDispatchCommittedNow {
		t.Fatalf("first BeginDispatchBatch = %v, want committed-now", got)
	}
	if got := begin(index); got != WorkspaceDispatchAlreadyExists {
		t.Fatalf("duplicate BeginDispatchBatch = %v, want already-exists", got)
	}

	resolution := workspaceTestResolution(batch.Actions[0].Action, "checkpoint-1", 19, core.ResolutionLocalResourcesStopped, "execution-1")
	if err := index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err != nil {
		t.Fatal(err)
	}
	reopened := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	if got := begin(reopened); got != WorkspaceDispatchAlreadyExists {
		t.Fatalf("BeginDispatchBatch for resolved marker = %v, want already-exists without replay", got)
	}
	if executorCalls != 1 {
		t.Fatalf("executor branch called %d times, want only the original committed-now call", executorCalls)
	}
}

func TestWorkspaceIndexNeverDispatchedWithoutMarkerIsIdempotentNoop(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	workspaceID := "workspace-no-marker-resolution"
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	ref := workspaceTestRef(workspaceID, "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	resolution := workspaceTestResolution(ref, "checkpoint-not-dispatched", 11, core.ResolutionNeverDispatched, "receipt-not-dispatched")
	if err := index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err != nil {
		t.Fatalf("validated no-marker never-dispatched checkpoint: %v", err)
	}
	reopened := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	if err := reopened.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err != nil {
		t.Fatalf("idempotent no-marker checkpoint after reopen: %v", err)
	}
	decisions, err := reopened.InspectDispatchMarkers(ctx, []PendingActionRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].MarkerState != DispatchMarkerAbsent {
		t.Fatalf("no-marker never-dispatched resolution created marker state %v", decisions[0].MarkerState)
	}
	rows, err := readWorkspaceIndexFile(reopened.indexPath(workspaceID), filepath.Dir(reopened.indexPath(workspaceID)), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("no-marker never-dispatched checkpoint wrote %d index rows, want only genesis", len(rows))
	}
}

func TestWorkspaceIndexBlocksConcurrentMutationAcrossSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	first := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	second := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	if err := first.EnsureWorkspace(ctx, "workspace-race"); err != nil {
		t.Fatal(err)
	}

	batches := []WorkspaceDispatchBatch{
		workspaceTestBatch("workspace-race", "session-a", "run-a", "batch-a", "action-a", 0, "call-a"),
		workspaceTestBatch("workspace-race", "session-b", "run-b", "batch-b", "action-b", 0, "call-b"),
	}
	indexes := []*WorkspaceQuarantineIndex{first, second}
	start := make(chan struct{})
	type result struct {
		state WorkspaceDispatchResult
		err   error
	}
	results := make([]result, len(batches))
	var wg sync.WaitGroup
	for i := range batches {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i].state, results[i].err = indexes[i].BeginDispatchBatch(ctx, batches[i])
		}(i)
	}
	close(start)
	wg.Wait()

	committed, blocked := 0, 0
	for _, result := range results {
		if result.err == nil && result.state == WorkspaceDispatchCommittedNow {
			committed++
			continue
		}
		if errors.Is(result.err, ErrWorkspaceQuarantined) {
			blocked++
			continue
		}
		t.Fatalf("unexpected concurrent begin result: %#v", result)
	}
	if committed != 1 || blocked != 1 {
		t.Fatalf("committed=%d blocked=%d, want one serialized winner and one quarantine rejection", committed, blocked)
	}
}

func TestWorkspaceIndexSyncFailureMarkerRemainsUnknown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	var failSync atomic.Bool
	var syncCalls atomic.Int32
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{sync: func(file *os.File) error {
		if failSync.Load() && syncCalls.Add(1) == 2 {
			return errors.New("injected index sync failure")
		}
		return file.Sync()
	}})
	batch := workspaceTestBatch("workspace-sync", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if err := index.EnsureWorkspace(ctx, batch.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	syncCalls.Store(0)
	executorCalls := 0
	failSync.Store(true)
	got, beginErr := index.BeginDispatchBatch(ctx, batch)
	if got == WorkspaceDispatchCommittedNow {
		executorCalls++
	}
	if beginErr == nil || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("BeginDispatchBatch after sync error = (%v, %v), want error and no dispatch permit", got, beginErr)
	}
	failSync.Store(false)

	reopened := newWorkspaceQuarantineIndex(root, workspaceIndexIO{})
	decisions, err := reopened.InspectDispatchMarkers(ctx, []PendingActionRef{batch.Actions[0].Action})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].MarkerState != DispatchMarkerPresent {
		t.Fatalf("post-sync-error marker = %#v, want present/unknown", decisions)
	}
	got, beginErr = reopened.BeginDispatchBatch(ctx, batch)
	if got == WorkspaceDispatchCommittedNow {
		executorCalls++
	}
	if beginErr != nil || got != WorkspaceDispatchAlreadyExists {
		t.Fatalf("retry after marker sync error = (%v, %v), want existing marker without dispatch permit", got, beginErr)
	}
	if executorCalls != 0 {
		t.Fatalf("sync-failure retries granted %d executor permits, want zero", executorCalls)
	}
}

func TestWorkspaceIndexResolutionSyncFailureNeedsRetryBeforeRelease(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	var failSync atomic.Bool
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{sync: func(file *os.File) error {
		if failSync.Load() {
			return errors.New("injected index sync failure")
		}
		return file.Sync()
	}})
	batch := workspaceTestBatch("workspace-resolution-sync", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if err := index.EnsureWorkspace(ctx, batch.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if got, err := index.BeginDispatchBatch(ctx, batch); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch = (%v, %v)", got, err)
	}
	resolution := workspaceTestResolution(batch.Actions[0].Action, "checkpoint-1", 23, core.ResolutionLocalResourcesStopped, "execution-1")
	failSync.Store(true)
	if err := index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err == nil {
		t.Fatal("ApplyDurableResolutions unexpectedly acknowledged injected Sync failure")
	}
	if _, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{batch.Actions[0].Action}); err == nil {
		t.Fatal("InspectDispatchMarkers accepted a possibly unsynced resolution row")
	}
	failSync.Store(false)
	if err := index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err != nil {
		t.Fatalf("identical resolution retry should Sync and finish: %v", err)
	}
	decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{batch.Actions[0].Action})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].MarkerState != DispatchMarkerResolved {
		t.Fatalf("marker after durable retry = %v, want resolved", decisions[0].MarkerState)
	}
	followup := workspaceTestBatch(batch.WorkspaceID, "session-b", "run-b", "batch-b", "action-b", 1, "call-b")
	if got, err := index.BeginDispatchBatch(ctx, followup); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("new action after durable resolution = (%v, %v), want committed", got, err)
	}
}

func TestWorkspaceIndexUnreservedOccurrenceIsAbsentOnlyWithoutAnyMarker(t *testing.T) {
	ctx := context.Background()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	workspaceID := "workspace-unreserved"
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	reserved := workspaceTestBatch(workspaceID, "session-a", "run-a", "batch-a", "action-a", 4, "call-a")
	unreserved := workspaceTestRef(workspaceID, "session-a", "run-a", "", "", 4, "call-a")
	if got, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{unreserved}); err != nil || len(got) != 1 || got[0].MarkerState != DispatchMarkerAbsent {
		t.Fatalf("unreserved no-marker decision = (%#v, %v), want absent", got, err)
	}
	if got, err := index.BeginDispatchBatch(ctx, reserved); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch = (%v, %v)", got, err)
	}
	decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{unreserved})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].Action != unreserved || decisions[0].MarkerState != DispatchMarkerUnverifiable {
		t.Fatalf("unreserved marker mismatch decision = %#v, want exact unreserved ref + unverifiable", decisions)
	}
	misplaced := workspaceTestRef(workspaceID, "session-a", "run-a", "batch-a", "action-a", 5, "call-other")
	decisions, err = index.InspectDispatchMarkers(ctx, []PendingActionRef{misplaced})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].MarkerState != DispatchMarkerUnverifiable {
		t.Fatalf("reused ActionID/batch on another occurrence = %v, want unverifiable", decisions[0].MarkerState)
	}
}

func TestWorkspaceIndexReturnsDecisionsInInputOrder(t *testing.T) {
	ctx := context.Background()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	workspaceID := "workspace-order"
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	batch := workspaceTestBatch(workspaceID, "session-a", "run-a", "batch-a", "action-a", 3, "call-a")
	second := workspaceTestRef(workspaceID, "session-a", "run-a", "batch-a", "action-b", 3, "call-b")
	batch.Actions = append(batch.Actions, WorkspaceDispatchAction{Action: second, EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceWrite}})
	if got, err := index.BeginDispatchBatch(ctx, batch); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch = (%v, %v)", got, err)
	}
	refs := []PendingActionRef{second, batch.Actions[0].Action}
	decisions, err := index.InspectDispatchMarkers(ctx, refs)
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != len(refs) || decisions[0].Action != refs[0] || decisions[1].Action != refs[1] || decisions[0].MarkerState != DispatchMarkerPresent || decisions[1].MarkerState != DispatchMarkerPresent {
		t.Fatalf("ordered decisions = %#v, want input order and complete echoed refs", decisions)
	}
}

func TestWorkspaceIndexNeverDispatchedCannotClearMismatchedOccurrenceMarker(t *testing.T) {
	ctx := context.Background()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	workspaceID := "workspace-resolution-mismatch"
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	batch := workspaceTestBatch(workspaceID, "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if got, err := index.BeginDispatchBatch(ctx, batch); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch = (%v, %v)", got, err)
	}
	wrong := workspaceTestRef(workspaceID, "session-a", "run-a", "batch-other", "action-other", 0, "call-a")
	resolution := workspaceTestResolution(wrong, "checkpoint-1", 17, core.ResolutionNeverDispatched, "receipt-1")
	if err := index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err == nil {
		t.Fatal("never-dispatched proof with a mismatched marker unexpectedly succeeded")
	}
	decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{batch.Actions[0].Action})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].MarkerState != DispatchMarkerPresent {
		t.Fatalf("mismatched proof cleared quarantine: marker state = %v", decisions[0].MarkerState)
	}
}

func TestWorkspaceIndexRejectsUnknownRemoteResolutionEvidence(t *testing.T) {
	ctx := context.Background()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	batch := workspaceTestBatch("workspace-remote-unknown", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if err := index.EnsureWorkspace(ctx, batch.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if got, err := index.BeginDispatchBatch(ctx, batch); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch = (%v, %v)", got, err)
	}
	resolution := workspaceTestResolution(batch.Actions[0].Action, "checkpoint-unknown", 19, core.ExecutionResolutionEvidence("remote_unknown"), "callback-returned")
	if err := index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err == nil {
		t.Fatal("unknown remote callback evidence unexpectedly resolved quarantine")
	}
	decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{batch.Actions[0].Action})
	if err != nil {
		t.Fatal(err)
	}
	if decisions[0].MarkerState != DispatchMarkerPresent {
		t.Fatalf("unknown remote evidence changed marker state to %v", decisions[0].MarkerState)
	}
}

func TestWorkspaceIndexMalformedCompleteRecordFailsClosed(t *testing.T) {
	ctx := context.Background()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	workspaceID := "workspace-malformed"
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(index.indexPath(workspaceID), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("{not-json}\n")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{workspaceTestRef(workspaceID, "session-a", "run-a", "batch-a", "action-a", 0, "call-a")}); err == nil {
		t.Fatal("InspectDispatchMarkers accepted a malformed complete record")
	}
}

func TestWorkspaceIndexUnresolvedMarkerBlocksAllNewActionsWithoutScopeProof(t *testing.T) {
	ctx := context.Background()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	workspaceID := "workspace-quarantine"
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	first := workspaceTestBatch(workspaceID, "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if got, err := index.BeginDispatchBatch(ctx, first); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("first BeginDispatchBatch = (%v, %v)", got, err)
	}
	read := workspaceTestBatch(workspaceID, "session-b", "run-b", "batch-b", "action-b", 0, "call-b")
	read.Actions[0].EffectSet = []tools.TrustedToolEffect{tools.EffectWorkspaceRead}
	if got, err := index.BeginDispatchBatch(ctx, read); !errors.Is(err, ErrWorkspaceQuarantined) || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("read without trusted path-scope proof = (%v, %v), want quarantine", got, err)
	}
}

func workspaceTestBatch(workspaceID, sessionID, runID, batchID, actionID string, ordinal uint64, toolCallID string) WorkspaceDispatchBatch {
	ref := workspaceTestRef(workspaceID, sessionID, runID, batchID, actionID, ordinal, toolCallID)
	return WorkspaceDispatchBatch{
		WorkspaceID:   workspaceID,
		SessionID:     sessionID,
		RunID:         runID,
		ActionBatchID: batchID,
		Actions: []WorkspaceDispatchAction{{
			Action:    ref,
			EffectSet: []tools.TrustedToolEffect{tools.EffectUnknown},
		}},
	}
}

func workspaceTestRef(workspaceID, sessionID, runID, batchID, actionID string, ordinal uint64, toolCallID string) PendingActionRef {
	return PendingActionRef{
		SessionID: sessionID, WorkspaceID: workspaceID, RunID: runID,
		ActionBatchID: batchID, ActionID: actionID,
		AssistantTranscriptOrdinal: ordinal, ToolCallID: toolCallID,
	}
}

func workspaceTestResolution(ref PendingActionRef, entryID string, seq uint64, evidence core.ExecutionResolutionEvidence, evidenceRef string) ValidatedExecutionResolution {
	return ValidatedExecutionResolution{
		Action: ref, CheckpointEntryID: entryID, CheckpointSeq: seq,
		Resolution: core.ExecutionResolution{
			RunID: ref.RunID, ActionID: ref.ActionID, Evidence: evidence, EvidenceRef: evidenceRef,
		},
	}
}

func TestWorkspaceIndexDispatchRecordsContainNoRawPayload(t *testing.T) {
	ctx := context.Background()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	batch := workspaceTestBatch("workspace-no-payload", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	batch.Actions[0].ToolIdentity = &tools.TrustedToolIdentitySnapshot{
		RegistrationSource: "builtin", ToolName: "write", RegistrationGeneration: 1,
		ExecutorFingerprint: strings.Repeat("a", 64), ScopeFingerprint: strings.Repeat("b", 64),
	}
	if err := index.EnsureWorkspace(ctx, batch.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if got, err := index.BeginDispatchBatch(ctx, batch); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch = (%v, %v)", got, err)
	}
	data, err := os.ReadFile(index.indexPath(batch.WorkspaceID))
	if err != nil {
		t.Fatal(err)
	}
	var rows []json.RawMessage
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		rows = append(rows, append(json.RawMessage(nil), line...))
	}
	if len(rows) != 2 {
		t.Fatalf("index rows = %d, want genesis and marker", len(rows))
	}
	for _, row := range rows {
		if strings.Contains(string(row), "raw_argument") || strings.Contains(string(row), "secret") {
			t.Fatalf("workspace index contains raw payload material: %s", row)
		}
	}
}

func TestWorkspaceIndexRejectsUnsafeWorkspaceIDs(t *testing.T) {
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	for _, workspaceID := range []string{"", "../escape", "a/b", "..", ".", "white space", "x\\y"} {
		if err := index.EnsureWorkspace(context.Background(), workspaceID); err == nil {
			t.Errorf("EnsureWorkspace accepted unsafe ID %q", workspaceID)
		}
	}
}

func TestWorkspaceIndexPreservesValidTailAndQuarantinesOnlyTornTail(t *testing.T) {
	ctx := context.Background()
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	workspaceID := "workspace-tail"
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	path := index.indexPath(workspaceID)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("fresh workspace index is not newline-delimited")
	}
	if err := os.WriteFile(path, data[:len(data)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	ref := workspaceTestRef(workspaceID, "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if _, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{ref}); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("valid JSON tail without LF was not preserved and delimited")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("{\"version\":")); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{ref}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var quarantined bool
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(path)+".torn-") && strings.HasSuffix(entry.Name(), ".tail") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Fatal("verified torn suffix was not preserved in a quarantine sidecar")
	}
}

func TestWorkspaceIndexAppendAndReadUseSymmetricExactRowLimit(t *testing.T) {
	for _, targetSize := range []int{maxDurableJSONLineBytes - 1, maxDurableJSONLineBytes, maxDurableJSONLineBytes + 1} {
		t.Run(strconv.Itoa(targetSize), func(t *testing.T) {
			ctx := context.Background()
			index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
			workspaceID := "workspace-limit-" + strconv.Itoa(targetSize)
			if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
				t.Fatal(err)
			}
			batch := workspaceBatchAtSerializedSize(workspaceID, targetSize)
			got, err := index.BeginDispatchBatch(ctx, batch)
			if targetSize > maxDurableJSONLineBytes {
				if err == nil || got != WorkspaceDispatchNotCommitted {
					t.Fatalf("oversized marker = (%v, %v), want rejection before append", got, err)
				}
				decisions, inspectErr := index.InspectDispatchMarkers(ctx, []PendingActionRef{batch.Actions[0].Action})
				if inspectErr != nil || len(decisions) != 1 || decisions[0].MarkerState != DispatchMarkerAbsent {
					t.Fatalf("oversized marker left a partial row: (%#v, %v)", decisions, inspectErr)
				}
				return
			}
			if err != nil || got != WorkspaceDispatchCommittedNow {
				t.Fatalf("marker at size %d = (%v, %v), want committed", targetSize, got, err)
			}
			path := index.indexPath(workspaceID)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := bytes.Split(data, []byte{'\n'})
			if len(lines) < 3 || len(lines[1]) != targetSize {
				t.Fatalf("serialized marker size = %d, want %d", len(lines[1]), targetSize)
			}
			ref := batch.Actions[0].Action
			reopened := newWorkspaceQuarantineIndex(index.root, workspaceIndexIO{})
			decisions, err := reopened.InspectDispatchMarkers(ctx, []PendingActionRef{ref})
			if err != nil || len(decisions) != 1 || decisions[0].MarkerState != DispatchMarkerPresent {
				t.Fatalf("reader rejected valid row of size %d: (%#v, %v)", targetSize, decisions, err)
			}
		})
	}
}

func workspaceBatchAtSerializedSize(workspaceID string, targetSize int) WorkspaceDispatchBatch {
	batch := workspaceTestBatch(workspaceID, "session-limit", "run-limit", "batch-limit", "action-limit", 0, "x")
	record := workspaceIndexRecord{
		Version: 1, RecordID: workspaceDispatchRecordID(batch), Kind: workspaceIndexDispatchReady,
		WorkspaceID: batch.WorkspaceID, SessionID: batch.SessionID, RunID: batch.RunID,
		ActionBatchID: batch.ActionBatchID, Actions: batch.Actions,
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		panic(err)
	}
	callIDLength := targetSize - len(encoded) + len(batch.Actions[0].Action.ToolCallID)
	if callIDLength < 1 {
		panic("target JSONL row size is smaller than the workspace record envelope")
	}
	batch.Actions[0].Action.ToolCallID = strings.Repeat("x", callIDLength)
	return batch
}
