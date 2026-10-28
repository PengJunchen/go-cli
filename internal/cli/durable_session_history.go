package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/session"
)

func (manager *durableSessionManager) selectedBranch() string {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.current == nil {
		return ""
	}
	return manager.current.branch
}

// editHistory reserves both manager transition and its exclusive session lease.
// Store CAS, validation and sync are one operation; no manager mutex spans I/O.
func (manager *durableSessionManager) editHistory(ctx context.Context, edit session.HistoryEdit) (session.PreparedSessionSnapshot, error) {
	if err := manager.enterTransition(ctx); err != nil {
		return session.PreparedSessionSnapshot{}, err
	}
	defer manager.leaveTransition()
	lease, err := manager.acquireReconciliationLease()
	if err != nil {
		return session.PreparedSessionSnapshot{}, err
	}
	defer lease.CloseRunCommit()
	editor, ok := lease.PreparedSession().(session.HistoryEditor)
	if !ok {
		return session.PreparedSessionSnapshot{}, errors.New("selected session has no administrative history contract")
	}
	snapshot, err := editor.EditHistory(ctx, edit)
	manager.mu.Lock()
	slot := lease.slot
	if manager.current != slot || slot.closed || slot.closeReq {
		manager.mu.Unlock()
		return session.PreparedSessionSnapshot{}, errors.Join(err, ErrDurableSessionUnavailable)
	}
	manager.mu.Unlock()
	if err != nil {
		_, inspectErr := lease.PreparedSession().Snapshot()
		if errors.Is(inspectErr, session.ErrDurableWriterPoisoned) {
			manager.mu.Lock()
			slot.poisoned = true
			slot.historyUncertain = true
			manager.signalSlotLocked(slot)
			manager.mu.Unlock()
			return session.PreparedSessionSnapshot{}, fmt.Errorf("administrative write is uncertain; close and resume exact physical session %s: %w", lease.SessionID(), err)
		}
		return session.PreparedSessionSnapshot{}, err
	}
	manager.mu.Lock()
	if manager.current != slot || slot.closed || slot.closeReq || snapshot.SessionID != slot.physical {
		manager.mu.Unlock()
		return session.PreparedSessionSnapshot{}, ErrDurableSessionUnavailable
	}
	slot.branch = snapshot.ActiveBranchID
	manager.signalSlotLocked(slot)
	manager.mu.Unlock()
	return snapshot, nil
}

func forkDurablePromptSession(ctx context.Context, cliSession *durableCLISession, entryID string) error {
	snapshot, err := cliSession.snapshotSelected()
	if err != nil {
		return err
	}
	if err := validateHistoryMutationReady(snapshot); err != nil {
		return err
	}
	target, branch, err := historyEntryTarget(snapshot, entryID)
	if err != nil {
		return err
	}
	_, err = cliSession.manager.editHistory(ctx, session.HistoryEdit{Expected: session.HistoryCoordinatesFromSnapshot(snapshot), Kind: session.HistoryFork, TargetEntryID: target, TargetBranchID: branch, NewBranchID: fmt.Sprintf("prompt-fork-%d", snapshot.MaxSeq+1)})
	return err
}

// selectedSessionMentionResolver reads the validated transcript afresh for each
// lookup. It never opens a mutable legacy store or exposes journal-only rows.
type selectedSessionMentionResolver struct {
	snapshot func() (session.PreparedSessionSnapshot, error)
}

var _ MentionResolver = (*selectedSessionMentionResolver)(nil)

func (r *selectedSessionMentionResolver) Resolve(ctx context.Context, payload string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	snapshot, err := r.snapshot()
	if err != nil {
		return "", fmt.Errorf("session mention: selected session is unavailable: %w", err)
	}
	for _, row := range snapshot.DurableEntries {
		if row.ID != payload || !row.SurfaceVisible() {
			continue
		}
		// A mention is quoted transcript text, including another branch in
		// this physical file. The canonical converter excludes execution-only
		// journal/checkpoint rows and wraps compaction summaries as untrusted.
		messages := session.EntriesToAgentMessages([]session.SessionEntry{row})
		if len(messages) == 0 {
			break
		}
		if messages[0].Content == "" {
			return "", fmt.Errorf("session mention: empty session entry: %s", payload)
		}
		return fmt.Sprintf("[Session %s — %s]\n%s", row.ID, row.Type, messages[0].Content), nil
	}
	return "", fmt.Errorf("session mention: entry not found in selected physical session transcript: %s", payload)
}
