package tools

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSnapshotRevert_TakeAndRevert verifies the core snapshot-and-revert flow:
// a snapshot captures the working-tree state before a mutation, and reverting
// restores the file to that captured state even after further modifications.
func TestSnapshotRevert_TakeAndRevert(t *testing.T) {
	git := setupGitRepo(t)
	dir := git.cwd
	ctx := context.Background()

	// Create and commit a tracked file.
	writeFileInDir(t, dir, "file.txt", "original\n")
	runGitInDir(t, dir, "add", "file.txt")
	runGitInDir(t, dir, "commit", "-m", "add file.txt")

	sm := NewSnapshotManager(dir)
	require.True(t, sm.Enabled(), "manager should be enabled in a git repo")

	// Modify the file (uncommitted) — this is the state the snapshot captures.
	writeFileInDir(t, dir, "file.txt", "modified1\n")
	require.NoError(t, sm.TakeSnapshot(ctx, "write", "file.txt"))

	// Overwrite with different content.
	writeFileInDir(t, dir, "file.txt", "modified2\n")
	got, err := os.ReadFile(dir + "/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "modified2\n", string(got))

	// Revert to snapshot "1" — should restore "modified1".
	require.NoError(t, sm.Revert(ctx, "1"))
	got, err = os.ReadFile(dir + "/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "modified1\n", string(got), "revert should restore snapshot state")
}

// TestSnapshotRevert_List verifies that snapshots are recorded in creation
// order and List returns all of them.
func TestSnapshotRevert_List(t *testing.T) {
	git := setupGitRepo(t)
	dir := git.cwd
	ctx := context.Background()

	writeFileInDir(t, dir, "file.txt", "v0\n")
	runGitInDir(t, dir, "add", "file.txt")
	runGitInDir(t, dir, "commit", "-m", "add file.txt")

	sm := NewSnapshotManager(dir)
	require.True(t, sm.Enabled())

	// Take three snapshots, modifying the file each time so stash create has
	// something to capture.
	for i, content := range []string{"a\n", "b\n", "c\n"} {
		writeFileInDir(t, dir, "file.txt", content)
		require.NoError(t, sm.TakeSnapshot(ctx, "write", "file.txt"))
		// Commit so the next modification produces a fresh uncommitted change.
		runGitInDir(t, dir, "add", "file.txt")
		runGitInDir(t, dir, "commit", "-m", fmt.Sprintf("commit-%d", i))
	}

	snapshots := sm.List()
	require.Len(t, snapshots, 3)
	assert.Equal(t, "1", snapshots[0].ID)
	assert.Equal(t, "2", snapshots[1].ID)
	assert.Equal(t, "3", snapshots[2].ID)
	assert.Equal(t, "write", snapshots[0].ToolName)
	assert.Equal(t, "file.txt", snapshots[0].FilePath)
}

// TestSnapshotRevert_NonGitRepo verifies graceful degradation (AC-5): when
// the working directory is not a git repository, the manager disables itself
// and all methods are no-ops that never error.
func TestSnapshotRevert_NonGitRepo(t *testing.T) {
	dir := t.TempDir() // not a git repo

	sm := NewSnapshotManager(dir)
	assert.False(t, sm.Enabled(), "manager should be disabled outside a git repo")

	// TakeSnapshot should be a no-op (no error, no snapshot stored).
	ctx := context.Background()
	require.NoError(t, sm.TakeSnapshot(ctx, "write", "file.txt"))
	assert.Empty(t, sm.List(), "no snapshots should be stored when disabled")
}

// TestSnapshotRevert_RevertNonexistent verifies that reverting a non-existent
// snapshot ID returns an error.
func TestSnapshotRevert_RevertNonexistent(t *testing.T) {
	git := setupGitRepo(t)
	dir := git.cwd
	ctx := context.Background()

	sm := NewSnapshotManager(dir)
	require.True(t, sm.Enabled())

	err := sm.Revert(ctx, "999")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

// TestSnapshotRevert_FileTrackerIntegration verifies that FileTracker's
// RecordMutation delegates to the SnapshotManager and captures a snapshot.
func TestSnapshotRevert_FileTrackerIntegration(t *testing.T) {
	git := setupGitRepo(t)
	dir := git.cwd
	ctx := context.Background()

	writeFileInDir(t, dir, "file.txt", "original\n")
	runGitInDir(t, dir, "add", "file.txt")
	runGitInDir(t, dir, "commit", "-m", "add file.txt")

	sm := NewSnapshotManager(dir)
	ft := NewFileTracker()
	ft.SetSnapshotManager(sm)

	// Modify the file so stash create has uncommitted changes to capture.
	writeFileInDir(t, dir, "file.txt", "changed\n")

	// RecordMutation should capture a snapshot via the SnapshotManager.
	ft.RecordMutation(ctx, "write", "file.txt")

	snapshots := sm.List()
	require.Len(t, snapshots, 1, "RecordMutation should create one snapshot")
	assert.Equal(t, "1", snapshots[0].ID)
	assert.Equal(t, "write", snapshots[0].ToolName)
	assert.Equal(t, "file.txt", snapshots[0].FilePath)
}

// TestSnapshotRevert_NoSnapshotManager verifies that RecordMutation is a safe
// no-op when no SnapshotManager is configured (no panic, no error).
func TestSnapshotRevert_NoSnapshotManager(t *testing.T) {
	ft := NewFileTracker()
	// No SetSnapshotManager call — snapshotMgr is nil.

	// This should not panic.
	assert.NotPanics(t, func() {
		ft.RecordMutation(context.Background(), "write", "file.txt")
	})
}
