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

func (h *WorktreeHandler) Handle(ctx context.Context, args []string, sc *slashContext) error {
	if len(args) == 0 {
		h.printUsage(sc)
		return nil
	}
	sub := args[0]
	switch sub {
	case "list":
		return h.handleList(ctx, sc)
	case "create":
		return h.handleCreate(ctx, sc)
	case "remove":
		return h.handleRemove(ctx, args[1:], sc)
	case "cleanup":
		return h.handleCleanup(ctx, sc)
	default:
		return newUsageError("worktree: unknown subcommand %q (use: list, create, remove, cleanup)", sub)
	}
}

func (h *WorktreeHandler) printUsage(sc *slashContext) {
	fmt.Fprintln(sc.out, "Usage: /worktree <list|create|remove|cleanup>")
	fmt.Fprintln(sc.out, "  list    - List active worktrees")
	fmt.Fprintln(sc.out, "  create  - Create a worktree for the current session")
	fmt.Fprintln(sc.out, "  remove  - Remove the worktree for the current session")
	fmt.Fprintln(sc.out, "  cleanup - Remove orphaned worktrees not tied to any session")
}

func (h *WorktreeHandler) handleList(ctx context.Context, sc *slashContext) error {
	if sc.worktreeManager == nil {
		fmt.Fprintln(sc.out, "Worktree isolation is not enabled. Set git.worktree_enabled in config.")
		return nil
	}
	sessions := sc.worktreeManager.List()
	if len(sessions) == 0 {
		fmt.Fprintln(sc.out, "No active worktrees.")
		return nil
	}
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	fmt.Fprintln(sc.out, "SESSION ID\tWORKTREE PATH")
	for _, id := range ids {
		fmt.Fprintf(sc.out, "%s\t%s\n", id, sessions[id])
	}
	return nil
}

func (h *WorktreeHandler) handleCreate(ctx context.Context, sc *slashContext) error {
	if sc.worktreeManager == nil {
		return fmt.Errorf("worktree isolation is not enabled")
	}
	sessionID := sc.sessionID
	if sessionID == "" {
		return fmt.Errorf("no active session")
	}
	branchPrefix := ""
	if sc.config != nil {
		branchPrefix = sc.config.Git.BranchPrefix
	}
	path, err := sc.worktreeManager.CreateForSession(ctx, sessionID, branchPrefix)
	if err != nil {
		return fmt.Errorf("worktree create: %w", err)
	}
	if sc.fileTracker != nil {
		sc.fileTracker.SetWorkdir(path)
	}
	fmt.Fprintf(sc.out, "Created worktree for session %s at %s\n", sessionID, path)
	return nil
}

func (h *WorktreeHandler) handleRemove(ctx context.Context, args []string, sc *slashContext) error {
	if sc.worktreeManager == nil {
		return fmt.Errorf("worktree isolation is not enabled")
	}
	sessionID := sc.sessionID
	if len(args) > 0 {
		sessionID = args[0]
	}
	if sessionID == "" {
		return fmt.Errorf("no session specified")
	}
	if err := sc.worktreeManager.RemoveForSession(ctx, sessionID); err != nil {
		return fmt.Errorf("worktree remove: %w", err)
	}
	fmt.Fprintf(sc.out, "Removed worktree for session %s\n", sessionID)
	return nil
}

func (h *WorktreeHandler) handleCleanup(ctx context.Context, sc *slashContext) error {
	if sc.worktreeManager == nil {
		return fmt.Errorf("worktree isolation is not enabled")
	}
	orphans, err := sc.worktreeManager.ScanOrphans()
	if err != nil {
		return fmt.Errorf("worktree scan: %w", err)
	}
	if len(orphans) == 0 {
		fmt.Fprintln(sc.out, "No orphan worktrees found.")
		return nil
	}
	fmt.Fprintln(sc.out, "SESSION ID\tPATH\tSTATUS")
	removed := 0
	for _, p := range orphans {
		if err := sc.worktreeManager.RemoveOrphan(ctx, p); err != nil {
			fmt.Fprintf(sc.out, "(orphan)\t%s\tFAILED: %v\n", p, err)
			continue
		}
		fmt.Fprintf(sc.out, "(orphan)\t%s\tREMOVED\n", p)
		removed++
	}
	fmt.Fprintf(sc.out, "Cleaned up %d/%d orphan worktree(s).\n", removed, len(orphans))
	return nil
}
