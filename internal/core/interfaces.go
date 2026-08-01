package core

import (
	"context"
	"log/slog"
)

// SubmissionType distinguishes the different kinds of Submission the runtime
// can receive.
type SubmissionType int

const (
	// SubmissionUserMessage is a normal user-authored message.
	SubmissionUserMessage SubmissionType = iota
	// SubmissionSteering injects a new steering instruction.
	SubmissionSteering
	// SubmissionFollowUp appends a follow-up message to a running turn.
	SubmissionFollowUp
)

// String returns the textual name of the submission type and logs it.
func (t SubmissionType) String() string {
	slog.Info("core.submission.type", "type", int(t))
	switch t {
	case SubmissionSteering:
		return "steering"
	case SubmissionFollowUp:
		return "followup"
	default:
		return "user"
	}
}

// DiscardPolicy controls what happens when a bounded EventStream buffer is
// full.
type DiscardPolicy int

const (
	// DiscardOldest drops the oldest buffered event (default).
	DiscardOldest DiscardPolicy = iota
	// DiscardNewest drops the newest event.
	DiscardNewest
	// BlockUntilConsumed blocks the sender until a consumer reads.
	BlockUntilConsumed
)

// String returns the textual name of the discard policy.
func (p DiscardPolicy) String() string {
	switch p {
	case DiscardNewest:
		return "discard_newest"
	case BlockUntilConsumed:
		return "block"
	default:
		return "discard_oldest"
	}
}

// Classification is the result of approval classification for a proposed tool
// call.
type Classification int

const (
	// ClassificationAllow permits the call without human approval.
	ClassificationAllow Classification = iota
	// ClassificationDeny rejects the call outright.
	ClassificationDeny
	// ClassificationRequireApproval asks a human to review the call.
	ClassificationRequireApproval
)

// String returns the textual name of the classification and logs it.
func (c Classification) String() string {
	slog.Info("core.approval.classification", "classification", int(c))
	switch c {
	case ClassificationDeny:
		return "deny"
	case ClassificationRequireApproval:
		return "require_approval"
	default:
		return "allow"
	}
}

// AgentLoop is the pure ReAct loop. It is stateless: given a Submission it
// executes the think -> act -> observe cycle and returns the events it fired.
type AgentLoop interface {
	// Run executes one iteration of the agent loop for the submission and
	// returns the events emitted during execution.
	Run(ctx context.Context, submission Submission) ([]AgentEvent, error)
}

// Agent is a stateful wrapper around the agent loop, exposing a named,
// addressable entry point for the runtime.
type Agent interface {
	// Name returns the agent identifier.
	Name() string
	// Run executes a submission through the agent and returns a result.
	Run(ctx context.Context, submission Submission) (Result, error)
}

// Harness is the full runtime facade. It accepts user input and returns an
// EventStream that streams progress until the run completes.
type Harness interface {
	// Submit submits a user message and returns a stream of agent events.
	Submit(ctx context.Context, msg string) (EventStream, error)
}

// TurnRunner manages the lifecycle of a single turn execution.
type TurnRunner interface {
	// RunTurn executes one turn from a submission and returns a result.
	RunTurn(ctx context.Context, submission Submission) (Result, error)
}

// SessionStore persists and loads conversational sessions.
type SessionStore interface {
	// Save persists a session.
	Save(ctx context.Context, session Session) error
	// Load retrieves the session with the given id.
	Load(ctx context.Context, id string) (Session, error)
	// List returns all persisted sessions.
	List(ctx context.Context) ([]Session, error)
}

// SessionTree manages the branching structure of sessions (append-only tree).
type SessionTree interface {
	// Create registers a new root session.
	Create(ctx context.Context, sessionID string) error
	// Branch creates a child session branched off parentID.
	Branch(ctx context.Context, parentID, sessionID string) error
	// Leaves returns the leaf session ids reachable from rootID.
	Leaves(ctx context.Context, rootID string) ([]string, error)
}

// ContextManager rebuilds the effective context (message list) for a session,
// replaying history and compaction summaries as needed.
type ContextManager interface {
	// Build returns the reconstructed message list for the session.
	Build(ctx context.Context, sessionID string) ([]AgentMessage, error)
}

// Compactor reduces a message list to fit within a token budget.
type Compactor interface {
	// Compact trims/rewrites messages so they stay below maxTokens.
	Compact(ctx context.Context, messages []AgentMessage, maxTokens int) ([]AgentMessage, error)
}

// TokenEstimator estimates the token count of text and message lists.
type TokenEstimator interface {
	// Estimate estimates the token count of a single text.
	Estimate(text string) int
	// EstimateMessages estimates the total token count of a message list.
	EstimateMessages(messages []AgentMessage) int
}

// ApprovalClassifier decides whether a proposed tool call is allowed, denied,
// or requires human approval.
type ApprovalClassifier interface {
	// Name returns the classifier identifier.
	Name() string
	// Classify returns the classification for a tool call by name.
	Classify(ctx context.Context, toolName string) Classification
}

// ApprovalStore records and queries approval decisions for tool calls.
type ApprovalStore interface {
	// Remember stores an approval decision for a tool.
	Remember(ctx context.Context, toolName string, allowed bool) error
	// IsAllowed reports whether the tool was previously approved.
	IsAllowed(ctx context.Context, toolName string) bool
}

// PluginLoader loads external extensions from a compiled artifact or remote
// endpoint.
type PluginLoader interface {
	// Load loads the extension at the given path.
	Load(ctx context.Context, path string) (Extension, error)
}
