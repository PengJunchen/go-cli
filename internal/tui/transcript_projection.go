package tui

import (
	"errors"
	"fmt"

	"github.com/pengjunchen/go-cli/internal/core"
)

var (
	ErrTranscriptRefreshInFlight = errors.New("tui: a transcript refresh is already in flight")
	ErrTranscriptRefreshClosed   = errors.New("tui: transcript refresh app is closed")
	ErrTranscriptRefreshIdentity = errors.New("tui: transcript refresh identity does not match the active app")
)

// DurableTranscriptKey identifies one canonical row in the selected durable
// session projection.
type DurableTranscriptKey struct {
	PhysicalSessionID string
	BranchID          string
	EntryID           string
}

// AssistantOccurrenceKey identifies one canonical assistant row in its run.
// Canonical transcript indexes and tool-call ordinals are distinct coordinates.
type AssistantOccurrenceKey struct {
	RunID                    string
	CanonicalTranscriptIndex uint64
}

// TranscriptRowKind identifies the durable source row kind. It is deliberately
// separate from AgentMessage.Role because compaction rows project as user
// messages for model context.
type TranscriptRowKind uint8

const (
	TranscriptRowUser TranscriptRowKind = iota + 1
	TranscriptRowAssistant
	TranscriptRowTool
	TranscriptRowSystem
	TranscriptRowCompaction
)

// CompactionFact preserves the durable boundary and exact summary separately
// from the provider-facing user-role representation.
type CompactionFact struct {
	CompactedThroughEntryID string
	Summary                 string
}

// ToolOccurrenceKey identifies one tool call in one assistant transcript
// occurrence. ToolCallID alone is not a stable occurrence identity.
type ToolOccurrenceKey struct {
	RunID                      string
	AssistantTranscriptOrdinal uint64
	ToolCallID                 string
}

// ActionJournalStage identifies the kind of journal row attached to a call.
type ActionJournalStage uint8

const (
	ActionJournalIntent ActionJournalStage = iota + 1
	ActionJournalExecution
)

// ActionJournalFact is a display-only copy of one exact durable action row.
type ActionJournalFact struct {
	EntryID         string
	ActionBatchID   string
	ActionID        string
	Stage           ActionJournalStage
	DispatchState   core.DispatchState
	ResourceState   core.RunExecutionState
	CompletionProof string
}

// ToolOccurrence contains action facts correlated to one exact tool call.
type ToolOccurrence struct {
	Key          ToolOccurrenceKey
	ToolName     string
	JournalFacts []ActionJournalFact
}

// CanonicalTranscriptRow is one visible durable message and its provenance.
type CanonicalTranscriptRow struct {
	Key                 DurableTranscriptKey
	Kind                TranscriptRowKind
	Message             core.AgentMessage
	SourceSeq           uint64
	ActionID            string
	Compaction          *CompactionFact
	AssistantOccurrence *AssistantOccurrenceKey
	Occurrence          *ToolOccurrenceKey
	SourceTruncated     *bool
	Calls               []ToolOccurrence
}

// CanonicalTranscriptSnapshot is a value-only replacement for one complete
// selected branch view.
type CanonicalTranscriptSnapshot struct {
	PhysicalSessionID string
	BranchID          string
	Revision          string
	MaxSeq            uint64
	ActiveLeafEntryID string
	Rows              []CanonicalTranscriptRow
}

// RunObservation copies the owner's status and, separately, a terminal row
// observed in the same durable snapshot. A terminal row is not an ACK.
type RunObservation struct {
	RunID           string
	OwnerGeneration string
	Outcome         core.RunOutcome
	ExecutionState  core.RunExecutionState
	EventGapCount   uint64
	TerminalEntryID string
	TerminalOutcome core.RunOutcome
}

// TranscriptRefresh is one same-source snapshot and owner observation applied
// atomically by the active model. Publication remains an explicit owner fact.
type TranscriptRefresh struct {
	Snapshot    CanonicalTranscriptSnapshot
	Observation RunObservation
	Publication core.DurablePublication
}

// Clone returns an independent snapshot, including nested message arguments.
func (s CanonicalTranscriptSnapshot) Clone() CanonicalTranscriptSnapshot {
	if s.Rows == nil {
		return s
	}
	messages := make([]core.AgentMessage, len(s.Rows))
	for index := range s.Rows {
		messages[index] = s.Rows[index].Message
	}
	messages = core.CloneAgentMessages(messages)
	rows := make([]CanonicalTranscriptRow, len(s.Rows))
	for index := range s.Rows {
		row := s.Rows[index]
		row.Message = messages[index]
		if row.Compaction != nil {
			fact := *row.Compaction
			row.Compaction = &fact
		}
		if row.AssistantOccurrence != nil {
			key := *row.AssistantOccurrence
			row.AssistantOccurrence = &key
		}
		if row.Occurrence != nil {
			key := *row.Occurrence
			row.Occurrence = &key
		}
		if row.SourceTruncated != nil {
			truncated := *row.SourceTruncated
			row.SourceTruncated = &truncated
		}
		if row.Calls != nil {
			row.Calls = append([]ToolOccurrence(nil), row.Calls...)
			for callIndex := range row.Calls {
				if row.Calls[callIndex].JournalFacts != nil {
					row.Calls[callIndex].JournalFacts = append([]ActionJournalFact(nil), row.Calls[callIndex].JournalFacts...)
				}
			}
		}
		rows[index] = row
	}
	s.Rows = rows
	return s
}

// Validate checks snapshot identity and row-level value invariants.
func (s CanonicalTranscriptSnapshot) Validate() error {
	if s.PhysicalSessionID == "" || s.BranchID == "" {
		return errors.New("tui: canonical transcript requires physical session and branch IDs")
	}
	if len(s.Rows) > 0 && s.Revision == "" {
		return errors.New("tui: non-empty canonical transcript requires a revision")
	}
	seenEntries := make(map[string]struct{}, len(s.Rows))
	for rowIndex, row := range s.Rows {
		if row.Key.PhysicalSessionID != s.PhysicalSessionID || row.Key.BranchID != s.BranchID || row.Key.EntryID == "" {
			return fmt.Errorf("tui: transcript row %d has a mismatched or incomplete durable key", rowIndex)
		}
		if _, exists := seenEntries[row.Key.EntryID]; exists {
			return fmt.Errorf("tui: transcript row %d repeats durable entry ID %q", rowIndex, row.Key.EntryID)
		}
		seenEntries[row.Key.EntryID] = struct{}{}
		if expected, ok := transcriptRoleForKind(row.Kind); !ok || row.Message.Role != expected {
			return fmt.Errorf("tui: transcript row %d kind %q does not match message role %q", rowIndex, row.Kind, row.Message.Role)
		}
		if (row.Kind == TranscriptRowCompaction) != (row.Compaction != nil) {
			return fmt.Errorf("tui: transcript row %d has inconsistent compaction facts", rowIndex)
		}
		if row.AssistantOccurrence != nil && (row.Kind != TranscriptRowAssistant || row.AssistantOccurrence.RunID == "") {
			return fmt.Errorf("tui: transcript row %d has an invalid assistant occurrence", rowIndex)
		}
		if row.Occurrence != nil {
			if row.Kind != TranscriptRowTool || row.Occurrence.RunID == "" || row.Occurrence.ToolCallID == "" ||
				row.Occurrence.ToolCallID != row.Message.ToolCallID || row.Message.ToolName == "" {
				return fmt.Errorf("tui: transcript row %d has an invalid tool occurrence", rowIndex)
			}
		}
		if row.SourceTruncated != nil && row.Kind != TranscriptRowTool {
			return fmt.Errorf("tui: transcript row %d has source truncation metadata on a non-tool row", rowIndex)
		}
		if len(row.Calls) > 0 && row.Kind != TranscriptRowAssistant {
			return fmt.Errorf("tui: non-assistant transcript row %d has tool call facts", rowIndex)
		}
		seenCalls := make(map[string]struct{}, len(row.Calls))
		lastCallPosition := -1
		for callIndex, call := range row.Calls {
			if call.Key.RunID == "" || call.Key.ToolCallID == "" || call.ToolName == "" {
				return fmt.Errorf("tui: assistant row %d has incomplete tool occurrence at index %d", rowIndex, callIndex)
			}
			messageCallPosition := toolCallPosition(row.Message, call.Key.ToolCallID, call.ToolName)
			if messageCallPosition < 0 || messageCallPosition <= lastCallPosition {
				return fmt.Errorf("tui: assistant row %d tool occurrence %q does not match its canonical message", rowIndex, call.Key.ToolCallID)
			}
			lastCallPosition = messageCallPosition
			if _, exists := seenCalls[call.Key.ToolCallID]; exists {
				return fmt.Errorf("tui: assistant row %d repeats tool occurrence %q", rowIndex, call.Key.ToolCallID)
			}
			seenCalls[call.Key.ToolCallID] = struct{}{}
			for factIndex, fact := range call.JournalFacts {
				if fact.EntryID == "" {
					return fmt.Errorf("tui: assistant row %d tool fact %d has no durable entry ID", rowIndex, factIndex)
				}
				if _, exists := seenEntries[fact.EntryID]; exists {
					return fmt.Errorf("tui: durable entry ID %q is repeated across transcript and journal facts", fact.EntryID)
				}
				seenEntries[fact.EntryID] = struct{}{}
				if fact.Stage != ActionJournalIntent && fact.Stage != ActionJournalExecution {
					return fmt.Errorf("tui: journal fact %q has an invalid stage", fact.EntryID)
				}
				if !validTranscriptDispatchState(fact.DispatchState) || !validTranscriptResourceState(fact.ResourceState) {
					return fmt.Errorf("tui: journal fact %q has an invalid dispatch or resource state", fact.EntryID)
				}
				if fact.CompletionProof != "" && (fact.Stage != ActionJournalExecution || !validTranscriptCompletionProof(fact.CompletionProof)) {
					return fmt.Errorf("tui: journal fact %q has an invalid completion proof", fact.EntryID)
				}
				if (fact.ActionBatchID == "") != (fact.ActionID == "") {
					return fmt.Errorf("tui: journal fact %q has a partial reservation identity", fact.EntryID)
				}
			}
		}
	}
	return nil
}

func transcriptRoleForKind(kind TranscriptRowKind) (string, bool) {
	switch kind {
	case TranscriptRowUser, TranscriptRowCompaction:
		return "user", true
	case TranscriptRowAssistant:
		return "assistant", true
	case TranscriptRowTool:
		return "tool", true
	case TranscriptRowSystem:
		return "system", true
	default:
		return "", false
	}
}

func toolCallPosition(message core.AgentMessage, id, name string) int {
	for index, call := range message.ToolCalls {
		if call.ID == id && call.Name == name {
			return index
		}
	}
	return -1
}

func validTranscriptDispatchState(state core.DispatchState) bool {
	switch state {
	case "", core.DispatchStateNotDispatched, core.DispatchStateDispatched:
		return true
	default:
		return false
	}
}

func validTranscriptResourceState(state core.RunExecutionState) bool {
	switch state {
	case "", core.RunExecutionRunning, core.RunExecutionCancelRequested, core.RunExecutionDraining,
		core.RunExecutionCleanupPending, core.RunExecutionQuiescent, core.RunExecutionQuarantined:
		return true
	default:
		return false
	}
}

func validTranscriptCompletionProof(proof string) bool {
	switch proof {
	case "inline_pipeline_joined", "queued_job_joined", "owned_process_group_joined":
		return true
	default:
		return false
	}
}

// ValidateTranscriptRefresh checks the same durable publication identity for
// graphical and headless consumers before either installs a replacement view.
func ValidateTranscriptRefresh(hasSnapshot bool, currentSession, currentBranch, expectedRunID, expectedOwnerGen string, refresh TranscriptRefresh) error {
	if err := refresh.Snapshot.Validate(); err != nil {
		return err
	}
	if !hasSnapshot || refresh.Snapshot.PhysicalSessionID != currentSession || refresh.Snapshot.BranchID != currentBranch {
		return ErrTranscriptRefreshIdentity
	}
	publication := refresh.Publication
	if publication.State != core.DurablePublicationUnbound && publication.State != core.DurablePublicationPending &&
		publication.State != core.DurablePublicationSynced && publication.State != core.DurablePublicationFailed {
		return fmt.Errorf("%w: invalid durable publication state %q", ErrTranscriptRefreshIdentity, publication.State)
	}
	if publication.State != core.DurablePublicationUnbound {
		if publication.RunID == "" || publication.SessionID != refresh.Snapshot.PhysicalSessionID || publication.OwnerGeneration == "" {
			return ErrTranscriptRefreshIdentity
		}
		if refresh.Observation.RunID != publication.RunID || refresh.Observation.OwnerGeneration != publication.OwnerGeneration {
			return ErrTranscriptRefreshIdentity
		}
		if expectedRunID != "" && publication.RunID != expectedRunID {
			return ErrTranscriptRefreshIdentity
		}
		if expectedOwnerGen != "" && publication.OwnerGeneration != expectedOwnerGen {
			return ErrTranscriptRefreshIdentity
		}
	}
	if publication.State == core.DurablePublicationSynced {
		if refresh.Observation.ExecutionState != core.RunExecutionQuiescent ||
			refresh.Observation.Outcome == core.RunOutcomeUnknown ||
			refresh.Observation.TerminalEntryID == "" ||
			refresh.Observation.TerminalOutcome != refresh.Observation.Outcome {
			return ErrTranscriptRefreshIdentity
		}
	}
	return nil
}
