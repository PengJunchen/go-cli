package cli

import (
	"errors"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/session"
	"github.com/pengjunchen/go-cli/internal/tui"
)

type durableUnknownCancelRecovery struct {
	sessionID       string
	activeBranchID  string
	runID           string
	ownerGeneration string
	terminalEntryID string
}

// unknownCancellationOwnerEvidence keeps the explicit UI interruption,
// successful owner join, quiescent unknown outcome, and durable publication
// binding in one fail-closed predicate. Local resource quiescence alone never
// changes the run's unknown logical outcome.
type unknownCancellationOwnerEvidence struct {
	explicitUICancel bool
	parentContextErr error
	turnContextErr   error
	joinErr          error
	statusHandle     bool
	resultRunID      string
	initialSessionID string
	joinedStatus     core.RunStatus
	publication      core.DurablePublication
	runSlotActive    bool
}

func validUnknownCancellationOwnerEvidence(evidence unknownCancellationOwnerEvidence) bool {
	return evidence.explicitUICancel && evidence.parentContextErr == nil && evidence.turnContextErr != nil &&
		evidence.joinErr == nil && evidence.statusHandle &&
		evidence.resultRunID != "" && evidence.initialSessionID != "" &&
		evidence.joinedStatus.RunID == evidence.resultRunID &&
		evidence.joinedStatus.ExecutionState == core.RunExecutionQuiescent &&
		evidence.joinedStatus.Outcome == core.RunOutcomeUnknown &&
		evidence.publication.State == core.DurablePublicationSynced &&
		evidence.publication.RunID == evidence.resultRunID &&
		evidence.publication.SessionID == evidence.initialSessionID &&
		evidence.publication.OwnerGeneration != "" && !evidence.runSlotActive
}

func validateUnknownCancellationRecoverySelection(
	recovery *durableUnknownCancelRecovery,
	resultRunID string,
	status core.RunStatus,
	publication core.DurablePublication,
	snapshot session.PreparedSessionSnapshot,
	observation tui.RunObservation,
) error {
	if recovery == nil || recovery.sessionID == "" || recovery.activeBranchID == "" || recovery.runID == "" ||
		recovery.ownerGeneration == "" || recovery.terminalEntryID == "" {
		return errors.New("interactive: unknown cancellation recovery binding is incomplete")
	}
	if resultRunID != recovery.runID || status.RunID != recovery.runID ||
		status.ExecutionState != core.RunExecutionQuiescent || status.Outcome != core.RunOutcomeUnknown {
		return errors.New("interactive: the joined unknown run no longer matches the saved recovery binding")
	}
	if publication.State != core.DurablePublicationSynced || publication.RunID != recovery.runID ||
		publication.SessionID != recovery.sessionID || publication.OwnerGeneration != recovery.ownerGeneration {
		return errors.New("interactive: the durable owner publication no longer matches the saved recovery binding")
	}
	if snapshot.SessionID != recovery.sessionID || snapshot.ActiveBranchID != recovery.activeBranchID {
		return errors.New("interactive: the selected durable session or branch changed; the interrupted run remains fenced")
	}
	if observation.RunID != recovery.runID || observation.ExecutionState != core.RunExecutionQuiescent ||
		observation.Outcome != core.RunOutcomeUnknown || observation.TerminalEntryID != recovery.terminalEntryID ||
		observation.TerminalOutcome != core.RunOutcomeUnknown {
		return errors.New("interactive: the selected durable snapshot no longer contains the exact unknown terminal checkpoint")
	}
	return nil
}
