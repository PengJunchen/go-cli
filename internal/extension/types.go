package extension

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// This file defines the Phase 4 richer Extension system model: the Extension
// lifecycle contract, the event-hook contract and the hook result/action model.
// These types coexist in this package with the earlier ConfigProvider concept
// (extension.go) but are intentionally kept separate from it.

// Extension is the lifecycle contract a Phase 4 extension satisfies. An
// extension is initialized once against a registry, runs, and is shut down
// when the CLI terminates.
type Extension interface {
	// Name returns the extension identifier.
	Name() string
	// Init is invoked once at startup to let the extension register its
	// building blocks (tools, commands, providers, hooks, middleware) with the
	// given registry.
	Init(ctx context.Context, registry ExtensionRegistry) error
	// Shutdown is invoked when the CLI terminates to release resources.
	Shutdown(ctx context.Context) error
}

// defaultExtension is a pass-through stub that satisfies the Extension
// contract without registering anything or acquiring resources.
type defaultExtension struct {
	name string
}

var _ Extension = (*defaultExtension)(nil)

// Name returns the extension name, defaulting to "default-extension".
func (d *defaultExtension) Name() string {
	if d.name == "" {
		return "default-extension"
	}
	return d.name
}

// Init is a no-op that accepts the registry and logs the event.
func (d *defaultExtension) Init(_ context.Context, _ ExtensionRegistry) error {
	slog.Info("extension.init", "name", d.Name())
	return nil
}

// Shutdown is a no-op that logs the event.
func (d *defaultExtension) Shutdown(_ context.Context) error {
	slog.Info("extension.shutdown", "name", d.Name())
	return nil
}

// HookAction describes the disposition of a hook event after a Hook handles it.
type HookAction string

const (
	// HookActionPass indicates the event was observed and processing should
	// continue with the next hook.
	HookActionPass HookAction = "pass"
	// HookActionBlock indicates processing should stop and the event should be
	// rejected (e.g. a permission denial).
	HookActionBlock HookAction = "block"
	// hookActionTerminate indicates the whole run should stop immediately.
	hookActionTerminate HookAction = "terminate"
	// hookActionReplace indicates the caller should substitute its payload with
	// the HookResult.Replacement value.
	hookActionReplace HookAction = "replace"
)

var (
	// ErrHookPayloadUnsupported reports that a Hook payload contains a value
	// outside the JSON-compatible observer contract.
	ErrHookPayloadUnsupported = errors.New("extension: hook payload is unsupported")
	// ErrHookPayloadLimitExceeded reports that an observer payload exceeded a
	// fixed depth, node-count, or byte-count limit.
	ErrHookPayloadLimitExceeded = errors.New("extension: hook payload limit exceeded")
	// ErrUnsupportedHookAction reports that a Hook returned an action or
	// replacement value unsupported by the observer bridge.
	ErrUnsupportedHookAction = errors.New("extension: hook action is unsupported")
	// ErrHookBlocked reports that a Hook blocked the run.
	ErrHookBlocked = errors.New("extension: hook blocked the run")
)

// HookSubmissionType is the extension-owned representation of a submission
// kind. It intentionally does not alias a core enum.
type HookSubmissionType string

// HookRunOutcome is the extension-owned representation of a run outcome.
type HookRunOutcome string

// HookRunExecutionState is the extension-owned representation of run cleanup
// state.
type HookRunExecutionState string

// HookErrorClass is a stable, message-free error category for observers.
type HookErrorClass string

const (
	HookErrorClassCanceled          HookErrorClass = "canceled"
	HookErrorClassDeadline          HookErrorClass = "deadline"
	HookErrorClassBlocked           HookErrorClass = "hook_blocked"
	HookErrorClassUnsupportedAction HookErrorClass = "unsupported_action"
	HookErrorClassUnsupported       HookErrorClass = "unsupported_payload"
	HookErrorClassUnknown           HookErrorClass = "unknown"
)

// HookRegistration binds a Hook to the extension that successfully
// initialized it. Owner is assigned by the coordinator and cannot be
// overridden by the Hook.
type HookRegistration struct {
	Owner string
	Hook  Hook
}

// ToolRegistration binds a tool to the extension that successfully initialized
// it. Owner is assigned by the coordinator and cannot be overridden by the
// tool's own name.
type ToolRegistration struct {
	Owner string
	Tool  tools.ToolDefinition
}

// HookSubmissionSnapshot is a bounded, extension-owned copy of a submission.
type HookSubmissionSnapshot struct {
	Type     HookSubmissionType
	Content  string
	Metadata map[string]any
	Messages []HookMessageSnapshot
}

// HookMessageSnapshot is the extension-owned representation of one message.
type HookMessageSnapshot struct {
	Role, Content             string
	ContentBlocks             []HookContentBlockSnapshot
	Usage                     *HookUsageSnapshot
	ToolCalls                 []HookToolCallSnapshot
	ToolCallID, ToolName      string
	InputKind, ReceiptOutcome string
}

// HookContentBlockSnapshot is the extension-owned representation of a
// multimodal content block.
type HookContentBlockSnapshot struct {
	Type, Text string
	ImageURL   *HookImageURLSnapshot
}

// HookImageURLSnapshot contains copied image URL data.
type HookImageURLSnapshot struct{ URL, Detail string }

// HookUsageSnapshot contains copied token usage values.
type HookUsageSnapshot struct{ InputTokens, OutputTokens, TotalTokens int }

// HookToolCallSnapshot contains copied tool call identity and JSON arguments.
type HookToolCallSnapshot struct {
	ID, Name string
	Args     any
}

// HookBeforeRunSnapshot is the concrete Data value for agent.before_run.
type HookBeforeRunSnapshot struct{ Submission HookSubmissionSnapshot }

// HookAfterRunSnapshot is the concrete Data value for agent.after_run.
type HookAfterRunSnapshot struct {
	Submission HookSubmissionSnapshot
	Run        HookRunSnapshot
	Error      HookErrorSnapshot
}

// HookRunSnapshot contains copied scalar fields about a completed run.
type HookRunSnapshot struct {
	Message                 string
	Success                 bool
	RunID                   string
	RunIDAvailable          bool
	Outcome                 HookRunOutcome
	OutcomeAvailable        bool
	ExecutionState          HookRunExecutionState
	ExecutionStateAvailable bool
}

// HookErrorSnapshot exposes only presence and a stable category. It never
// carries the underlying error or its text.
type HookErrorSnapshot struct {
	Present bool
	Class   HookErrorClass
}

// HookEvent is the immutable description of an event delivered to a Hook.
type HookEvent struct {
	// Name is the event name (e.g. "agent.before_run").
	Name string
	// Data carries optional structured payload associated with the event.
	Data any
	// Source identifies where the event originated (e.g. an extension name).
	Source string
	// Timestamp records when the event was produced.
	Timestamp time.Time
}

// HookResult is what a Hook returns after handling an event.
type HookResult struct {
	// Action is the disposition requested by the hook.
	Action HookAction
	// Reason describes the outcome, especially for block/terminate.
	Reason string
	// Replacement carries the substituted value when Action is hookActionReplace.
	Replacement any
}

// Hook is an event hook that observes agent events and may influence their
// processing through the returned HookResult.
type Hook interface {
	// Name returns the hook identifier.
	Name() string
	// Handle processes the given event and returns a HookResult.
	Handle(ctx context.Context, event HookEvent) HookResult
}

// defaultHook is a pass-through hook that always returns HookActionPass.
type defaultHook struct {
	name string
}

var _ Hook = (*defaultHook)(nil)

// Name returns the hook name, defaulting to "default-hook".
func (h *defaultHook) Name() string {
	return h.name
}

// Handle logs the event and returns HookActionPass.
func (h *defaultHook) Handle(_ context.Context, event HookEvent) HookResult {
	slog.Info("extension.hook", "name", h.Name(), "event", event.Name)
	return HookResult{Action: HookActionPass}
}
