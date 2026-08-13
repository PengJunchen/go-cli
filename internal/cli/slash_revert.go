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

func (h *RevertHandler) Handle(ctx context.Context, args []string, deps Dependencies) (string, error) {
	if deps.SnapshotManager() == nil || !deps.SnapshotManager().Enabled() {
		fmt.Fprintln(deps.Out(), "Snapshot revert is not available (not in a git repository).")
		return "", nil
	}
	if len(args) == 0 {
		h.printUsage(deps)
		return "", nil
	}
	switch args[0] {
	case "list":
		return "", h.handleList(deps)
	default:
		return "", h.handleRevert(ctx, args[0], deps)
	}
}

func (h *RevertHandler) printUsage(deps Dependencies) {
	fmt.Fprintln(deps.Out(), "Usage: /revert <list|<n>>")
	fmt.Fprintln(deps.Out(), "  list  - List available snapshots")
	fmt.Fprintln(deps.Out(), "  <n>   - Revert file state to snapshot n")
}

func (h *RevertHandler) handleList(deps Dependencies) error {
	snapshots := deps.SnapshotManager().List()
	if len(snapshots) == 0 {
		fmt.Fprintln(deps.Out(), "No snapshots available.")
		return nil
	}
	fmt.Fprintf(deps.Out(), "%-4s  %-20s  %-10s  %s\n", "ID", "TIMESTAMP", "TOOL", "FILE")
	for _, s := range snapshots {
		fmt.Fprintf(deps.Out(), "%-4s  %-20s  %-10s  %s\n", s.ID, s.Timestamp.Format("2006-01-02 15:04:05"), s.ToolName, s.FilePath)
	}
	return nil
}

func (h *RevertHandler) handleRevert(ctx context.Context, id string, deps Dependencies) error {
	if err := deps.SnapshotManager().Revert(ctx, id); err != nil {
		return fmt.Errorf("revert: %w", err)
	}
	fmt.Fprintf(deps.Out(), "Reverted to snapshot %s.\n", id)
	return nil
}
