package core

import (
	"context"
	"errors"
)

// ErrCanonicalMiddlewareUnsupported is returned before loop execution when an
// AgentLoop supports canonical transcripts but a middleware in its chain
// cannot preserve them. The rejection itself still commits the current user
// input as a valid, side-effect-free per-run delta.
var ErrCanonicalMiddlewareUnsupported = errors.New("core: canonical transcript unsupported by middleware chain")

// InputKind values identify where an input recorded in a canonical transcript
// came from. AgentMessage stores these as strings for JSON compatibility.
const (
	// InputKindUser marks the initial user-authored submission.
	InputKindUser = "user"
	// InputKindSteering marks an instruction injected into a running turn.
	InputKindSteering = "steering"
	// InputKindFollowUp marks a follow-up input appended to a running turn.
	InputKindFollowUp = "follow_up"
)

// ReceiptOutcome values record the terminal truth for a tool receipt in a
// canonical transcript. AgentMessage stores these as strings for JSON
// compatibility. `success` means the tool succeeded; `error` means it returned
// a known failure; `interrupted` means it is known to have stopped without a
// side effect; `unknown` means it may have acted but its result cannot be
// proved.
const (
	// ReceiptOutcomeSuccess marks a tool that completed successfully.
	ReceiptOutcomeSuccess = "success"
	// ReceiptOutcomeError marks a tool that returned a known failure.
	ReceiptOutcomeError = "error"
	// ReceiptOutcomeInterrupted marks a known interruption with no side effect.
	ReceiptOutcomeInterrupted = "interrupted"
	// ReceiptOutcomeUnknown marks a tool whose terminal outcome cannot be proven.
	ReceiptOutcomeUnknown = "unknown"
)

// CanonicalRunResult contains the event projection and typed transcript from
// one loop invocation. Its slices belong to that invocation: after returning,
// the loop must not mutate them, and consumers must treat them as immutable.
type CanonicalRunResult struct {
	Events     []AgentEvent
	Transcript []TranscriptEntry
}

// TranscriptEntry wraps a single typed message in the canonical run history.
type TranscriptEntry struct {
	Message AgentMessage
}

// CanonicalLoop is an optional extension to AgentLoop for implementations that
// can return a complete typed transcript alongside the lossy event projection.
// Implementations must return fresh per-call data and must not mutate its
// backing slices after RunCanonical returns.
type CanonicalLoop interface {
	RunCanonical(ctx context.Context, submission Submission, stream ...EventStream) (CanonicalRunResult, error)
}
