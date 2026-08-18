//exempt:scan010
package cli

import (
	"context"
	"fmt"
	"sort"
)

// WorktreeHandler implements the /worktree slash command for managing git
// worktrees used in parallel session isolation.
type WorktreeHandler struct{}

func (h *WorktreeHandler) Name() string        { return "worktree" }
func (h *WorktreeHandler) Description() string { return "Manage git worktrees for session isolation" }

func (h *WorktreeHandler) Handle(ctx context.Context, args []string, deps Dependencies) (string, error) {
	if len(args) == 0 {
		h.printUsage(deps)
		return "", nil
	}
	sub := args[0]
	switch sub {
	case "list":
		return "", h.handleList(ctx, deps)
	case "create":
		return "", h.handleCreate(ctx, deps)
	case "remove":
		return "", h.handleRemove(ctx, args[1:], deps)
	case "cleanup":
		return "", h.handleCleanup(ctx, deps)
	default:
		return "", newUsageError("worktree: unknown subcommand %q (use: list, create, remove, cleanup)", sub)
	}
}

func (h *WorktreeHandler) printUsage(deps Dependencies) {
	fmt.Fprintln(deps.Out(), "Usage: /worktree <list|create|remove|cleanup>")                 //nolint:errcheck
	fmt.Fprintln(deps.Out(), "  list    - List active worktrees")                             //nolint:errcheck
	fmt.Fprintln(deps.Out(), "  create  - Create a worktree for the current session")         //nolint:errcheck
	fmt.Fprintln(deps.Out(), "  remove  - Remove the worktree for the current session")       //nolint:errcheck
	fmt.Fprintln(deps.Out(), "  cleanup - Remove orphaned worktrees not tied to any session") //nolint:errcheck
}

func (h *WorktreeHandler) handleList(ctx context.Context, deps Dependencies) error {
	if deps.WorktreeManager() == nil {
		fmt.Fprintln(deps.Out(), "Worktree isolation is not enabled. Set git.worktree_enabled in config.") //nolint:errcheck
		return nil
	}
	sessions := deps.WorktreeManager().List()
	if len(sessions) == 0 {
		fmt.Fprintln(deps.Out(), "No active worktrees.") //nolint:errcheck
		return nil
	}
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Fprintln(deps.Out(), "SESSION ID\tWORKTREE PATH") //nolint:errcheck
	for _, id := range ids {
		fmt.Fprintf(deps.Out(), "%s\t%s\n", id, sessions[id]) //nolint:errcheck
	}
	return nil
}

func (h *WorktreeHandler) handleCreate(ctx context.Context, deps Dependencies) error {
	if deps.WorktreeManager() == nil {
		return fmt.Errorf("worktree isolation is not enabled")
	}
	sessionID := deps.SessionID()
	if sessionID == "" {
		return fmt.Errorf("no active session")
	}
	branchPrefix := ""
	if deps.Config() != nil {
		branchPrefix = deps.Config().Git.BranchPrefix
	}
	path, err := deps.WorktreeManager().CreateForSession(ctx, sessionID, branchPrefix)
	if err != nil {
		return fmt.Errorf("worktree create: %w", err)
	}
	if deps.FileTracker() != nil {
		deps.FileTracker().SetWorkdir(path)
	}
	fmt.Fprintf(deps.Out(), "Created worktree for session %s at %s\n", sessionID, path) //nolint:errcheck
	return nil
}

func (h *WorktreeHandler) handleRemove(ctx context.Context, args []string, deps Dependencies) error {
	if deps.WorktreeManager() == nil {
		return fmt.Errorf("worktree isolation is not enabled")
	}
	sessionID := deps.SessionID()
	if len(args) > 0 {
		sessionID = args[0]
	}
	if sessionID == "" {
		return fmt.Errorf("no session specified")
	}
	if err := deps.WorktreeManager().RemoveForSession(ctx, sessionID); err != nil {
		return fmt.Errorf("worktree remove: %w", err)
	}
	fmt.Fprintf(deps.Out(), "Removed worktree for session %s\n", sessionID) //nolint:errcheck
	return nil
}

func (h *WorktreeHandler) handleCleanup(ctx context.Context, deps Dependencies) error {
	if deps.WorktreeManager() == nil {
		return fmt.Errorf("worktree isolation is not enabled")
	}
	orphans, err := deps.WorktreeManager().ScanOrphans()
	if err != nil {
		return fmt.Errorf("worktree scan: %w", err)
	}
	if len(orphans) == 0 {
		fmt.Fprintln(deps.Out(), "No orphan worktrees found.") //nolint:errcheck
		return nil
	}
	fmt.Fprintln(deps.Out(), "SESSION ID\tPATH\tSTATUS") //nolint:errcheck
	removed := 0
	for _, p := range orphans {
		if err := deps.WorktreeManager().RemoveOrphan(ctx, p); err != nil {
			fmt.Fprintf(deps.Out(), "(orphan)\t%s\tFAILED: %v\n", p, err) //nolint:errcheck
			continue
		}
		fmt.Fprintf(deps.Out(), "(orphan)\t%s\tREMOVED\n", p) //nolint:errcheck
		removed++
	}
	fmt.Fprintf(deps.Out(), "Cleaned up %d/%d orphan worktree(s).\n", removed, len(orphans)) //nolint:errcheck
	return nil
}
