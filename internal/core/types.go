// Package core defines the core abstraction layer for the go-cli runtime.
// It holds the smallest interfaces and data types (AgentLoop, Agent, Harness,
// EventStream, TurnRunner, and the value types they exchange) that both the
// service layer and the extension layer build upon. Per the layered design,
// core has zero downward dependencies on any service or extension package.
package core

import (
	"time"

	"github.com/pengjunchen/go-cli/internal/llm"
)

// Event kind constants used by AgentEvent.Kind.
const (
	// EventKindPreToolCall marks an event emitted before each tool execution,
	// allowing external interceptors to cancel the call via PreToolCall.Cancel.
	EventKindPreToolCall = "pre_tool_call"
	// EventKindAssistantGenerationStarted marks the start of a guarded assistant
	// response operation. It carries occurrence coordinates but no candidate content.
	EventKindAssistantGenerationStarted = "assistant_generation_started"
	// EventKindAssistantGenerationFinished closes the matching guarded assistant
	// response operation, regardless of whether it succeeded, failed, or canceled.
	EventKindAssistantGenerationFinished = "assistant_generation_finished"
)

// AgentMessage is a single message exchanged within a conversation.
type AgentMessage struct {
	// Role identifies the speaker ("user", "assistant", "system", "tool").
	Role string
	// Content is the textual payload of the message.
	Content string
	// ContentBlocks holds typed content parts for multimodal messages
	// (text + images). When non-nil, it takes precedence over Content.
	ContentBlocks []llm.ContentBlock
	// Usage reports the token consumption for this message as reported by
	// the API. It is nil for user/system/tool messages and for assistant
	// messages when the provider did not return usage data.
	Usage *llm.Usage
	// ToolCalls holds tool invocations requested by the assistant. Populated
	// for assistant messages that request tool execution.
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID associates a tool-result message with the originating tool
	// call. Populated for "tool" role messages.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ToolName is the name of the tool that produced this message. Populated
	// for "tool" role messages.
	ToolName string `json:"tool_name,omitempty"`
	// MaterializedArtifactDigest audits on-demand instruction bytes, not authorization.
	MaterializedArtifactDigest string `json:"materialized_artifact_digest,omitempty"`
	// InputKind preserves whether a user-facing message came from an initial
	// submission, steering input, or follow-up input.
	InputKind string `json:"input_kind,omitempty"`
	// ReceiptOutcome records the terminal truth for a tool receipt.
	ReceiptOutcome string `json:"receipt_outcome,omitempty"`
	// Runtime carries per-item source coordinates through runtime-only
	// conversions. It is not persisted in session rows or sent to providers.
	Runtime llm.RuntimeMetadata `json:"-"`
}

// String returns a compact one-line representation of the message.
func (m AgentMessage) String() string {
	return m.Role + ": " + m.Content
}

// AgentTool describes a tool available to an agent during a loop.
type AgentTool struct {
	// Name is the unique tool identifier.
	Name string
	// Description is a human-readable summary of what the tool does.
	Description string
}

// AgentEvent is a discrete event emitted by an agent while it is running.
// Consumers (e.g. the TUI or a test harness) stream these from an EventStream.
type AgentEvent struct {
	// Kind classifies the event (e.g. "message", "tool", "status").
	Kind string `json:"kind"`
	// Content carries the event payload.
	Content string `json:"content"`
	// Timestamp records when the event was produced.
	Timestamp time.Time `json:"timestamp"`
	// Incremental marks a partial "message" event that contains only a
	// fragment of the assistant's response (one or more tokens from a
	// streaming LLM). The TUI accumulates these into the full message.
	Incremental bool `json:"incremental,omitempty"`
	// TokenUsage carries token consumption data for "token_usage" events.
	// It is nil for all other event kinds.
	TokenUsage *TokenUsage `json:"token_usage,omitempty"`
	// ToolCallID associates streaming output with the originating tool
	// call. Populated for "tool_output" events; empty for other kinds.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Stream identifies the output source for "tool_output" events:
	// "stdout" or "stderr". Empty for other event kinds.
	Stream string `json:"stream,omitempty"`
	// Usage carries the API-reported token consumption for "message"
	// events. It is nil for all other event kinds and for "message"
	// events when the provider did not return usage data.
	Usage *llm.Usage `json:"usage,omitempty"`
	// ToolCalls carries the tool invocations requested by the assistant
	// for "message" events. It is nil for events with no tool calls.
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
	// IsError marks a "tool_result" event as an error for structured
	// detection. When true, consumers use this boolean instead of string
	// matching on Content to determine whether the tool failed.
	IsError bool `json:"is_error,omitempty"`
	// RunID identifies the owner that produced this event.
	RunID string `json:"run_id,omitempty"`
	// CanonicalTranscriptIndex identifies the assistant row in the durable run transcript.
	CanonicalTranscriptIndex *uint64 `json:"canonical_transcript_index,omitempty"`
	// AssistantTranscriptOrdinal identifies the tool-call assistant occurrence.
	AssistantTranscriptOrdinal *uint64 `json:"assistant_transcript_ordinal,omitempty"`
	// ReceiptOutcome carries the exact typed tool receipt outcome when present.
	ReceiptOutcome string `json:"receipt_outcome,omitempty"`
	// SourceTruncated reports a source truncation fact when structured metadata provides one.
	SourceTruncated *bool `json:"source_truncated,omitempty"`
	// PreToolCall carries a cancelable handle for "pre_tool_call" events.
	// External interceptors call PreToolCall.Cancel() to prevent the
	// tool from executing. It is nil for all other event kinds.
	PreToolCall *PreToolCallEvent `json:"pre_tool_call,omitempty"`
}

func cloneAgentEvent(event AgentEvent) AgentEvent {
	event.Usage = cloneUsage(event.Usage)
	if event.ToolCalls != nil {
		calls := make([]llm.ToolCall, len(event.ToolCalls))
		copy(calls, event.ToolCalls)
		for i := range calls {
			calls[i].Args = cloneProtocolJSONValue(calls[i].Args)
		}
		event.ToolCalls = calls
	}
	event.PreToolCall = clonePreToolCallEvent(event.PreToolCall)
	if event.CanonicalTranscriptIndex != nil {
		index := *event.CanonicalTranscriptIndex
		event.CanonicalTranscriptIndex = &index
	}
	if event.AssistantTranscriptOrdinal != nil {
		ordinal := *event.AssistantTranscriptOrdinal
		event.AssistantTranscriptOrdinal = &ordinal
	}
	if event.SourceTruncated != nil {
		truncated := *event.SourceTruncated
		event.SourceTruncated = &truncated
	}
	if event.TokenUsage != nil {
		usage := *event.TokenUsage
		usage.ProviderModels = append([]TokenUsageProviderModel(nil), event.TokenUsage.ProviderModels...)
		usage.CostSources = append([]TokenUsageCostSource(nil), event.TokenUsage.CostSources...)
		usage.PriceSources = append([]TokenUsagePriceSource(nil), event.TokenUsage.PriceSources...)
		event.TokenUsage = &usage
	}
	return event
}

// TokenUsageSource identifies whether the displayed token counts came from a
// provider response, local estimate, or no usable evidence.
type TokenUsageSource string

const (
	TokenUsageSourceReported  TokenUsageSource = "reported"
	TokenUsageSourceEstimated TokenUsageSource = "estimated"
	TokenUsageSourceUnknown   TokenUsageSource = "unknown"
)

// TokenUsageCostSource describes the evidence behind accumulated cost.
type TokenUsageCostSource string

const (
	TokenUsageCostReported  TokenUsageCostSource = "reported"
	TokenUsageCostEstimated TokenUsageCostSource = "estimated"
	TokenUsageCostUnknown   TokenUsageCostSource = "unknown"
)

// TokenUsagePriceSource names a safe pricing source without exposing price
// configuration contents or provider endpoint data.
type TokenUsagePriceSource string

const (
	TokenUsagePriceProvider       TokenUsagePriceSource = "provider"
	TokenUsagePriceDefaultTier    TokenUsagePriceSource = "default_tier"
	TokenUsagePriceConfiguredTier TokenUsagePriceSource = "configured_tier"
	TokenUsagePriceUnknown        TokenUsagePriceSource = "unknown"
)

// TokenUsageProviderModel identifies a provider/model pair that contributed to
// the usage summary. Endpoint and credential data are intentionally omitted.
type TokenUsageProviderModel struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}

// TokenUsage carries token consumption and cost data for a token_usage event.
type TokenUsage struct {
	// InputTokens is the total prompt tokens consumed so far.
	InputTokens int `json:"input_tokens"`
	// OutputTokens is the total completion tokens consumed so far.
	OutputTokens int `json:"output_tokens"`
	// MaxTokens is the token budget for the session.
	MaxTokens int `json:"max_tokens,omitempty"`
	// Cost is the accumulated monetary cost in USD.
	Cost float64 `json:"cost,omitempty"`
	// UnknownUsageAttempts counts provider attempts without authoritative token counts.
	UnknownUsageAttempts int `json:"unknown_usage_attempts,omitempty"`
	// UnknownCostAttempts counts provider attempts without an authoritative or estimated cost.
	UnknownCostAttempts int `json:"unknown_cost_attempts,omitempty"`
	// UsageSource identifies the evidence behind the displayed token counts.
	UsageSource TokenUsageSource `json:"usage_source,omitempty"`
	// CostSources lists the distinct evidence classes contributing to Cost.
	CostSources []TokenUsageCostSource `json:"cost_sources,omitempty"`
	// PriceSources lists the distinct safe sources used to price provider attempts.
	PriceSources []TokenUsagePriceSource `json:"price_sources,omitempty"`
	// ProviderModels lists actual resolved provider/model pairs in the attempt ledger.
	ProviderModels []TokenUsageProviderModel `json:"provider_models,omitempty"`
}

// DurablePublicationState describes the durable visibility of a run result.
type DurablePublicationState string

const (
	// DurablePublicationUnbound means the run had no durable session binding.
	DurablePublicationUnbound DurablePublicationState = "unbound"
	// DurablePublicationPending means durable publication or owner cleanup is incomplete.
	DurablePublicationPending DurablePublicationState = "pending"
	// DurablePublicationSynced means the owner's terminal commit and cleanup were acknowledged.
	DurablePublicationSynced DurablePublicationState = "synced"
	// DurablePublicationFailed means a bound run finished without positive durable publication evidence.
	DurablePublicationFailed DurablePublicationState = "failed"
)

// DurablePublication contains owner-derived evidence for one run's durable transcript.
type DurablePublication struct {
	State           DurablePublicationState
	RunID           string
	SessionID       string
	OwnerGeneration string
}

// String returns a time-stamped representation of the event and logs it.
func (e AgentEvent) String() string {
	return e.Timestamp.Format(time.RFC3339Nano) + " [" + e.Kind + "] " + e.Content
}

// Result is the typed outcome of running an agent or a turn.
type Result struct {
	// Message is the final response text produced by the run.
	Message string
	// Success reports whether the run completed without a fatal error.
	Success bool
	// RunID identifies the owned execution that produced this result.
	RunID string
	// Outcome records the logical outcome independently from resource cleanup.
	Outcome RunOutcome
	// ExecutionState records whether the run is still executing or has quiesced.
	ExecutionState RunExecutionState
	// StatusHandle provides a fresh immutable status snapshot while cleanup drains.
	StatusHandle RunStatusHandle
	// DurablePublication is the owner-derived publication state for this result.
	DurablePublication DurablePublication
}

// String returns a short summary of the result and logs it.
func (r Result) String() string {
	return r.Message
}

// Submission is the request handed to an agent, turn runner or loop when a
// user wants the runtime to act on something.
type Submission struct {
	// Type distinguishes a normal user message from enhanced inputs.
	Type SubmissionType
	// Content is the textual request.
	Content string
	// Metadata carries optional structured context for the submission.
	Metadata map[string]any
	// History carries the prior conversation messages so the loop can send
	// the full context to the LLM. Populated by AgentImpl.Run from its
	// internal history before invoking the loop.
	History []AgentMessage
}

// Session is a minimal unit of persisted conversational state referenced by
// SessionStore / SessionTree. Later phases flesh out its internal shape.
type Session struct {
	// ID uniquely identifies the session.
	ID string
	// Messages is the accumulated message history of the session.
	Messages []AgentMessage
	// CreatedAt records when the session began.
	CreatedAt time.Time
}
