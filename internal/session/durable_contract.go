package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/core"
)

// DurableSessionWriter acknowledges entries only after the complete batch has
// been appended, flushed, and synced.
type DurableSessionWriter interface {
	AppendAndSync(ctx context.Context, entries ...SessionEntry) ([]SessionEntry, error)
}

// SessionView is an immutable projection of one physical session branch.
// Messages and SourceEntryIDs have a one-to-one positional mapping.
type SessionView struct {
	SessionID      string
	BranchID       string
	Revision       string
	Messages       []core.AgentMessage
	SourceEntryIDs []string
}

// Validate checks the required identity and source-message mapping.
func (v SessionView) Validate() error {
	if v.SessionID == "" {
		return errors.New("session: view requires a physical session ID")
	}
	if len(v.Messages) != len(v.SourceEntryIDs) {
		return fmt.Errorf("session: view has %d messages and %d source entry IDs", len(v.Messages), len(v.SourceEntryIDs))
	}
	for i, id := range v.SourceEntryIDs {
		if id == "" {
			return fmt.Errorf("session: view source entry ID at index %d is empty", i)
		}
	}
	return nil
}

// Clone returns an independent copy, including nested protocol JSON tool args.
func (v SessionView) Clone() SessionView {
	v.Messages = core.CloneAgentMessages(v.Messages)
	if v.SourceEntryIDs != nil {
		v.SourceEntryIDs = append([]string(nil), v.SourceEntryIDs...)
	}
	return v
}

// PreparedSession is a locked, fully recovered session handle. View returns a
// deep copy. DurableWriter remains bound to this handle and rejects writes
// after Close. Close is idempotent and releases this handle's lock and store.
type PreparedSession interface {
	View() SessionView
	DurableWriter() DurableSessionWriter
	Close() error
}

// SessionPreparer creates an independently locked handle that may be swapped
// into a runtime only after preparation succeeds. A failed preparation releases
// every resource it acquired and does not disturb a previously prepared handle.
type SessionPreparer interface {
	// PrepareNew creates a new physical session; label is display metadata only.
	PrepareNew(ctx context.Context, label string) (PreparedSession, error)
	// PrepareResume opens the specified physical session ID under its file lock.
	PrepareResume(ctx context.Context, physicalSessionID string) (PreparedSession, error)
}

// PendingActionRef identifies an assistant tool-call occurrence being checked
// against the global dispatch index. An unreserved open occurrence has empty
// ActionBatchID and ActionID; authority must query by occurrence and may only
// prove absence. Any marker without a matching local reservation is
// Unverifiable; recovery must not invent an ActionID.
type PendingActionRef struct {
	SessionID                  string `json:"session_id"`
	WorkspaceID                string `json:"workspace_id"`
	RunID                      string `json:"run_id"`
	ActionBatchID              string `json:"action_batch_id,omitempty"`
	AssistantTranscriptOrdinal uint64 `json:"assistant_transcript_ordinal"`
	ToolCallID                 string `json:"tool_call_id"`
	ActionID                   string `json:"action_id,omitempty"`
}

// Validate checks the durable occurrence key and all-or-none reservation IDs.
func (r PendingActionRef) Validate() error {
	if r.SessionID == "" || r.WorkspaceID == "" || r.RunID == "" || r.ToolCallID == "" {
		return errors.New("session: pending action reference has an incomplete occurrence key")
	}
	if (r.ActionBatchID == "") != (r.ActionID == "") {
		return errors.New("session: pending action reference has a partial reservation identity")
	}
	return nil
}

// DispatchMarkerState is the authoritative state of a global dispatch marker.
type DispatchMarkerState uint8

const (
	// DispatchMarkerPresent means the action may have dispatched and remains unresolved.
	DispatchMarkerPresent DispatchMarkerState = iota + 1
	// DispatchMarkerAbsent means the index proves there is no dispatch-ready marker.
	DispatchMarkerAbsent
	// DispatchMarkerResolved means matching durable resource resolution exists.
	DispatchMarkerResolved
	// DispatchMarkerUnverifiable means the index cannot safely establish action state.
	DispatchMarkerUnverifiable
)

// ValidatedExecutionResolution binds a typed resolution to its validated local
// checkpoint and the exact action occurrence it resolves.
type ValidatedExecutionResolution struct {
	Action            PendingActionRef         `json:"action"`
	CheckpointEntryID string                   `json:"checkpoint_entry_id"`
	CheckpointSeq     uint64                   `json:"checkpoint_seq"`
	Resolution        core.ExecutionResolution `json:"resolution"`
}

// PendingActionDecision is the ordered marker-inspection result for one full
// action occurrence reference. The echoed reference makes ordering and
// unreserved occurrences verifiable without inventing an ActionID.
type PendingActionDecision struct {
	Action      PendingActionRef    `json:"action"`
	MarkerState DispatchMarkerState `json:"marker_state"`
}

// RecoveryAuthority applies locally validated resolution checkpoints to the
// global workspace index, then inspects all open action occurrences. It must
// not call back into session storage while applying global resolutions. The
// returned decisions preserve input order; ActionID is the correlation guard
// for reserved occurrences, while unreserved occurrences remain identified by
// their occurrence key and cannot be assigned a fabricated ActionID.
type RecoveryAuthority interface {
	ApplyDurableResolutions(ctx context.Context, resolutions []ValidatedExecutionResolution) error
	InspectDispatchMarkers(ctx context.Context, refs []PendingActionRef) ([]PendingActionDecision, error)
}

func validatePendingActionDecisions(refs []PendingActionRef, decisions []PendingActionDecision) error {
	if len(refs) != len(decisions) {
		return fmt.Errorf("session: recovery authority returned %d decisions for %d references", len(decisions), len(refs))
	}
	seen := make(map[actionOccurrenceKey]struct{}, len(refs))
	seenActionIDs := make(map[string]struct{}, len(refs))
	for i, ref := range refs {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("session: invalid pending action reference at index %d: %w", i, err)
		}
		key := actionOccurrenceKey{
			sessionID: ref.SessionID,
			runID:     ref.RunID,
			ordinal:   ref.AssistantTranscriptOrdinal,
			toolCall:  ref.ToolCallID,
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("session: duplicate pending action occurrence at index %d", i)
		}
		seen[key] = struct{}{}
		if ref.ActionID != "" {
			if _, exists := seenActionIDs[ref.ActionID]; exists {
				return fmt.Errorf("session: duplicate pending action ID at index %d", i)
			}
			seenActionIDs[ref.ActionID] = struct{}{}
		}

		decision := decisions[i]
		if decision.Action != ref {
			return fmt.Errorf("session: recovery authority reference mismatch at index %d", i)
		}
		if !validDispatchMarkerState(decision.MarkerState) {
			return fmt.Errorf("session: recovery authority returned invalid marker state at index %d", i)
		}
		if ref.ActionID == "" && decision.MarkerState != DispatchMarkerAbsent && decision.MarkerState != DispatchMarkerUnverifiable {
			return fmt.Errorf("session: unreserved occurrence has a marker decision at index %d", i)
		}
	}
	return nil
}

func validDispatchMarkerState(state DispatchMarkerState) bool {
	switch state {
	case DispatchMarkerPresent, DispatchMarkerAbsent, DispatchMarkerResolved, DispatchMarkerUnverifiable:
		return true
	default:
		return false
	}
}

type actionOccurrenceKey struct {
	sessionID string
	runID     string
	ordinal   uint64
	toolCall  string
}
