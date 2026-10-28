package session

import (
	"context"
	"errors"
	"fmt"
)

// ValidateNewTurn checks the run-start snapshot before a CLI starts an owner or
// contacts a model. Recovery and inspection deliberately do not use this gate:
// they must remain available while a prior action is pending. Validation grants
// no execution authority and never changes the journal or workspace quarantine.
func (s *SessionRunCommitSink) ValidateNewTurn(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextErr(ctx); err != nil {
		return err
	}
	if s.closed {
		return errors.New("session: new-turn admission requires an open run-commit lease")
	}
	if s.runID != "" {
		return errors.New("session: new-turn admission must precede run ownership")
	}
	plan, err := analyzeDurableActionJournal(s.allRows, s.sessionID)
	if err != nil {
		return fmt.Errorf("session: validate new-turn action journal: %w", err)
	}
	if len(plan.pending) > 0 {
		return errors.New("session: pending actions prevent a new turn; inspect or reconcile the pending actions, or select a new physical session")
	}
	if err := validateToolCallJournalTail(s.sessionID, s.branchRows); err != nil {
		return fmt.Errorf("session: incomplete tool-call group prevents a new turn: %w", err)
	}
	return nil
}
