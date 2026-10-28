package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tools"
)

func (s *REPLSession) dispatchHistoryCommand(ctx context.Context, args []string) error {
	// Preflight releases its temporary claim before a host Git run takes the
	// same slot. A prior applied-selection cleanup failure forbids new work.
	if err := s.withHistoryAdmission(ctx, func() error { return nil }); err != nil {
		return err
	}
	original := append([]string(nil), args...)
	var cleaned []string
	gitBranch := ""
	gitRequested := false
	for index := 0; index < len(args); index++ {
		if args[index] == "--git" {
			gitRequested = true
			if args[0] == "fork" {
				if index+1 >= len(args) {
					return errors.New("/session fork --git requires a Git branch")
				}
				index++
				gitBranch = args[index]
			}
			continue
		}
		if strings.HasPrefix(args[index], "--git=") {
			gitRequested = true
			gitBranch = strings.TrimPrefix(args[index], "--git=")
			continue
		}
		cleaned = append(cleaned, args[index])
	}
	args = cleaned
	if len(args) == 0 {
		return errors.New("session command is required")
	}
	if args[0] != "fork" && args[0] != "switch" && gitRequested {
		return errors.New("git-aware administration supports fork and switch")
	}
	if args[0] == "switch" && s.rc != nil && s.rc.Session.GitAwareBranch != nil && *s.rc.Session.GitAwareBranch {
		gitRequested = true
	}
	if !gitRequested {
		return s.withHistoryAdmission(ctx, func() error { return s.editHistoryCommand(ctx, args, "") })
	}
	snapshot, err := s.assembly.durableSession.snapshotSelected()
	if err != nil {
		return err
	}
	if err := validateHistoryMutationReady(snapshot); err != nil {
		return err
	}
	if s.unknownCancelRecovery != nil {
		return errors.New("unknown canceled run prevents Git-aware administration")
	}
	var call tools.ToolCall
	switch args[0] {
	case "fork":
		if len(args) > 2 || gitBranch == "" {
			return errors.New("usage: /session fork [branch] --git <git-branch>")
		}
		if len(args) == 2 {
			if strings.TrimSpace(args[1]) == "" || len(args[1]) > 128 || strings.ContainsAny(args[1], "/\\\x00\r\n\t ") {
				return errors.New("invalid session branch ID")
			}
			if _, err := historyBranchTip(snapshot, args[1]); err == nil {
				return errors.New("session branch already exists")
			}
		}
		call = tools.ToolCall{Name: "git_create_branch", Args: map[string]any{"name": gitBranch}}
	case "switch":
		if len(args) != 2 {
			return errors.New("usage: /session switch <branch> [--git]")
		}
		if _, err := historyBranchTip(snapshot, args[1]); err != nil {
			return err
		}
		linked := ""
		for _, row := range snapshot.DurableEntries {
			if row.CheckpointKind == session.CheckpointBranch && row.BranchID == args[1] {
				linked = row.SessionGitBranch
			}
		}
		if linked == "" {
			// Configured Git awareness only applies to branches explicitly linked.
			if len(original) == len(args) {
				return s.withHistoryAdmission(ctx, func() error { return s.editHistoryCommand(ctx, args, "") })
			}
			return errors.New("session branch has no Git linkage")
		}
		if gitBranch != "" && gitBranch != linked {
			return errors.New("requested Git branch differs from recorded session linkage")
		}
		gitBranch = linked
		call = tools.ToolCall{Name: "git_checkout", Args: map[string]any{"branch": gitBranch}}
	}
	if err := s.runHistoryGitAction(ctx, "/session "+strings.Join(original, " "), call); err != nil {
		return err
	}
	if err := s.withHistoryAdmission(ctx, func() error { return s.editHistoryCommand(ctx, args, gitBranch) }); err != nil {
		return fmt.Errorf("git action completed for %s, but session selection was not acknowledged (physical %s, branch %s); no automatic rollback was attempted: %w", gitBranch, s.assembly.PhysicalSessionID, s.assembly.durableSession.manager.selectedBranch(), err)
	}
	return nil
}

func (s *REPLSession) runHistoryGitAction(ctx context.Context, description string, call tools.ToolCall) error {
	if err := s.withHistoryAdmission(ctx, func() error { return nil }); err != nil {
		return err
	}
	runCtx, sink, err := s.assembly.durableSession.bindRun(ctx)
	if err != nil {
		return err
	}
	result, runErr := s.assembly.TurnRunner.RunHostToolAction(runCtx, s.assembly.LoopAgent, core.HostToolActionRequest{Description: description, Call: call})
	if result.StatusHandle == nil {
		sink.CloseRunCommit()
		return errors.Join(runErr, errors.New("git administrative action has no joined owner"))
	}
	waiter, ok := result.StatusHandle.(core.RunStatusWaiter)
	if !ok {
		return errors.Join(runErr, errors.New("git administrative owner cannot be joined"))
	}
	joinCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), durableOwnerJoinWait)
	defer cancel()
	status, publication, joinErr := waiter.WaitForCompletion(joinCtx)
	if joinErr != nil || status.RunID != result.RunID || status.ExecutionState != core.RunExecutionQuiescent || status.Outcome == core.RunOutcomeUnknown || publication.State != core.DurablePublicationSynced || publication.SessionID != s.assembly.PhysicalSessionID {
		return errors.Join(runErr, joinErr, errors.New("git administrative action remains uncertain; inspect /session pending; no retry or rollback occurred"))
	}
	snapshot, snapshotErr := s.assembly.durableSession.snapshotSelected()
	if snapshotErr != nil {
		return errors.Join(runErr, snapshotErr)
	}
	if err := s.hydrateHistorySelection(snapshot); err != nil {
		return errors.Join(runErr, err)
	}
	if runErr != nil || status.Outcome != core.RunOutcomeSuccess {
		return errors.Join(runErr, core.ErrHostToolActionFailed)
	}
	return nil
}
