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

func TestWorkspaceIndexEnsureRetriesExistingDirectorySyncAfterFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	appDir := filepath.Join(root, "go-cli")
	indexDir := filepath.Join(appDir, "workspace-quarantine")
	var fail atomic.Bool
	fail.Store(true)
	var mu sync.Mutex
	syncCalls := make(map[string]int)
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{dirSync: func(path string) error {
		mu.Lock()
		syncCalls[path]++
		mu.Unlock()
		if fail.Load() && path == root {
			return errors.New("injected parent directory sync failure")
		}
		return syncDirectory(path)
	}})
	if err := index.EnsureWorkspace(ctx, "workspace-dir-retry"); err == nil {
		t.Fatal("EnsureWorkspace succeeded despite parent directory sync failure")
	}
	if info, err := os.Stat(appDir); err != nil || !info.IsDir() {
		t.Fatalf("failed bootstrap should leave the visible directory for retry: info=%v err=%v", info, err)
	}
	if _, err := os.Stat(indexDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("EnsureWorkspace unexpectedly advanced past failed parent sync: stat error = %v", err)
	}
	batch := workspaceTestBatch("workspace-dir-retry", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if result, err := index.BeginDispatchBatch(ctx, batch); err == nil || result == WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch after failed bootstrap = (%v, %v), want no dispatch permit", result, err)
	}

	mu.Lock()
	rootSyncsBeforeRetry := syncCalls[root]
	mu.Unlock()
	fail.Store(false)
	if err := index.EnsureWorkspace(ctx, batch.WorkspaceID); err != nil {
		t.Fatalf("EnsureWorkspace retry after parent became syncable: %v", err)
	}
	mu.Lock()
	rootSyncsAfterRetry := syncCalls[root]
	mu.Unlock()
	if rootSyncsAfterRetry <= rootSyncsBeforeRetry {
		t.Fatalf("retry did not re-sync the existing parent directory: before=%d after=%d", rootSyncsBeforeRetry, rootSyncsAfterRetry)
	}
}

func TestWorkspaceIndexEnsureRetriesParentSyncAfterGenesisRenameFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	appDir := filepath.Join(root, "go-cli")
	indexDir := filepath.Join(appDir, "workspace-quarantine")
	indexPath := filepath.Join(indexDir, "workspace-genesis-retry.jsonl")
	var fail atomic.Bool
	fail.Store(true)
	var mu sync.Mutex
	syncCalls := make(map[string]int)
	index := newWorkspaceQuarantineIndex(root, workspaceIndexIO{dirSync: func(path string) error {
		mu.Lock()
		syncCalls[path]++
		mu.Unlock()
		if fail.Load() && path == appDir {
			if _, err := os.Lstat(indexPath); err == nil {
				return errors.New("injected parent sync failure after genesis rename")
			}
		}
		return syncDirectory(path)
	}})
	if err := index.EnsureWorkspace(ctx, "workspace-genesis-retry"); err == nil {
		t.Fatal("EnsureWorkspace succeeded despite parent sync failure after genesis rename")
	}
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("test requires genesis JSONL to be visible after injected parent sync failure: %v", err)
	}
	batch := workspaceTestBatch("workspace-genesis-retry", "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
	if result, err := index.BeginDispatchBatch(ctx, batch); err == nil || result == WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch before bootstrap durability retry = (%v, %v), want no dispatch permit", result, err)
	}

	mu.Lock()
	indexDirSyncsBeforeRetry := syncCalls[indexDir]
	appDirSyncsBeforeRetry := syncCalls[appDir]
	mu.Unlock()
	fail.Store(false)
	if err := index.EnsureWorkspace(ctx, batch.WorkspaceID); err != nil {
		t.Fatalf("EnsureWorkspace retry after parent became syncable: %v", err)
	}
	mu.Lock()
	indexDirSyncsAfterRetry := syncCalls[indexDir]
	appDirSyncsAfterRetry := syncCalls[appDir]
	mu.Unlock()
	if indexDirSyncsAfterRetry <= indexDirSyncsBeforeRetry || appDirSyncsAfterRetry <= appDirSyncsBeforeRetry {
		t.Fatalf("retry did not sync existing index and parent directories: index %d→%d, parent %d→%d", indexDirSyncsBeforeRetry, indexDirSyncsAfterRetry, appDirSyncsBeforeRetry, appDirSyncsAfterRetry)
	}
	if got, err := index.BeginDispatchBatch(ctx, batch); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("BeginDispatchBatch after successful durability retry = (%v, %v), want committed", got, err)
	}
}

func TestMkdirDurablyRetriesExistingParentChainAfterFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "created-parent", "created-target")
	createdParent := filepath.Dir(path)
	var fail atomic.Bool
	fail.Store(true)
	var mu sync.Mutex
	syncCalls := make(map[string]int)
	syncDir := func(directory string) error {
		mu.Lock()
		syncCalls[directory]++
		mu.Unlock()
		if fail.Load() && directory == createdParent {
			return errors.New("injected created-parent sync failure")
		}
		return syncDirectory(directory)
	}
	if err := mkdirDurablyWithSync(path, 0o700, syncDir); err == nil {
		t.Fatal("mkdirDurably succeeded despite created-parent sync failure")
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("failed mkdirDurably should leave both created levels visible: info=%v err=%v", info, err)
	}
	mu.Lock()
	parentSyncsBeforeRetry := syncCalls[createdParent]
	rootSyncsBeforeRetry := syncCalls[root]
	mu.Unlock()
	fail.Store(false)
	if err := mkdirDurablyWithSync(path, 0o700, syncDir); err != nil {
		t.Fatalf("mkdirDurably retry after parent became syncable: %v", err)
	}
	mu.Lock()
	parentSyncsAfterRetry := syncCalls[createdParent]
	rootSyncsAfterRetry := syncCalls[root]
	mu.Unlock()
	if parentSyncsAfterRetry <= parentSyncsBeforeRetry || rootSyncsAfterRetry <= rootSyncsBeforeRetry {
		t.Fatalf("mkdirDurably retry did not sync the existing directory chain: parent %d→%d root %d→%d", parentSyncsBeforeRetry, parentSyncsAfterRetry, rootSyncsBeforeRetry, rootSyncsAfterRetry)
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

func TestWorkspaceIndexRejectsNULInStableRecordIdentityParts(t *testing.T) {
	ctx := context.Background()
	workspaceID := "workspace-nul-identity"
	left := workspaceTestBatch(workspaceID, "session-a", "run\x00part", "tail", "action-a", 0, "call-a")
	right := workspaceTestBatch(workspaceID, "session-a", "run", "part\x00tail", "action-a", 0, "call-a")
	if leftID, rightID := workspaceDispatchRecordID(left), workspaceDispatchRecordID(right); leftID != rightID {
		t.Fatalf("test setup requires ambiguous stable dispatch IDs, got %q and %q", leftID, rightID)
	}
	leftResolution := workspaceTestResolution(left.Actions[0].Action, "checkpoint-a", 1, core.ResolutionLocalResourcesStopped, "evidence-a")
	rightResolution := workspaceTestResolution(right.Actions[0].Action, "checkpoint-b", 2, core.ResolutionLocalResourcesStopped, "evidence-b")
	if leftID, rightID := workspaceResolutionRecordID(leftResolution.Action), workspaceResolutionRecordID(rightResolution.Action); leftID != rightID {
		t.Fatalf("test setup requires ambiguous stable resolution IDs, got %q and %q", leftID, rightID)
	}

	mutators := []struct {
		name   string
		mutate func(*WorkspaceDispatchBatch)
	}{
		{name: "workspace ID", mutate: func(batch *WorkspaceDispatchBatch) {
			batch.WorkspaceID += "\x00tail"
			batch.Actions[0].Action.WorkspaceID = batch.WorkspaceID
		}},
		{name: "session ID", mutate: func(batch *WorkspaceDispatchBatch) {
			batch.SessionID += "\x00tail"
			batch.Actions[0].Action.SessionID = batch.SessionID
		}},
		{name: "run ID", mutate: func(batch *WorkspaceDispatchBatch) {
			batch.RunID += "\x00tail"
			batch.Actions[0].Action.RunID = batch.RunID
		}},
		{name: "action batch ID", mutate: func(batch *WorkspaceDispatchBatch) {
			batch.ActionBatchID += "\x00tail"
			batch.Actions[0].Action.ActionBatchID = batch.ActionBatchID
		}},
		{name: "action ID", mutate: func(batch *WorkspaceDispatchBatch) {
			batch.Actions[0].Action.ActionID += "\x00tail"
		}},
	}

	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	for _, test := range mutators {
		t.Run(test.name, func(t *testing.T) {
			batch := workspaceTestBatch(workspaceID, "session-a", "run-a", "batch-a", "action-a", 0, "call-a")
			test.mutate(&batch)
			if _, err := normalizeWorkspaceDispatchBatch(batch); err == nil {
				t.Fatal("dispatch API normalization accepted a NUL in a stable record identity part")
			}
			if result, err := index.BeginDispatchBatch(ctx, batch); err == nil || result != WorkspaceDispatchNotCommitted {
				t.Fatalf("BeginDispatchBatch = (%v, %v), want rejection without a permit", result, err)
			}
			if _, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{batch.Actions[0].Action}); err == nil {
				t.Fatal("InspectDispatchMarkers accepted a NUL in a stable record identity part")
			}
			resolution := workspaceTestResolution(batch.Actions[0].Action, "checkpoint-nul", 1, core.ResolutionNeverDispatched, "receipt-nul")
			if err := index.ApplyDurableResolutions(ctx, []ValidatedExecutionResolution{resolution}); err == nil {
				t.Fatal("ApplyDurableResolutions accepted a NUL in a stable record identity part")
			}

			row := workspaceIndexRecord{
				Version: 1, Seq: 2, RecordID: workspaceDispatchRecordID(batch), Kind: workspaceIndexDispatchReady,
				WorkspaceID: batch.WorkspaceID, SessionID: batch.SessionID, RunID: batch.RunID,
				ActionBatchID: batch.ActionBatchID, Actions: batch.Actions,
			}
			genesis := workspaceIndexRecord{Version: 1, Seq: 1, RecordID: workspaceGenesisRecordID(batch.WorkspaceID), Kind: workspaceIndexInitialized, WorkspaceID: batch.WorkspaceID}
			if _, err := validateWorkspaceIndexRecords([]workspaceIndexRecord{genesis, row}, batch.WorkspaceID); err == nil {
				t.Fatal("workspace row validation accepted a NUL in a stable record identity part")
			}
			resolutionRow := workspaceIndexRecord{
				Version: 1, Seq: 2, RecordID: workspaceResolutionRecordID(resolution.Action), Kind: workspaceIndexResolution,
				WorkspaceID: resolution.Action.WorkspaceID, Action: refPointer(resolution.Action),
				CheckpointID: resolution.CheckpointEntryID, CheckpointSeq: resolution.CheckpointSeq,
				Resolution: resolutionPointer(resolution.Resolution),
			}
			resolutionState := &workspaceIndexState{actionsByKey: make(map[workspaceActionKey]*workspaceIndexedAction)}
			if err := resolutionState.addResolutionRecord(&resolutionRow); err == nil {
				t.Fatal("resolution row validation accepted a NUL in a stable record identity part")
			}
			rows, err := readWorkspaceIndexFile(index.indexPath(workspaceID), filepath.Dir(index.indexPath(workspaceID)), workspaceID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("invalid API input wrote %d index rows, want genesis only", len(rows))
			}
		})
	}

	valid := workspaceTestBatch(workspaceID, "session-normal", "run-normal", "batch-normal", "action-normal", 0, "call-normal")
	if result, err := index.BeginDispatchBatch(ctx, valid); err != nil || result != WorkspaceDispatchCommittedNow {
		t.Fatalf("ordinary stable ID values changed behavior: (%v, %v)", result, err)
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

func TestWorkspaceIndexAllowsOnlyFreshPreparedBuiltinReadDuringQuarantine(t *testing.T) {
	ctx := context.Background()
	root := canonicalWorkspaceIndexTestRoot(t, t.TempDir())
	readPath := filepath.Join(root, "safe.txt")
	if err := os.WriteFile(readPath, []byte("safe contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := WorkspaceIDForCanonicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	index := newWorkspaceQuarantineIndex(t.TempDir(), workspaceIndexIO{})
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	oldMutation := workspaceTestBatch(workspaceID, "session-old", "run-old", "batch-old", "action-old", 0, "write-old")
	oldMutation.Actions[0].EffectSet = []tools.TrustedToolEffect{tools.EffectWorkspaceWrite}
	if got, err := index.BeginDispatchBatch(ctx, oldMutation); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("initial mutation marker = (%v, %v), want committed-now", got, err)
	}

	invocation, proof := preparedWorkspaceIndexRead(t, root, readPath, "read-fresh")
	identity := invocation.ToolIdentity().Snapshot()
	readBatch := WorkspaceDispatchBatch{
		WorkspaceID: workspaceID, SessionID: "session-read", RunID: "run-read", ActionBatchID: "batch-read",
		CanonicalWorkspaceRoot: root,
		Actions: []WorkspaceDispatchAction{{
			Action:       workspaceTestRef(workspaceID, "session-read", "run-read", "batch-read", "action-read", 0, "read-fresh"),
			ToolIdentity: &identity, EffectSet: invocation.EffectSet().Values(), ScopedReadProof: proof,
		}},
	}
	executorCalls := 0
	tryRead := func(attemptCtx context.Context, prepared tools.PreparedInvocation, batch WorkspaceDispatchBatch) (WorkspaceDispatchResult, error, *tools.ToolResult) {
		state, beginErr := index.BeginDispatchBatch(attemptCtx, batch)
		if beginErr != nil || state != WorkspaceDispatchCommittedNow {
			return state, beginErr, nil
		}
		executorCalls++
		result, dispatchErr := prepared.Dispatch(attemptCtx)
		return state, dispatchErr, result
	}

	// Invalid runtime facts and pre-dispatch cancellation must fail before the
	// prepared invocation's actual ReadTool executor is entered.
	wrongRoot := canonicalWorkspaceIndexTestRoot(t, t.TempDir())
	wrongRootBatch := readBatch
	wrongRootBatch.CanonicalWorkspaceRoot = wrongRoot
	if got, err, _ := tryRead(ctx, invocation, wrongRootBatch); !errors.Is(err, ErrWorkspaceQuarantined) || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("root-mismatched read = (%v, %v), want quarantine", got, err)
	}
	mixedBatch := readBatch
	mixedBatch.Actions = append(append([]WorkspaceDispatchAction(nil), readBatch.Actions...), WorkspaceDispatchAction{
		Action:    workspaceTestRef(workspaceID, "session-read", "run-read", "batch-read", "action-write", 0, "write-mixed"),
		EffectSet: []tools.TrustedToolEffect{tools.EffectWorkspaceWrite},
	})
	if got, err, _ := tryRead(ctx, invocation, mixedBatch); !errors.Is(err, ErrWorkspaceQuarantined) || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("mixed read/write batch = (%v, %v), want quarantine", got, err)
	}
	compositeBatch := readBatch
	compositeBatch.Actions = append([]WorkspaceDispatchAction(nil), readBatch.Actions...)
	compositeBatch.Actions[0].EffectSet = []tools.TrustedToolEffect{tools.EffectWorkspaceRead, tools.EffectWorkspaceWrite}
	if got, err, _ := tryRead(ctx, invocation, compositeBatch); !errors.Is(err, ErrWorkspaceQuarantined) || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("composite-effect read = (%v, %v), want quarantine", got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err, _ := tryRead(canceled, invocation, readBatch); !errors.Is(err, context.Canceled) || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("canceled read = (%v, %v), want canceled without a permit", got, err)
	}
	noProofInvocation, _ := preparedWorkspaceIndexRead(t, root, readPath, "read-dto-only")
	noProofIdentity := noProofInvocation.ToolIdentity().Snapshot()
	noProofBatch := WorkspaceDispatchBatch{
		WorkspaceID: workspaceID, SessionID: "session-dto", RunID: "run-dto", ActionBatchID: "batch-dto",
		CanonicalWorkspaceRoot: root,
		Actions: []WorkspaceDispatchAction{{
			Action:       workspaceTestRef(workspaceID, "session-dto", "run-dto", "batch-dto", "action-dto", 0, "read-dto-only"),
			ToolIdentity: &noProofIdentity, EffectSet: noProofInvocation.EffectSet().Values(),
		}},
	}
	if got, err, _ := tryRead(ctx, noProofInvocation, noProofBatch); !errors.Is(err, ErrWorkspaceQuarantined) || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("matching public identity/effect DTO without private proof = (%v, %v), want quarantine", got, err)
	}
	if executorCalls != 0 {
		t.Fatalf("rejected read attempts entered actual executor %d times, want zero", executorCalls)
	}

	got, err, result := tryRead(ctx, invocation, readBatch)
	if err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("fresh prepared builtin read = (%v, %v), want committed-now", got, err)
	}
	if result == nil || result.Output != "safe contents\n" {
		t.Fatalf("actual read result = %#v, want captured file contents", result)
	}
	if got, err := index.BeginDispatchBatch(ctx, readBatch); err != nil || got != WorkspaceDispatchAlreadyExists {
		t.Fatalf("duplicate marker attempt = (%v, %v), want already-exists without replay", got, err)
	}
	cloneBatch := readBatch
	cloneBatch.SessionID, cloneBatch.RunID, cloneBatch.ActionBatchID = "session-clone", "run-clone", "batch-clone"
	cloneBatch.Actions = append([]WorkspaceDispatchAction(nil), readBatch.Actions...)
	cloneBatch.Actions[0].Action = workspaceTestRef(workspaceID, cloneBatch.SessionID, cloneBatch.RunID, cloneBatch.ActionBatchID, "action-clone", 0, "read-fresh")
	if got, err := index.BeginDispatchBatch(ctx, cloneBatch); !errors.Is(err, ErrWorkspaceQuarantined) || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("cloned claimed proof = (%v, %v), want quarantine", got, err)
	}
	mutation := workspaceTestBatch(workspaceID, "session-new", "run-new", "batch-new", "action-new", 0, "write-new")
	mutation.Actions[0].EffectSet = []tools.TrustedToolEffect{tools.EffectWorkspaceWrite}
	if got, err := index.BeginDispatchBatch(ctx, mutation); !errors.Is(err, ErrWorkspaceQuarantined) || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("new mutation while old marker unresolved = (%v, %v), want quarantine", got, err)
	}
	if executorCalls != 1 {
		t.Fatalf("actual prepared read executor entered %d times, want exactly once", executorCalls)
	}

	decisions, err := index.InspectDispatchMarkers(ctx, []PendingActionRef{oldMutation.Actions[0].Action, readBatch.Actions[0].Action})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 2 || decisions[0].MarkerState != DispatchMarkerPresent || decisions[1].MarkerState != DispatchMarkerPresent {
		t.Fatalf("old mutation/read marker states = %#v, want both unresolved", decisions)
	}
	payload, err := os.ReadFile(index.indexPath(workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), root) || strings.Contains(string(payload), "ScopedWorkspaceReadProof") {
		t.Fatalf("runtime read proof/root leaked into durable index: %s", payload)
	}
}

func TestWorkspaceIndexSyncFailureConsumesNoSafeReadExecutorPermit(t *testing.T) {
	ctx := context.Background()
	root := canonicalWorkspaceIndexTestRoot(t, t.TempDir())
	readPath := filepath.Join(root, "safe.txt")
	if err := os.WriteFile(readPath, []byte("safe contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceID, err := WorkspaceIDForCanonicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	var failSync atomic.Bool
	var syncCalls atomic.Int32
	indexRoot := t.TempDir()
	index := newWorkspaceQuarantineIndex(indexRoot, workspaceIndexIO{sync: func(file *os.File) error {
		if failSync.Load() && syncCalls.Add(1) == 2 {
			return errors.New("injected safe-read marker sync failure")
		}
		return file.Sync()
	}})
	if err := index.EnsureWorkspace(ctx, workspaceID); err != nil {
		t.Fatal(err)
	}
	oldMutation := workspaceTestBatch(workspaceID, "session-old", "run-old", "batch-old", "action-old", 0, "write-old")
	if got, err := index.BeginDispatchBatch(ctx, oldMutation); err != nil || got != WorkspaceDispatchCommittedNow {
		t.Fatalf("initial mutation marker = (%v, %v)", got, err)
	}
	invo, proof := preparedWorkspaceIndexRead(t, root, readPath, "read-sync-failure")
	identity := invo.ToolIdentity().Snapshot()
	batch := WorkspaceDispatchBatch{
		WorkspaceID: workspaceID, SessionID: "session-safe", RunID: "run-safe", ActionBatchID: "batch-safe",
		CanonicalWorkspaceRoot: root,
		Actions: []WorkspaceDispatchAction{{
			Action:       workspaceTestRef(workspaceID, "session-safe", "run-safe", "batch-safe", "action-safe", 0, "read-sync-failure"),
			ToolIdentity: &identity, EffectSet: invo.EffectSet().Values(), ScopedReadProof: proof,
		}},
	}
	var executorCalls atomic.Int32
	failSync.Store(true)
	syncCalls.Store(0)
	got, beginErr := index.BeginDispatchBatch(ctx, batch)
	if got == WorkspaceDispatchCommittedNow {
		executorCalls.Add(1)
		_, _ = invo.Dispatch(ctx)
	}
	if beginErr == nil || got != WorkspaceDispatchNotCommitted {
		t.Fatalf("safe read after failed marker sync = (%v, %v), want no permit", got, beginErr)
	}
	if executorCalls.Load() != 0 {
		t.Fatalf("safe-read sync failure entered executor %d times, want zero", executorCalls.Load())
	}

	failSync.Store(false)
	reopened := newWorkspaceQuarantineIndex(indexRoot, workspaceIndexIO{})
	decisions, err := reopened.InspectDispatchMarkers(ctx, []PendingActionRef{batch.Actions[0].Action})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].MarkerState != DispatchMarkerPresent {
		t.Fatalf("failed-sync safe-read marker = %#v, want present/unknown", decisions)
	}
	got, beginErr = reopened.BeginDispatchBatch(ctx, batch)
	if beginErr != nil || got != WorkspaceDispatchAlreadyExists {
		t.Fatalf("retry after failed safe-read marker sync = (%v, %v), want already-exists without replay", got, beginErr)
	}
	if executorCalls.Load() != 0 {
		t.Fatalf("safe-read sync retry entered executor %d times, want zero", executorCalls.Load())
	}
}

func canonicalWorkspaceIndexTestRoot(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func preparedWorkspaceIndexRead(t *testing.T, root, path, callID string) (tools.PreparedInvocation, *tools.ScopedWorkspaceReadProof) {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	registry := tools.NewMiddlewareToolRegistry(base)
	if err := registry.SetPreparedToolStages(tools.PreparedToolStages{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := tools.RegisterDefaults(ctx, registry, tools.WithRegisteredCanonicalWorkspaceRoot(root)); err != nil {
		t.Fatal(err)
	}
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{ID: callID, Name: "read", Args: map[string]any{"path": path}})
	if err != nil {
		t.Fatal(err)
	}
	proof, ok := tools.ScopedWorkspaceReadProofFor(invocation, root)
	if !ok {
		t.Fatal("actual prepared built-in read did not issue a scoped proof")
	}
	return invocation, proof
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
