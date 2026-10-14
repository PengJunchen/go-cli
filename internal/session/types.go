// Package session provides the conversational session storage layer: an
// append-only entry store, a branching session tree, and JSONL persistence. It
// implements the richer Phase 2 session interfaces (SessionStore and
// SessionTree) that build on top of the core stubs.
package session

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// EntryType categorizes a single immutable session entry.
type EntryType string

func init() {
	slog.Debug("session types initialized", "entry_types", []string{"user", "assistant", "tool", "compaction", "system", "checkpoint"})
}

const (
	// EntryTypeUser is a message authored by the human user.
	EntryTypeUser EntryType = "user"
	// EntryTypeAssistant is a message produced by the assistant.
	EntryTypeAssistant EntryType = "assistant"
	// EntryTypeTool is a tool invocation or its result.
	EntryTypeTool EntryType = "tool"
	// EntryTypeCompaction is a summarized replacement for older history.
	EntryTypeCompaction EntryType = "compaction"
	// EntryTypeSystem is a system-level informational entry.
	EntryTypeSystem EntryType = "system"
	// EntryTypeCheckpoint stores durable run, branch, and recovery checkpoints.
	EntryTypeCheckpoint EntryType = "checkpoint"
)

// SurfaceOp controls how an entry is projected into the model-visible message
// stream by DeriveMessages. It separates the storage format from the
// model-visible format.
type SurfaceOp string

const (
	// SurfaceOpVisible marks an entry as included in the model context. This
	// is the default behavior when SurfaceOp is empty.
	SurfaceOpVisible SurfaceOp = "visible"
	// SurfaceOpCompacted marks an entry as replaced by a compaction summary.
	// It is not included individually in the model context.
	SurfaceOpCompacted SurfaceOp = "compacted"
	// SurfaceOpHidden marks an entry as excluded from the model context.
	SurfaceOpHidden SurfaceOp = "hidden"
)

// SessionEntry is a single immutable record in a session. Entries are linked
// into a tree via ParentID and are never mutated once appended.
type SessionEntry struct {
	ID            string             `json:"id"`
	ParentID      string             `json:"parent_id,omitempty"`
	Type          EntryType          `json:"type"`
	Content       string             `json:"content"`
	ContentBlocks []llm.ContentBlock `json:"content_blocks,omitempty"`
	// Usage preserves token accounting reported on the canonical assistant
	// message so durable history can be compared with the R1 transcript.
	Usage     *llm.Usage `json:"usage,omitempty"`
	Timestamp time.Time  `json:"timestamp"`
	Summary   string     `json:"summary,omitempty"`
	// IsSummary marks an entry as an auto-generated branch summary appended to
	// a departed branch by SessionTree.MoveTo. It does not affect replay.
	IsSummary bool `json:"is_summary,omitempty"`
	// ToolCalls holds tool invocations requested by the assistant. Populated
	// for assistant entries that request tool execution.
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID associates a tool-result entry with the originating tool
	// call. Populated for tool entries.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName is the name of the tool that produced this entry. Populated
	// for tool entries.
	ToolName string `json:"tool_name,omitempty"`
	// SurfaceOp controls how DeriveMessages projects this entry into the
	// model-visible message stream. An empty value is treated as
	// SurfaceOpVisible.
	SurfaceOp SurfaceOp `json:"surface_op,omitempty"`
	// Seq is a monotonically increasing sequence number assigned by the
	// session tree when the entry is appended. It is zero when unset.
	Seq uint64 `json:"seq"`

	// RecordVersion identifies the additive on-disk record schema. Missing
	// values are legacy version zero.
	RecordVersion uint16 `json:"record_version,omitempty"`
	// SessionID is the opaque physical identity of the session file.
	SessionID string `json:"session_id,omitempty"`
	// SessionLabel preserves a user-facing name without using it as a path.
	SessionLabel string `json:"session_label,omitempty"`
	// BranchID identifies the branch that owns this entry or checkpoint.
	BranchID string `json:"branch_id,omitempty"`
	// ParentBranchID identifies the source branch for a branch checkpoint.
	ParentBranchID string `json:"parent_branch_id,omitempty"`
	// BaseLeafEntryID identifies the shared prefix leaf when a branch is created.
	BaseLeafEntryID string `json:"base_leaf_entry_id,omitempty"`
	// ActiveLeafEntryID is the durable active leaf recorded by a checkpoint.
	ActiveLeafEntryID string `json:"active_leaf_entry_id,omitempty"`
	// RunID identifies the run that produced the journal or transcript entry.
	RunID string `json:"run_id,omitempty"`
	// ActionBatchID groups a sequential reservation or parallel dispatch batch.
	ActionBatchID string `json:"action_batch_id,omitempty"`
	// ActionID identifies one reserved executor invocation.
	ActionID string `json:"action_id,omitempty"`
	// OwnerGeneration identifies the runtime generation that launched an action.
	OwnerGeneration string `json:"owner_generation,omitempty"`
	// WorkspaceID is a stable digest of the canonical workspace root.
	WorkspaceID string `json:"workspace_id,omitempty"`
	// InputKind preserves whether a user message is an initial, steering, or
	// follow-up input.
	InputKind string `json:"input_kind,omitempty"`
	// ActionState separates journal-only intent/execution rows from receipts.
	ActionState ActionState `json:"action_state,omitempty"`
	// DispatchState records whether a reserved action crossed its dispatch fence.
	DispatchState DispatchState `json:"dispatch_state,omitempty"`
	// ResourceState is the run owner's local resource observation for an
	// execution row. It is descriptive and never proves quiescence by itself.
	ResourceState core.RunExecutionState `json:"resource_state,omitempty"`
	// ReceiptOutcome records the known or unknown terminal outcome of a tool call.
	ReceiptOutcome string `json:"receipt_outcome,omitempty"`
	// PayloadDigest stores the canonical digest used to detect conflicting retries.
	PayloadDigest string `json:"payload_digest,omitempty"`
	// ArgumentDigest is the SHA-256 digest of normalized arguments, in lowercase hex.
	ArgumentDigest string `json:"argument_digest,omitempty"`
	// SchemaDigest is the SHA-256 digest of the captured tool schema, in lowercase hex.
	SchemaDigest string `json:"schema_digest,omitempty"`
	// ToolIdentity is a durable description of the executor registration. It
	// records facts only and cannot restore runtime dispatch authority.
	ToolIdentity *tools.TrustedToolIdentitySnapshot `json:"tool_identity,omitempty"`
	// EffectSet records trusted effect labels as facts. Recovery never turns
	// these labels back into a trusted runtime effect set.
	EffectSet []tools.TrustedToolEffect `json:"effect_set,omitempty"`
	// ExecutionIdentity describes a verifiable local resource identity when one
	// was captured. Its persisted values are diagnostics, not permission to signal
	// a process after restart.
	ExecutionIdentity *ExecutionIdentity `json:"execution_identity,omitempty"`
	// CompletionProof is a closed, action-scoped proof captured only after the
	// prepared invocation's owned resources have signaled completion. It is
	// validated against the complete action reference during recovery; identity
	// fields remain diagnostic and are not themselves quiescence proof.
	CompletionProof tools.PreparedCompletionProof `json:"completion_proof,omitempty"`
	// AssistantTranscriptOrdinal identifies the assistant tool-call message
	// occurrence within its run.
	AssistantTranscriptOrdinal *uint64 `json:"assistant_transcript_ordinal,omitempty"`
	// TranscriptIndex is the zero-based index of a canonical transcript item.
	TranscriptIndex *uint64 `json:"transcript_index,omitempty"`
	// CompactedThroughEntryID is the exact source boundary covered by a summary.
	CompactedThroughEntryID string `json:"compacted_through_entry_id,omitempty"`
	// RunOutcome records the terminal logical result of a run.
	RunOutcome core.RunOutcome `json:"run_outcome,omitempty"`
	// CheckpointKind identifies the durable checkpoint represented by this row.
	CheckpointKind CheckpointKind `json:"checkpoint_kind,omitempty"`
	// ExecutionResolution is the typed resource-quiescence proof carried by a
	// checkpoint; a terminal receipt alone is never a resolution.
	ExecutionResolution *core.ExecutionResolution `json:"execution_resolution,omitempty"`
	// OperatorReconciliation records the explicit, user-approved acceptance of
	// an unresolved action's unknown outcome. It is audit evidence only.
	OperatorReconciliation *OperatorReconciliationRecord `json:"operator_reconciliation,omitempty"`
}

// clone returns a defensive copy of the entry so callers cannot mutate the
// stored immutable record through the returned pointer.
func (e *SessionEntry) clone() *SessionEntry {
	if e == nil {
		return nil
	}
	cp := *e
	message := core.AgentMessage{
		Role:           string(e.Type),
		Content:        e.Content,
		ContentBlocks:  e.ContentBlocks,
		Usage:          e.Usage,
		ToolCalls:      e.ToolCalls,
		ToolCallID:     e.ToolCallID,
		ToolName:       e.ToolName,
		InputKind:      e.InputKind,
		ReceiptOutcome: e.ReceiptOutcome,
	}
	cloned := core.CloneAgentMessages([]core.AgentMessage{message})[0]
	cp.ContentBlocks = cloned.ContentBlocks
	cp.Usage = cloned.Usage
	cp.ToolCalls = cloned.ToolCalls
	if e.ExecutionIdentity != nil {
		identity := *e.ExecutionIdentity
		cp.ExecutionIdentity = &identity
	}
	if e.ToolIdentity != nil {
		identity := *e.ToolIdentity
		cp.ToolIdentity = &identity
	}
	if e.EffectSet != nil {
		cp.EffectSet = append([]tools.TrustedToolEffect(nil), e.EffectSet...)
	}
	if e.ExecutionResolution != nil {
		resolution := *e.ExecutionResolution
		cp.ExecutionResolution = &resolution
	}
	if e.OperatorReconciliation != nil {
		reconciliation := *e.OperatorReconciliation
		cp.OperatorReconciliation = &reconciliation
	}
	if e.AssistantTranscriptOrdinal != nil {
		ordinal := *e.AssistantTranscriptOrdinal
		cp.AssistantTranscriptOrdinal = &ordinal
	}
	if e.TranscriptIndex != nil {
		index := *e.TranscriptIndex
		cp.TranscriptIndex = &index
	}
	return &cp
}

// ActionState describes a durable row in the action journal.
type ActionState string

const (
	// ActionStateIntent marks a journal-only reservation row.
	ActionStateIntent ActionState = "intent"
	// ActionStateExecution marks a journal-only executor lifecycle row.
	ActionStateExecution ActionState = "execution"
)

// DispatchState aliases the core action dispatch state for durable rows.
type DispatchState = core.DispatchState

const (
	DispatchStateNotDispatched = core.DispatchStateNotDispatched
	DispatchStateDispatched    = core.DispatchStateDispatched
)

// CheckpointKind identifies the durable checkpoint represented by an entry.
type CheckpointKind string

// OperatorReconciliationRecord records a user's explicit acceptance that an
// action's dispatched outcome remains unknown. It does not claim termination.
type OperatorReconciliationRecord struct {
	Reason              string `json:"reason"`
	Reference           string `json:"reference,omitempty"`
	AcceptedUnknownRisk bool   `json:"accepted_unknown_risk"`
}

const (
	maxOperatorReconciliationReasonBytes    = 512
	maxOperatorReconciliationReferenceBytes = 1024
)

func (record OperatorReconciliationRecord) validate() error {
	if !record.AcceptedUnknownRisk {
		return errors.New("session: operator reconciliation requires explicit unknown-risk acceptance")
	}
	if strings.TrimSpace(record.Reason) == "" || len(record.Reason) > maxOperatorReconciliationReasonBytes || strings.ContainsAny(record.Reason, "\x00\r\n") {
		return errors.New("session: operator reconciliation reason must be non-empty, single-line, and at most 512 bytes")
	}
	if len(record.Reference) > maxOperatorReconciliationReferenceBytes || strings.ContainsAny(record.Reference, "\x00\r\n") {
		return errors.New("session: operator reconciliation reference must be single-line and at most 1024 bytes")
	}
	return nil
}

const (
	// CheckpointRunTerminal marks a terminal run checkpoint.
	CheckpointRunTerminal CheckpointKind = "run_terminal"
	// CheckpointExecutionResolution marks validated action-resource evidence.
	CheckpointExecutionResolution CheckpointKind = "execution_resolution"
	// CheckpointActiveLeaf marks a branch's durable active leaf.
	CheckpointActiveLeaf CheckpointKind = "active_leaf"
	// CheckpointBranch marks a branch fork over a shared immutable prefix.
	CheckpointBranch CheckpointKind = "branch"
	// CheckpointReconciliation marks explicit operator reconciliation.
	CheckpointReconciliation CheckpointKind = "reconciliation"
)

// ExecutionIdentity remains an alias so the JSON storage API stays compatible
// while core owns the typed action commit contract.
type ExecutionIdentity = core.ExecutionIdentity

// ExecutionIdentityKind remains an alias for source compatibility.
type ExecutionIdentityKind = core.ExecutionIdentityKind

const (
	ExecutionIdentityProcessGroup = core.ExecutionIdentityProcessGroup
	ExecutionIdentityWindowsJob   = core.ExecutionIdentityWindowsJob
)

// SurfaceVisible reports whether the entry should be included in the
// model-visible message stream. It returns true when SurfaceOp is empty
// (the default) or explicitly set to SurfaceOpVisible.
func (e SessionEntry) SurfaceVisible() bool {
	return e.SurfaceOp == "" || e.SurfaceOp == SurfaceOpVisible
}

// SessionContext is the effective, replayable context for a session branch.
type SessionContext struct {
	// LeafID is the entry the context was reconstructed for.
	LeafID string `json:"leaf_id"`
	// RootID is the root entry of the reconstructed branch.
	RootID string `json:"root_id,omitempty"`
	// Messages are the ordered (root to leaf) effective messages for the
	// context. Compaction entries are folded into a single summary message.
	Messages []SessionEntry `json:"messages"`
	// Traversed are the raw entries visited while walking from the leaf back to
	// the root, in walk (leaf to root) order.
	Traversed []SessionEntry `json:"traversed,omitempty"`
	// EntryCount is the number of effective messages in Messages.
	EntryCount int `json:"entry_count"`
	// EstimatedTokens is a heuristic estimate of the token count for the
	// reconstructed context.
	EstimatedTokens int `json:"estimated_tokens,omitempty"`
	// LastUpdate is the most recent timestamp among the traversed entries.
	LastUpdate time.Time `json:"last_update,omitempty"`
}

// SessionStore persists and loads immutable session entries.
type SessionStore interface {
	// Append inserts an immutable entry without overwriting an existing id.
	Append(ctx context.Context, entry *SessionEntry) error
	// Get returns a defensive copy of the entry with the given id.
	Get(ctx context.Context, id string) (*SessionEntry, error)
	// Save flushes any buffered state (no-op for in-memory stores).
	Save(ctx context.Context) error
}

// SessionTree manages the append-only branching structure of a session.
type SessionTree interface {
	// Append adds an immutable entry to the tree.
	Append(ctx context.Context, entry *SessionEntry) error
	// MoveTo sets the current leaf pointer to the given entry id.
	MoveTo(ctx context.Context, leafID string) error
	// GetBranch returns the ordered entries from root to leafID.
	GetBranch(ctx context.Context, leafID string) ([]*SessionEntry, error)
	// BuildContext reconstructs the effective context for a branch.
	BuildContext(ctx context.Context, leafID string) (*SessionContext, error)
	// CurrentLeaf returns the id of the current leaf, or "" when empty.
	CurrentLeaf() string
	// Branch zero-copy points the current leaf at the existing entry fromID,
	// establishing a new branch without copying any entries.
	Branch(ctx context.Context, fromID string, opts ...BranchOption) error
	// ListBranches returns metadata for all branches in the tree.
	ListBranches() []BranchMeta
	// Clone deep-copies entries from the source branch into a new branch ID.
	Clone(ctx context.Context, fromBranchID, newBranchID string) error
}
