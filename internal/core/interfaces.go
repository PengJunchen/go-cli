package core

import (
	"context"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
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
	Run(ctx context.Context, submission Submission, stream ...EventStream) ([]AgentEvent, error)
}

// Agent is a stateful wrapper around the agent loop, exposing a named,
// addressable entry point for the runtime.
type Agent interface {
	// Name returns the agent identifier.
	Name() string
	// Run executes a submission through the agent and returns a result.
	Run(ctx context.Context, submission Submission, stream ...EventStream) (Result, error)
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

// Registry is the dependency-inversion hub of the runtime. It exposes one
// getter and one RegisterXxx method per subsystem. Consumers depend on this
// interface rather than on any concrete implementation, so the bound
// subsystem can be swapped without coupling callers to a concrete type.
type Registry interface {
	// AgentLoop returns the bound AgentLoop implementation.
	AgentLoop() AgentLoop
	// RegisterAgentLoop replaces the AgentLoop implementation and returns the
	// old one. It panics if n is nil.
	RegisterAgentLoop(n AgentLoop) AgentLoop

	// Agent returns the bound Agent implementation.
	Agent() Agent
	// RegisterAgent replaces the Agent implementation and returns the old one.
	// It panics if n is nil.
	RegisterAgent(n Agent) Agent

	// Harness returns the bound Harness implementation.
	Harness() Harness
	// RegisterHarness replaces the Harness implementation and returns the old
	// one. It panics if n is nil.
	RegisterHarness(n Harness) Harness

	// TurnRunner returns the bound TurnRunner implementation.
	TurnRunner() TurnRunner
	// RegisterTurnRunner replaces the TurnRunner implementation and returns
	// the old one. It panics if n is nil.
	RegisterTurnRunner(n TurnRunner) TurnRunner

	// SessionStore returns the bound SessionStore implementation.
	SessionStore() SessionStore
	// RegisterSessionStore replaces the SessionStore implementation and
	// returns the old one. It panics if n is nil.
	RegisterSessionStore(n SessionStore) SessionStore

	// SessionTree returns the bound SessionTree implementation.
	SessionTree() SessionTree
	// RegisterSessionTree replaces the SessionTree implementation and returns
	// the old one. It panics if n is nil.
	RegisterSessionTree(n SessionTree) SessionTree

	// ContextManager returns the bound ContextManager implementation.
	ContextManager() ContextManager
	// RegisterContextManager replaces the ContextManager implementation and
	// returns the old one. It panics if n is nil.
	RegisterContextManager(n ContextManager) ContextManager

	// Compactor returns the bound Compactor implementation.
	Compactor() Compactor
	// RegisterCompactor replaces the Compactor implementation and returns the
	// old one. It panics if n is nil.
	RegisterCompactor(n Compactor) Compactor

	// TokenEstimator returns the bound TokenEstimator implementation.
	TokenEstimator() TokenEstimator
	// RegisterTokenEstimator replaces the TokenEstimator implementation and
	// returns the old one. It panics if n is nil.
	RegisterTokenEstimator(n TokenEstimator) TokenEstimator

	// ToolRegistry returns the bound ToolRegistry implementation.
	ToolRegistry() tools.ToolRegistry
	// RegisterToolRegistry replaces the ToolRegistry implementation and
	// returns the old one. It panics if n is nil.
	RegisterToolRegistry(n tools.ToolRegistry) tools.ToolRegistry

	// ModelProvider returns the bound ModelProvider implementation.
	ModelProvider() llm.ModelProvider
	// RegisterModelProvider replaces the ModelProvider implementation and
	// returns the old one. It panics if n is nil.
	RegisterModelProvider(n llm.ModelProvider) llm.ModelProvider

	// ApprovalClassifier returns the bound ApprovalClassifier implementation.
	ApprovalClassifier() ApprovalClassifier
	// RegisterApprovalClassifier replaces the ApprovalClassifier
	// implementation and returns the old one. It panics if n is nil.
	RegisterApprovalClassifier(n ApprovalClassifier) ApprovalClassifier

	// ApprovalStore returns the bound ApprovalStore implementation.
	ApprovalStore() ApprovalStore
	// RegisterApprovalStore replaces the ApprovalStore implementation and
	// returns the old one. It panics if n is nil.
	RegisterApprovalStore(n ApprovalStore) ApprovalStore

	// TraceExporter returns the bound TraceExporter implementation.
	TraceExporter() tracing.TraceExporter
	// RegisterTraceExporter replaces the TraceExporter implementation and
	// returns the old one. It panics if n is nil.
	RegisterTraceExporter(n tracing.TraceExporter) tracing.TraceExporter

	// ConfigProvider returns the bound ConfigProvider implementation.
	ConfigProvider() extension.ConfigProvider
	// RegisterConfigProvider replaces the ConfigProvider implementation and
	// returns the old one. It panics if n is nil.
	RegisterConfigProvider(n extension.ConfigProvider) extension.ConfigProvider

	// PluginLoader returns the bound PluginLoader implementation.
	PluginLoader() PluginLoader
	// RegisterPluginLoader replaces the PluginLoader implementation and
	// returns the old one. It panics if n is nil.
	RegisterPluginLoader(n PluginLoader) PluginLoader
}
