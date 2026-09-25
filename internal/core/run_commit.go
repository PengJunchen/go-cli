package core

import (
	"context"

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

type runCommitSinkContextKey struct{}

type runCommitSinkContextValue struct {
	sink RunCommitSink
}

// WithRunCommitSink attaches the durable commit sink for one run. Passing nil
// explicitly masks any sink inherited from a parent context.
func WithRunCommitSink(ctx context.Context, sink RunCommitSink) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, runCommitSinkContextKey{}, runCommitSinkContextValue{sink: sink})
}

// RunCommitSinkFromContext returns the sink attached to the current run, if
// any. The result is a capability for committing facts; it does not itself
// authorize tool execution.
func RunCommitSinkFromContext(ctx context.Context) (RunCommitSink, bool) {
	if ctx == nil {
		return nil, false
	}
	value, ok := ctx.Value(runCommitSinkContextKey{}).(runCommitSinkContextValue)
	if !ok || value.sink == nil {
		return nil, false
	}
	return value.sink, true
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

// RunCommitInput is an input message at its canonical per-run transcript
// position.
type RunCommitInput struct {
	RunID           string       `json:"run_id"`
	TranscriptIndex uint64       `json:"transcript_index"`
	Message         AgentMessage `json:"message"`
	InputKind       string       `json:"input_kind"`
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
	EffectSet tools.TrustedEffectSet `json:"-"`
}

// ActionExecution records the dispatch fence and local resource observation
// for one reserved invocation. ResourceState is an observation only; it is
// not durable proof that clears a workspace marker. Only ExecutionResolution
// carries that evidence.
type ActionExecution struct {
	RunID                      string             `json:"run_id"`
	AssistantTranscriptOrdinal uint64             `json:"assistant_transcript_ordinal"`
	ToolCallID                 string             `json:"tool_call_id"`
	ActionID                   string             `json:"action_id"`
	OwnerGeneration            string             `json:"owner_generation"`
	DispatchState              DispatchState      `json:"dispatch_state"`
	ResourceState              RunExecutionState  `json:"resource_state"`
	ExecutionIdentity          *ExecutionIdentity `json:"execution_identity,omitempty"`
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
	RunID                string                 `json:"run_id"`
	Transcript           []TranscriptEntry      `json:"transcript"`
	Projections          []CompactionProjection `json:"projections,omitempty"`
	Outcome              RunOutcome             `json:"outcome"`
	TerminalCheckpointID TerminalCheckpointID   `json:"terminal_checkpoint_id"`
}
