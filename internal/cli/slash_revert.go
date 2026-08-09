package cli

import (
	"context"
	"fmt"
)

// RevertHandler implements the /revert slash command for reverting file state
// to a previous snapshot captured by the SnapshotManager before a file
// mutation. Snapshots are taken automatically before each write/edit when the
// working directory is a git repository.
type RevertHandler struct{}

func (h *RevertHandler) Name() string        { return "revert" }
func (h *RevertHandler) Description() string { return "Revert file state to a previous snapshot" }

func (h *RevertHandler) Handle(ctx context.Context, args []string, sc *slashContext) error {
	if sc.snapshotManager == nil || !sc.snapshotManager.Enabled() {
		fmt.Fprintln(sc.out, "Snapshot revert is not available (not in a git repository).")
		return nil
	}
	if len(args) == 0 {
		h.printUsage(sc)
		return nil
	}
	switch args[0] {
	case "list":
		return h.handleList(sc)
	default:
		return h.handleRevert(ctx, args[0], sc)
	}
}

func (h *RevertHandler) printUsage(sc *slashContext) {
	fmt.Fprintln(sc.out, "Usage: /revert <list|<n>>")
	fmt.Fprintln(sc.out, "  list  - List available snapshots")
	fmt.Fprintln(sc.out, "  <n>   - Revert file state to snapshot n")
}

func (h *RevertHandler) handleList(sc *slashContext) error {
	snapshots := sc.snapshotManager.List()
	if len(snapshots) == 0 {
		fmt.Fprintln(sc.out, "No snapshots available.")
		return nil
	}
	fmt.Fprintf(sc.out, "%-4s  %-20s  %-10s  %s\n", "ID", "TIMESTAMP", "TOOL", "FILE")
	for _, s := range snapshots {
		fmt.Fprintf(sc.out, "%-4s  %-20s  %-10s  %s\n", s.ID, s.Timestamp.Format("2006-01-02 15:04:05"), s.ToolName, s.FilePath)
	}
	return nil
}

func (h *RevertHandler) handleRevert(ctx context.Context, id string, sc *slashContext) error {
	if err := sc.snapshotManager.Revert(ctx, id); err != nil {
		return fmt.Errorf("revert: %w", err)
	}
	fmt.Fprintf(sc.out, "Reverted to snapshot %s.\n", id)
	return nil
}
