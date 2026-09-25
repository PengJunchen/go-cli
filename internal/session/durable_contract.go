package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
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
	if v.BranchID == "" {
		return errors.New("session: view requires an active branch ID")
	}
	if len(v.Messages) != len(v.SourceEntryIDs) {
		return fmt.Errorf("session: view has %d messages and %d source entry IDs", len(v.Messages), len(v.SourceEntryIDs))
	}
	if len(v.Messages) > 0 && v.Revision == "" {
		return errors.New("session: non-empty view requires a durable revision")
	}
	seen := make(map[string]struct{}, len(v.SourceEntryIDs))
	for i, id := range v.SourceEntryIDs {
		if id == "" {
			return fmt.Errorf("session: view source entry ID at index %d is empty", i)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("session: view source entry ID %q is repeated", id)
		}
		seen[id] = struct{}{}
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

// PreparedSessionSnapshot is a read-only, defensive snapshot of the complete
// durable session state owned by one prepared handle. DurableEntries preserves
// physical JSONL append order, including legacy version-zero rows; it contains
// journal, checkpoint, hidden, and branch rows as well as transcript entries.
// PublishedView is the most recent complete model-history projection and may
// lag DurableEntries while a tool-call group is durably open. Neither field
// grants execution authority or replaces the prepared handle's file lock.
type PreparedSessionSnapshot struct {
	SessionID         string
	SessionLabel      string
	DurableEntries    []SessionEntry
	ActiveBranchID    string
	ActiveLeafEntryID string
	MaxSeq            uint64
	PublishedView     SessionView
}

// Clone returns an independent copy of the complete snapshot payload.
func (s PreparedSessionSnapshot) Clone() PreparedSessionSnapshot {
	if s.DurableEntries != nil {
		s.DurableEntries = cloneSessionEntries(s.DurableEntries)
	}
	s.PublishedView = s.PublishedView.Clone()
	return s
}

// PreparedSession is a locked, fully recovered session handle. View returns a
// deep copy. Snapshot returns one coherent deep copy of all durable rows and
// current session cursors while holding the store mutex; it errors after Close
// or after a write/flush/sync failure poisons the writer. DurableWriter remains
// bound to this handle and rejects writes after Close. Close is idempotent and
// releases this handle's lock and store.
type PreparedSession interface {
	View() SessionView
	Snapshot() (PreparedSessionSnapshot, error)
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

func validateVersion1EntryCoordinates(entry SessionEntry) error {
	if entry.RecordVersion == 0 {
		return nil
	}
	if entry.RecordVersion != 1 {
		return fmt.Errorf("session: unsupported record version %d", entry.RecordVersion)
	}
	if entry.CompletionProof != "" && (entry.Type != EntryTypeTool || entry.ActionState != ActionStateExecution) {
		return errors.New("session: completion proof is only valid on a version 1 execution row")
	}

	if entry.ActionState != "" {
		if entry.Type != EntryTypeTool {
			return errors.New("session: action journal row must have tool type")
		}
		if entry.RunID == "" || entry.AssistantTranscriptOrdinal == nil || entry.ToolCallID == "" || entry.ToolName == "" || entry.ActionBatchID == "" || entry.ActionID == "" {
			return errors.New("session: action journal row has incomplete occurrence identity")
		}
		if entry.TranscriptIndex != nil {
			return errors.New("session: action journal row cannot have a transcript index")
		}
		switch entry.ActionState {
		case ActionStateIntent:
			if !isHexDigest(entry.ArgumentDigest) || !isHexDigest(entry.SchemaDigest) {
				return errors.New("session: version 1 intent requires lowercase argument and schema digests")
			}
			if err := validateToolIdentitySnapshot(entry.ToolIdentity, entry.ToolName); err != nil {
				return err
			}
			if len(entry.EffectSet) == 0 {
				return errors.New("session: version 1 intent requires a captured effect set")
			}
			previous := ""
			for i, effect := range entry.EffectSet {
				value := string(effect)
				if value == "" || (i > 0 && value <= previous) {
					return errors.New("session: version 1 intent effect set must be non-empty, unique, and sorted")
				}
				previous = value
			}
			return nil
		case ActionStateExecution:
			switch entry.CompletionProof {
			case "", tools.CompletionInlinePipelineJoined, tools.CompletionQueuedJobJoined, tools.CompletionOwnedProcessGroupJoined:
			default:
				return fmt.Errorf("session: execution row has unsupported completion proof %q", entry.CompletionProof)
			}
			return nil
		default:
			return fmt.Errorf("session: unsupported action state %q", entry.ActionState)
		}
	}

	switch entry.Type {
	case EntryTypeUser:
		if entry.RunID == "" || entry.TranscriptIndex == nil {
			return errors.New("session: version 1 user transcript row requires run ID and transcript index")
		}
	case EntryTypeAssistant:
		if entry.RunID == "" || entry.TranscriptIndex == nil {
			return errors.New("session: version 1 assistant transcript row requires run ID and transcript index")
		}
		if len(entry.ToolCalls) > 0 {
			if entry.AssistantTranscriptOrdinal == nil {
				return errors.New("session: assistant tool-call row requires an occurrence ordinal")
			}
			for i, call := range entry.ToolCalls {
				if call.ID == "" || call.Name == "" {
					return fmt.Errorf("session: assistant tool-call row has incomplete call at index %d", i)
				}
			}
		}
	case EntryTypeTool:
		if entry.RunID == "" || entry.AssistantTranscriptOrdinal == nil || entry.TranscriptIndex == nil || entry.ToolCallID == "" || entry.ToolName == "" {
			return errors.New("session: version 1 tool receipt has incomplete occurrence identity")
		}
		if entry.ReceiptOutcome != core.ReceiptOutcomeSuccess && entry.ReceiptOutcome != core.ReceiptOutcomeError && entry.ReceiptOutcome != core.ReceiptOutcomeInterrupted && entry.ReceiptOutcome != core.ReceiptOutcomeUnknown {
			return fmt.Errorf("session: version 1 tool receipt has invalid outcome %q", entry.ReceiptOutcome)
		}
		if entry.DispatchState != DispatchStateNotDispatched && entry.DispatchState != DispatchStateDispatched {
			return fmt.Errorf("session: version 1 tool receipt has invalid dispatch state %q", entry.DispatchState)
		}
	case EntryTypeCompaction:
		if entry.CompactedThroughEntryID == "" {
			return errors.New("session: version 1 compaction row requires a source boundary")
		}
	case EntryTypeCheckpoint:
		if entry.CheckpointKind == "" {
			return errors.New("session: version 1 checkpoint requires a checkpoint kind")
		}
		if entry.CheckpointKind == CheckpointExecutionResolution {
			if entry.ExecutionResolution == nil || entry.RunID == "" || entry.ActionID == "" || entry.ExecutionResolution.RunID != entry.RunID || entry.ExecutionResolution.ActionID != entry.ActionID {
				return errors.New("session: execution resolution checkpoint has mismatched typed proof")
			}
			if entry.ExecutionResolution.EvidenceRef == "" {
				return errors.New("session: execution resolution checkpoint has no evidence reference")
			}
			switch entry.ExecutionResolution.Evidence {
			case core.ResolutionNeverDispatched, core.ResolutionLocalResourcesStopped, core.ResolutionRemoteTerminal, core.ResolutionOperatorReconciled:
			default:
				return fmt.Errorf("session: execution resolution checkpoint has unsupported evidence %q", entry.ExecutionResolution.Evidence)
			}
		}
	case EntryTypeSystem:
		// System rows may be legacy-style user-visible context, not run transcript.
	default:
		return fmt.Errorf("session: version 1 entry has unsupported type %q", entry.Type)
	}
	return nil
}

func validateToolIdentitySnapshot(identity *tools.TrustedToolIdentitySnapshot, expectedTool string) error {
	if identity == nil || identity.RegistrationSource == "" || identity.ToolName == "" || identity.RegistrationGeneration == 0 || identity.ToolName != expectedTool || !isHexDigest(identity.ExecutorFingerprint) || !isHexDigest(identity.ScopeFingerprint) {
		return errors.New("session: version 1 intent has an incomplete trusted tool identity snapshot")
	}
	return nil
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

type actionOccurrenceKey struct {
	sessionID string
	runID     string
	ordinal   uint64
	toolCall  string
}
