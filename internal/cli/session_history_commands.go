package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/session"
)

// durableHistoryCommands owns the session-specific slash routes. Administrative
// commands claim the same runtime slot as a turn and publish canonical history.
type durableHistoryCommands struct{ repl *REPLSession }

func (commands *durableHistoryCommands) handle(ctx context.Context, name string, args []string) (string, bool, error) {
	s := commands.repl
	switch name {
	case "save", "load":
		snapshot, err := s.assembly.durableSession.snapshotSelected()
		if err != nil {
			return "", true, err
		}
		if name == "save" {
			if _, err := fmt.Fprintf(s.out, "Durable records synced: physical session %s, revision %s (%d records). This does not resolve pending actions.\n", snapshot.SessionID, snapshot.PublishedView.Revision, len(snapshot.DurableEntries)); err != nil {
				return "", true, err
			}
		} else {
			if _, err := fmt.Fprintf(s.out, "Persisted session %s (%d records):\n", snapshot.SessionID, len(snapshot.DurableEntries)); err != nil {
				return "", true, err
			}
			for _, row := range snapshot.DurableEntries {
				if _, err := fmt.Fprintf(s.out, "  [%s] %s: %s\n", row.Type, row.ID, truncatePreview(row.Content)); err != nil {
					return "", true, err
				}
			}
		}
		return "", true, nil
	case "clear", "retry", "compact":
		var pending string
		err := s.withHistoryAdmission(ctx, func() error {
			snapshot, err := s.assembly.durableSession.snapshotSelected()
			if err != nil {
				return err
			}
			if err := validateHistoryMutationReady(snapshot); err != nil {
				return err
			}
			switch name {
			case "clear":
				if err := s.selectHistorySession(ctx, "", true); err != nil {
					return err
				}
				_, err := fmt.Fprintf(s.out, "Conversation history cleared in new physical session %s. Previous session retained: %s.\n", s.assembly.PhysicalSessionID, snapshot.SessionID)
				return err
			case "retry":
				var retryErr error
				pending, retryErr = s.retryHistory(ctx, snapshot)
				return retryErr
			case "compact":
				return s.compactHistory(ctx, snapshot)
			}
			return nil
		})
		return pending, true, err
	case "session":
		if len(args) == 0 {
			return "", false, nil
		}
		handlers := map[string]func(context.Context, []string) error{
			"resume": commands.resumeOrCatalog,
			"list":   commands.resumeOrCatalog,
			"fork":   s.dispatchHistoryCommand,
			"clone":  s.dispatchHistoryCommand,
			"switch": s.dispatchHistoryCommand,
			"entry":  s.dispatchHistoryCommand,
		}
		handler, ok := handlers[args[0]]
		if !ok {
			return "", false, nil
		}
		return "", true, handler(ctx, args)
	}
	return "", false, nil
}

// resumeOrCatalog preserves the no-target listing alias while exact targets
// continue through the normal administrative admission and selection path.
func (commands *durableHistoryCommands) resumeOrCatalog(ctx context.Context, args []string) error {
	if len(args) > 1 {
		return commands.repl.dispatchHistoryCommand(ctx, args)
	}
	s := commands.repl
	entries, err := session.ListSessionCatalog(ctx, s.assembly.durableSession.storeDirectory)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(s.out, "Physical sessions (exact IDs; busy entries are not opened):"); err != nil {
		return err
	}
	for _, entry := range entries {
		if _, err := fmt.Fprintf(s.out, "  %s [%s] %s %s\n", entry.SessionID, entry.Status, entry.Modified.Format(time.RFC3339), truncatePreview(entry.Preview)); err != nil {
			return err
		}
	}
	return nil
}

func (s *REPLSession) withHistoryAdmission(ctx context.Context, fn func() error) error {
	if s.historyAdmissionError != nil {
		return s.historyAdmissionError
	}
	if s.unknownCancelRecovery != nil {
		return errors.New("the interrupted run is unresolved; use /session pending or /session new")
	}
	claimer, ok := s.assembly.runSlotGuard.(core.RunSlotTryClaimer)
	if !ok {
		return errors.New("history administration requires nonblocking run admission")
	}
	claim, err := claimer.TryClaimRun()
	if err != nil {
		return err
	}
	defer s.assembly.runSlotGuard.Release(claim)
	return fn()
}

func validateHistoryMutationReady(snapshot session.PreparedSessionSnapshot) error {
	return session.ValidateHistoryAdministration(snapshot)
}

func (s *REPLSession) selectHistorySession(ctx context.Context, physicalID string, fresh bool) error {
	manager := s.assembly.durableSession.manager
	var err error
	if fresh {
		err = manager.prepareNew(ctx, "interactive context")
	} else {
		err = manager.selectResume(ctx, physicalID, "")
	}
	var applied *SelectionAppliedCleanupError
	if err != nil && !errors.As(err, &applied) {
		return err
	}
	snapshot, snapshotErr := s.assembly.durableSession.snapshotSelected()
	if snapshotErr != nil {
		s.historyAdmissionError = errors.Join(err, snapshotErr)
		if applied != nil {
			s.assembly.PhysicalSessionID = applied.SelectedPhysicalSessionID
			s.slashCtx.sessionID = applied.SelectedPhysicalSessionID
			s.assembly.Agent.ClearHistory()
		}
		return s.historyAdmissionError
	}
	if hydrateErr := s.hydrateHistorySelection(snapshot); hydrateErr != nil {
		s.assembly.PhysicalSessionID = snapshot.SessionID
		s.slashCtx.sessionID = snapshot.SessionID
		s.assembly.Agent.ClearHistory()
		s.historyAdmissionError = errors.Join(err, hydrateErr)
		return s.historyAdmissionError
	}
	if err != nil {
		s.historyAdmissionError = fmt.Errorf("physical session %s was selected; prior-session cleanup needs attention; restart before new work: %w", snapshot.SessionID, err)
		return s.historyAdmissionError
	}
	return nil
}

func (s *REPLSession) hydrateHistorySelection(snapshot session.PreparedSessionSnapshot) error {
	if err := s.applyDurableSessionSnapshot(snapshot); err != nil {
		return err
	}
	s.assembly.PhysicalSessionID = snapshot.SessionID
	s.slashCtx.sessionID = snapshot.SessionID
	return nil
}

func (s *REPLSession) applyHistoryEdit(ctx context.Context, edit session.HistoryEdit) error {
	snapshot, err := s.assembly.durableSession.manager.editHistory(ctx, edit)
	if err != nil {
		return err
	}
	return s.hydrateHistorySelection(snapshot)
}

func historyBranchTip(snapshot session.PreparedSessionSnapshot, branchID string) (string, error) {
	var tip string
	for _, row := range snapshot.DurableEntries {
		branch := row.BranchID
		if branch == "" {
			branch = "main"
		}
		if branch == branchID && row.CheckpointKind != session.CheckpointActiveLeaf {
			tip = row.ID
		}
	}
	if tip == "" {
		return "", fmt.Errorf("session: branch %q not found", branchID)
	}
	return tip, nil
}

func historyTarget(snapshot session.PreparedSessionSnapshot, target string) (string, string, error) {
	if leaf, err := historyBranchTip(snapshot, target); err == nil {
		return leaf, target, nil
	}
	return historyEntryTarget(snapshot, target)
}

// historyEntryTarget never interprets an exact entry ID as a branch name.
func historyEntryTarget(snapshot session.PreparedSessionSnapshot, target string) (string, string, error) {
	for _, row := range snapshot.DurableEntries {
		if row.ID == target {
			branch := row.BranchID
			if branch == "" {
				branch = "main"
			}
			return row.ID, branch, nil
		}
	}
	return "", "", fmt.Errorf("session: entry %q not found", target)
}

func (s *REPLSession) editHistoryCommand(ctx context.Context, args []string, gitBranch string) error {
	snapshot, err := s.assembly.durableSession.snapshotSelected()
	if err != nil {
		return err
	}
	if err := validateHistoryMutationReady(snapshot); err != nil {
		return err
	}
	edit := session.HistoryEdit{Expected: session.HistoryCoordinatesFromSnapshot(snapshot)}
	switch args[0] {
	case "fork":
		if len(args) > 2 {
			return errors.New("usage: /session fork [branch]; Git linkage requires the explicit managed Git path")
		}
		edit.Kind = session.HistoryFork
		edit.TargetEntryID = snapshot.ActiveLeafEntryID
		edit.TargetBranchID = snapshot.ActiveBranchID
		edit.GitBranch = gitBranch
		edit.NewBranchID = fmt.Sprintf("fork-%d", snapshot.MaxSeq+1)
		if len(args) == 2 {
			edit.NewBranchID = args[1]
		}
	case "clone":
		if len(args) != 3 {
			return errors.New("usage: /session clone <branch-or-entry> <new-branch>")
		}
		edit.TargetEntryID, edit.TargetBranchID, err = historyTarget(snapshot, args[1])
		if err != nil {
			return err
		}
		edit.Kind = session.HistoryFork
		edit.NewBranchID = args[2]
	case "switch":
		if len(args) != 2 {
			return errors.New("usage: /session switch <branch>")
		}
		edit.Kind = session.HistorySelect
		edit.TargetBranchID = args[1]
		edit.TargetEntryID, err = historyBranchTip(snapshot, args[1])
		if err != nil {
			return err
		}
	case "resume", "entry":
		if len(args) == 3 && args[1] == "--entry" {
			args = []string{"entry", args[2]}
		}
		if len(args) != 2 {
			return errors.New("usage: /session resume <physical-session-id> or /session entry <entry-id>")
		}
		if args[0] == "resume" {
			// Historical /session resume accepted current-tree entry IDs. Retain
			// that unambiguous compatibility while physical IDs select exact files.
			found := false
			for _, row := range snapshot.DurableEntries {
				if row.ID == args[1] {
					found = true
				}
			}
			if !found {
				return s.selectHistorySession(ctx, args[1], false)
			}
		}
		edit.Kind = session.HistorySelect
		edit.TargetEntryID, edit.TargetBranchID, err = historyEntryTarget(snapshot, args[1])
		if err != nil {
			return err
		}
	default:
		return errors.New("session: unknown administration command")
	}
	if err := s.applyHistoryEdit(ctx, edit); err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.out, "Selected physical session %s, branch %s.\n", s.assembly.PhysicalSessionID, s.assembly.durableSession.manager.selectedBranch())
	return err
}

func (s *REPLSession) retryHistory(ctx context.Context, snapshot session.PreparedSessionSnapshot) (string, error) {
	view := snapshot.PublishedView
	userIndex := -1
	for index := len(view.Messages) - 1; index >= 0; index-- {
		if view.Messages[index].Role == "user" {
			genuineUser := false
			for _, row := range snapshot.DurableEntries {
				if row.ID == view.SourceEntryIDs[index] && row.Type == session.EntryTypeUser {
					genuineUser = true
					break
				}
			}
			if !genuineUser {
				continue
			}
			userIndex = index
			break
		}
	}
	if userIndex < 0 || userIndex == len(view.Messages)-1 {
		return "", errors.New("no completed assistant response to retry")
	}
	var user session.SessionEntry
	for _, row := range snapshot.DurableEntries {
		if row.ID == view.SourceEntryIDs[userIndex] {
			user = row
		}
	}
	known := false
	for _, row := range snapshot.DurableEntries {
		if row.RunID == user.RunID && row.CheckpointKind == session.CheckpointRunTerminal {
			known = row.RunOutcome == core.RunOutcomeSuccess
		}
	}
	if !known {
		return "", errors.New("retry requires a positively completed successful owning run")
	}
	if s.turnResult.RunID == user.RunID {
		waiter, ok := s.turnResult.StatusHandle.(core.RunStatusWaiter)
		if !ok {
			return "", errors.New("retry requires its completed owning run status")
		}
		status, publication, err := waiter.WaitForCompletion(ctx)
		if err != nil || status.RunID != user.RunID || status.Outcome != core.RunOutcomeSuccess || status.ExecutionState != core.RunExecutionQuiescent || publication.State != core.DurablePublicationSynced || publication.RunID != user.RunID || publication.SessionID != snapshot.SessionID {
			return "", errors.Join(err, errors.New("retry owning run is not positively joined and published"))
		}
	}
	if userIndex == 0 {
		if err := s.selectHistorySession(ctx, "", true); err != nil {
			return "", err
		}
	} else {
		err := s.applyHistoryEdit(ctx, session.HistoryEdit{Expected: session.HistoryCoordinatesFromSnapshot(snapshot), Kind: session.HistoryFork, TargetEntryID: view.SourceEntryIDs[userIndex-1], TargetBranchID: snapshot.ActiveBranchID, NewBranchID: fmt.Sprintf("retry-%d", snapshot.MaxSeq+1)})
		if err != nil {
			return "", err
		}
	}
	if _, err := fmt.Fprintln(s.out, "Retrying last user message as new work; prior side effects remain."); err != nil {
		return "", err
	}
	return view.Messages[userIndex].Content, nil
}

func (s *REPLSession) compactHistory(ctx context.Context, snapshot session.PreparedSessionSnapshot) error {
	if s.assembly.Compactor == nil || s.assembly.Estimator == nil {
		return errors.New("compactor is not configured")
	}
	original := messagesToTurnItems(snapshot.PublishedView.Messages)
	for index := range original {
		original[index].ID = snapshot.PublishedView.SourceEntryIDs[index]
		original[index].Runtime = llm.RuntimeMetadata{}
	}
	ctx = core.WithAuxiliaryOperations(ctx, func(result llm.ProviderAttemptResult) error {
		if s.assembly.CostTracker == nil {
			return nil
		}
		_, err := s.assembly.CostTracker.RecordAttempt(result)
		return err
	}, nil)
	result, err := s.assembly.Compactor.Compact(ctx, original, s.assembly.MaxTokens, s.assembly.Estimator)
	if err != nil {
		return fmt.Errorf("manual compaction: %w", err)
	}
	// An unchanged conversation is an honest no-op, matching the configured
	// compactor's budget. Replacements require an exact preserved suffix.
	retained := len(original)
	suffix := len(result.Items)
	for retained > 0 && suffix > 0 && original[retained-1].ID != "" && original[retained-1].ID == result.Items[suffix-1].ID && original[retained-1].Content == result.Items[suffix-1].Content && original[retained-1].ToolResult == result.Items[suffix-1].ToolResult {
		retained--
		suffix--
	}
	if retained == 0 {
		_, err := fmt.Fprintln(s.out, "Conversation already fits the compaction budget.")
		return err
	}
	var summary strings.Builder
	summary.WriteString("Earlier conversation compacted by explicit user request.\n")
	for _, item := range result.Items[:suffix] {
		if item.Role == "system" && !item.IsCompaction {
			return errors.New("manual compaction cannot replace mandatory system instructions")
		}
		text := item.Content
		if item.ToolResult != "" {
			text = item.ToolResult
		}
		_, _ = fmt.Fprintf(&summary, "[%s] %s\n", item.Role, text) // strings.Builder writes cannot fail.
	}
	for _, item := range original[:retained] {
		if item.Role == "system" && !item.IsCompaction {
			return errors.New("manual compaction cannot replace mandatory system instructions")
		}
	}
	if summary.Len() > 512*1024 {
		return errors.New("manual compaction summary exceeds its limit")
	}
	if err := s.applyHistoryEdit(ctx, session.HistoryEdit{Expected: session.HistoryCoordinatesFromSnapshot(snapshot), Kind: session.HistoryCompact, TargetEntryID: original[retained-1].ID, TargetBranchID: snapshot.ActiveBranchID, Summary: summary.String()}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.out, "Compacted history: %d -> %d messages.\n", len(original), len(s.assembly.Agent.Messages()))
	return err
}
