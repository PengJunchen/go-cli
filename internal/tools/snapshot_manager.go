package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Snapshot records the state of the working tree at a point in time, captured
// via `git stash create`. It allows reverting individual files to their state
// when the snapshot was taken.
type Snapshot struct {
	// ID is the sequential numeric identifier ("1", "2", "3", ...).
	ID string
	// Timestamp is when the snapshot was captured.
	Timestamp time.Time
	// StashRef is the git stash create output (commit SHA), or "HEAD" when
	// there were no uncommitted changes at capture time.
	StashRef string
	// ToolName is the tool that triggered the snapshot.
	ToolName string
	// FilePath is the file path that was about to be modified.
	FilePath string
}

// SnapshotManager captures git working-tree snapshots before file mutations
// and supports reverting files to a previous snapshot state. When the working
// directory is not a git repository, the manager disables itself gracefully
// (AC-5): all methods become no-ops and never block callers on failure.
type SnapshotManager struct {
	mu        sync.Mutex
	cwd       string // git working directory
	snapshots []Snapshot
	nextID    int
	enabled   bool // false when not in a git repo
}

// NewSnapshotManager creates a SnapshotManager anchored at cwd. It probes
// whether cwd is inside a git work tree; when it is not, the manager disables
// itself so that subsequent TakeSnapshot/Revert calls are no-ops.
func NewSnapshotManager(cwd string) *SnapshotManager {
	sm := &SnapshotManager{cwd: cwd, nextID: 1}
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--is-inside-work-tree")
	if err := cmd.Run(); err != nil {
		slog.Warn("snapshot_manager.disabled", "reason", "not a git repository", "cwd", cwd)
		sm.enabled = false
	} else {
		sm.enabled = true
	}
	return sm
}

// Enabled reports whether the manager is active. It returns false when the
// working directory is not inside a git repository.
func (sm *SnapshotManager) Enabled() bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.enabled
}

// TakeSnapshot captures the current working-tree state via `git stash create`.
// When the manager is disabled (non-git repo) or the command fails, it returns
// nil without recording a snapshot (AC-5: don't block on failure). When there
// are no uncommitted changes, git stash create returns an empty string; in that
// case "HEAD" is used as the StashRef so Revert can still restore the committed
// version of the file.
func (sm *SnapshotManager) TakeSnapshot(ctx context.Context, toolName, filePath string) error {
	sm.mu.Lock()
	if !sm.enabled {
		sm.mu.Unlock()
		return nil
	}
	sm.mu.Unlock()

	cmd := exec.CommandContext(ctx, "git", "-C", sm.cwd, "stash", "create")
	out, err := cmd.Output()
	if err != nil {
		slog.Warn("snapshot_manager.take_failed", "tool", toolName, "file", filePath, "err", err)
		return nil
	}

	ref := strings.TrimSpace(string(out))
	if ref == "" {
		ref = "HEAD"
	}

	sm.mu.Lock()
	id := strconv.Itoa(sm.nextID)
	sm.snapshots = append(sm.snapshots, Snapshot{
		ID:        id,
		Timestamp: time.Now(),
		StashRef:  ref,
		ToolName:  toolName,
		FilePath:  filePath,
	})
	sm.nextID++
	sm.mu.Unlock()

	slog.Info("snapshot_manager.taken", "id", id, "tool", toolName, "file", filePath)
	return nil
}

// Revert restores the file recorded in the snapshot with the given ID to its
// state when the snapshot was taken, using `git checkout <stashRef> -- <path>`.
// It returns an error when the snapshot does not exist or the checkout fails.
func (sm *SnapshotManager) Revert(ctx context.Context, id string) error {
	sm.mu.Lock()
	if !sm.enabled {
		sm.mu.Unlock()
		return fmt.Errorf("snapshot_manager: not enabled")
	}
	var snapshot Snapshot
	found := false
	for _, s := range sm.snapshots {
		if s.ID == id {
			snapshot = s
			found = true
			break
		}
	}
	sm.mu.Unlock()

	if !found {
		return fmt.Errorf("snapshot %s not found", id)
	}

	cmd := exec.CommandContext(ctx, "git", "-C", sm.cwd, "checkout", snapshot.StashRef, "--", snapshot.FilePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("snapshot_manager.revert %s: %w (output: %s)", id, err, strings.TrimSpace(string(out)))
	}

	slog.Info("snapshot_manager.reverted", "id", id, "file", snapshot.FilePath)
	return nil
}

// List returns a copy of all captured snapshots in creation order.
func (sm *SnapshotManager) List() []Snapshot {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	out := make([]Snapshot, len(sm.snapshots))
	copy(out, sm.snapshots)
	return out
}
