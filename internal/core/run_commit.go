package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type runIDContextKey struct{}

// RunIDFromContext returns the run identifier owned by the current run. It
// returns an empty string when the context is nil or no run owner has attached
// an identifier.
func RunIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	runID, _ := ctx.Value(runIDContextKey{}).(string)
	return runID
}

func withRunID(ctx context.Context, runID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, runIDContextKey{}, runID)
}

type runCommitContextKey struct{}

// RunCommitBinding identifies the physical session and workspace bound to a
// durable run. OwnerGeneration is overwritten by the run owner before the
// callback begins; callers must not use a supplied value as authority.
type RunCommitBinding struct {
	SessionID              string
	WorkspaceID            string
	CanonicalWorkspaceRoot string `json:"-"`
	OwnerGeneration        string
}

// ErrInvalidRunCommitBinding indicates that a durable run lacks a valid
// physical session, workspace, or owner-generation identifier.
var ErrInvalidRunCommitBinding = errors.New("core: invalid run commit binding")

// ErrRunCommitLeaseClosePanicked reports that an optional per-run sink lease
// release panicked. The owner does not retry an uncertain release, but still
// completes local run-slot cleanup after all owned resources have joined.
var ErrRunCommitLeaseClosePanicked = errors.New("core: run commit lease close panicked")

// ErrRunSlotReleasePanicked reports an uncertain panic from the run-slot guard release.
var ErrRunSlotReleasePanicked = errors.New("core: run slot release panicked")

// ErrRunCommitTerminalNotSynced indicates that a bound durable run ended
// without an acknowledged terminal checkpoint. The run is unknown until the
// sink confirms the terminal append and sync.
var ErrRunCommitTerminalNotSynced = errors.New("core: run commit terminal was not synced")

// Validate reports whether the binding contains the minimum valid durable
// identifiers. The run owner supplies OwnerGeneration before invoking user
// code.
func (binding RunCommitBinding) Validate() error {
	for _, field := range []struct{ name, value string }{
		{name: "session_id", value: binding.SessionID},
		{name: "workspace_id", value: binding.WorkspaceID},
		{name: "owner_generation", value: binding.OwnerGeneration},
	} {
		name, value := field.name, field.value
		if value == "" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%w: %s must be non-empty and contain no NUL", ErrInvalidRunCommitBinding, name)
		}
	}
	return nil
}

type runCommitContextValue struct {
	sink    RunCommitSink
	binding RunCommitBinding
}

// WithRunCommit atomically attaches a durable sink and its physical session
// and workspace descriptor. The run owner replaces OwnerGeneration before
// invoking the callback.
func WithRunCommit(ctx context.Context, sink RunCommitSink, binding RunCommitBinding) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if sink == nil {
		binding = RunCommitBinding{}
	}
	return context.WithValue(ctx, runCommitContextKey{}, runCommitContextValue{sink: sink, binding: binding})
}

// RunCommitFromContext returns the sink and binding attached to the current
// run, if any. A caller-provided OwnerGeneration is not trusted; the run owner
// replaces it for each callback.
func RunCommitFromContext(ctx context.Context) (RunCommitSink, RunCommitBinding, bool) {
	if ctx == nil {
		return nil, RunCommitBinding{}, false
	}
	value, ok := ctx.Value(runCommitContextKey{}).(runCommitContextValue)
	if !ok || value.sink == nil {
		return nil, RunCommitBinding{}, false
	}
	return value.sink, value.binding, true
}

// WithRunCommitSink is the legacy sink-only helper. It always clears any
// inherited physical binding, including when sink is nil.
func WithRunCommitSink(ctx context.Context, sink RunCommitSink) context.Context {
	return WithRunCommit(ctx, sink, RunCommitBinding{})
}

// RunCommitSinkFromContext returns the sink attached to the current run, if
// any. The result is a capability for committing facts; it does not itself
// authorize tool execution.
func RunCommitSinkFromContext(ctx context.Context) (RunCommitSink, bool) {
	sink, _, ok := RunCommitFromContext(ctx)
	return sink, ok
}

// RunCommitSink is the typed durable checkpoint boundary for one run. A sink
// implementation is bound to one prepared physical session and workspace.
// Core deliberately does not depend on the session storage package.
type RunCommitSink interface {
	CommitInput(context.Context, RunCommitInput) error
	BeginIntentBatch(context.Context, RunIntentBatch) error
	RecordExecution(context.Context, ActionExecution) error
	RecordReceipt(context.Context, RunToolReceipt) error
	ResolveExecution(context.Context, ExecutionResolution) error
	CommitCompaction(context.Context, CompactionProjection) error
	CommitTerminal(context.Context, RunTerminal) error
}

// CompactionSourceSnapshot is the exact immutable history view captured by a
// run's sink. It ties runtime source metadata to the same branch revision the
// sink will later use to validate compaction projections.
type CompactionSourceSnapshot struct {
	Revision       string
	BranchID       string
	Messages       []AgentMessage
	SourceEntryIDs []string
}

// Validate checks the one-to-one message/source mapping required for durable
// history hydration.
func (snapshot CompactionSourceSnapshot) Validate() error {
	if snapshot.BranchID == "" {
		return errors.New("core: compaction source snapshot requires a branch ID")
	}
	if len(snapshot.Messages) != len(snapshot.SourceEntryIDs) {
		return fmt.Errorf("core: compaction source snapshot has %d messages and %d source IDs", len(snapshot.Messages), len(snapshot.SourceEntryIDs))
	}
	if len(snapshot.Messages) > 0 && snapshot.Revision == "" {
		return errors.New("core: non-empty compaction source snapshot requires a revision")
	}
	seen := make(map[string]struct{}, len(snapshot.SourceEntryIDs))
	for index, id := range snapshot.SourceEntryIDs {
		if id == "" {
			return fmt.Errorf("core: compaction source snapshot has an empty entry ID at index %d", index)
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("core: compaction source snapshot repeats entry ID %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// Clone returns a defensive copy of the exact source view.
func (snapshot CompactionSourceSnapshot) Clone() CompactionSourceSnapshot {
	snapshot.Messages = CloneAgentMessages(snapshot.Messages)
	if snapshot.SourceEntryIDs != nil {
		snapshot.SourceEntryIDs = append([]string(nil), snapshot.SourceEntryIDs...)
	}
	return snapshot
}

// RunCommitCompactionSource is an optional capability on a run sink that
// exposes the same captured view used for later durable source mapping.
type RunCommitCompactionSource interface {
	CompactionSourceSnapshot() (CompactionSourceSnapshot, error)
}

// RunCommitRunLease is an optional capability implemented by sinks that borrow
// a longer-lived prepared session. CloseRunCommit must only release this run's
// idempotent borrow; it must not perform durable writes or close the underlying
// session. Session managers own and report errors from physical session close.
// The run owner calls it once, after the callback, workers, and completion
// resources have joined, including when durable commit failed.
type RunCommitRunLease interface {
	CloseRunCommit()
}

// runCommitLeaseGuard captures the optional borrowed-session release capability
// before an entrypoint can reject a run. Ownership may move from that
// entrypoint to runOwner, but the underlying capability is invoked at most once.
type runCommitLeaseGuard struct {
	lease    RunCommitRunLease
	once     sync.Once
	panicked bool
}

func newRunCommitLeaseGuard(ctx context.Context) *runCommitLeaseGuard {
	sink, _, ok := RunCommitFromContext(ctx)
	if !ok {
		return nil
	}
	return newRunCommitLeaseGuardForSink(sink)
}

func newRunCommitLeaseGuardForSink(sink RunCommitSink) *runCommitLeaseGuard {
	if sink == nil {
		return nil
	}
	lease, ok := sink.(RunCommitRunLease)
	if !ok {
		return nil
	}
	return &runCommitLeaseGuard{lease: lease}
}

// captureRunCommitLeaseGuard defers a panic raised by a caller-supplied
// context.Value until the entrypoint knows admission succeeded. Rejected
// admission keeps precedence over context reads, while a successfully claimed
// run replays the panic after ordinary wrapper cleanup has been installed.
func captureRunCommitLeaseGuard(ctx context.Context) (guard *runCommitLeaseGuard, panicValue any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicValue = recovered
		}
	}()
	return newRunCommitLeaseGuard(ctx), nil
}

func (guard *runCommitLeaseGuard) close() bool {
	if guard == nil || guard.lease == nil {
		return false
	}
	guard.once.Do(func() {
		guard.panicked = closeRunCommitLease(guard.lease)
	})
	return guard.panicked
}

// RunCommitInput is an input message at its canonical per-run transcript
// position.
type RunCommitInput struct {
	Generation      *RunGenerationMetadata `json:"generation,omitempty"`
	RunID           string                 `json:"run_id"`
	TranscriptIndex uint64                 `json:"transcript_index"`
	Message         AgentMessage           `json:"message"`
	InputKind       string                 `json:"input_kind"`
}

// RunIntentBatch is one sequential reservation or parallel assistant call
// group. KnownReceipts are pre-dispatch denials with no ActionID. Each intent
// must carry this batch's RunID and ActionBatchID.
type RunIntentBatch struct {
	RunID                      string           `json:"run_id"`
	ActionBatchID              string           `json:"action_batch_id"`
	AssistantTranscriptOrdinal uint64           `json:"assistant_transcript_ordinal"`
	AssistantMessage           AgentMessage     `json:"assistant_message"`
	Intents                    []ActionIntent   `json:"intents,omitempty"`
	KnownReceipts              []RunToolReceipt `json:"known_receipts,omitempty"`
	CanonicalWorkspaceRoot     string           `json:"-"`
}

// ActionIntent reserves one registered invocation without storing its raw
// arguments. The sink validates SessionID and WorkspaceID against its bound
// prepared session/workspace before committing this value.
type ActionIntent struct {
	SessionID                  string `json:"session_id"`
	WorkspaceID                string `json:"workspace_id"`
	RunID                      string `json:"run_id"`
	ActionBatchID              string `json:"action_batch_id"`
	OwnerGeneration            string `json:"owner_generation"`
	AssistantTranscriptOrdinal uint64 `json:"assistant_transcript_ordinal"`
	ToolCallID                 string `json:"tool_call_id"`
	ActionID                   string `json:"action_id"`
	// ToolIdentity is runtime-only trust. A durable sink must persist Snapshot()
	// as audit facts and must never reconstruct runtime authority from that DTO.
	ToolIdentity   tools.TrustedToolIdentity `json:"-"`
	SchemaDigest   [32]byte                  `json:"schema_digest"`
	ArgumentDigest [32]byte                  `json:"argument_digest"`
	// EffectSet is runtime-only trust. A durable sink may persist Values() as
	// facts, but must not hydrate those labels into an authorization capability.
	EffectSet              tools.TrustedEffectSet          `json:"-"`
	ScopedReadProof        *tools.ScopedWorkspaceReadProof `json:"-"`
	ManagedDelegationProof *tools.ManagedDelegationProof   `json:"-"`
}

// ActionExecution records the dispatch fence and local resource observation
// for one reserved invocation. ResourceState and ExecutionIdentity are
// observations only. CompletionProof is a closed positive signal that may be
// referenced by a later ExecutionResolution; it does not itself clear a
// workspace marker.
type ActionExecution struct {
	RunID                      string                        `json:"run_id"`
	AssistantTranscriptOrdinal uint64                        `json:"assistant_transcript_ordinal"`
	ToolCallID                 string                        `json:"tool_call_id"`
	ActionID                   string                        `json:"action_id"`
	OwnerGeneration            string                        `json:"owner_generation"`
	DispatchState              DispatchState                 `json:"dispatch_state"`
	ResourceState              RunExecutionState             `json:"resource_state"`
	ExecutionIdentity          *ExecutionIdentity            `json:"execution_identity,omitempty"`
	CompletionProof            tools.PreparedCompletionProof `json:"completion_proof,omitempty"`
}

// RunToolReceipt is the canonical terminal result for one tool-call
// occurrence. ActionID is empty only for a call rejected before reservation.
type RunToolReceipt struct {
	RunID                      string        `json:"run_id"`
	AssistantTranscriptOrdinal uint64        `json:"assistant_transcript_ordinal"`
	ToolCallID                 string        `json:"tool_call_id"`
	ActionID                   string        `json:"action_id,omitempty"`
	TranscriptIndex            uint64        `json:"transcript_index"`
	DispatchState              DispatchState `json:"dispatch_state"`
	Message                    AgentMessage  `json:"message"`
	ReceiptOutcome             string        `json:"receipt_outcome"`
}

// CompactionCursor retains coordinates in the run-start prior-history and
// canonical-transcript space.
type CompactionCursor struct {
	PriorHistoryCount uint64 `json:"prior_history_count"`
	TranscriptCount   uint64 `json:"transcript_count"`
}

// CompactionProjection records the exact prompt-history prefix replaced by a
// summary during a run.
type CompactionProjection struct {
	ExpectedHistoryRevision string           `json:"expected_history_revision"`
	Through                 CompactionCursor `json:"through"`
	Summary                 string           `json:"summary"`
}

// TerminalCheckpointID is generated before terminal commit and retained
// across retries so the terminal checkpoint has stable idempotency identity.
type TerminalCheckpointID string

// RunTerminal is the final immutable per-run transcript and its durable
// terminal checkpoint request.
type RunTerminal struct {
	Generation           *RunGenerationMetadata `json:"generation,omitempty"`
	RunID                string                 `json:"run_id"`
	Transcript           []TranscriptEntry      `json:"transcript"`
	Projections          []CompactionProjection `json:"projections,omitempty"`
	Outcome              RunOutcome             `json:"outcome"`
	TerminalCheckpointID TerminalCheckpointID   `json:"terminal_checkpoint_id"`
}
