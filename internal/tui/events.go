package tui

import "github.com/pengjunchen/go-cli/internal/core"

// Msg is the generic message type delivered to an App via Send. It corresponds
// to tea.Msg in the bubbletea-based design: any value pushed through Send is
// forwarded to the model's Update as a tea.Msg.
type Msg any

// AgentEvent is a discrete event consumed by the App from an event stream. It
// deliberately mirrors the shape required by the TUI spec, carrying a content
// type for renderer dispatch plus trace/span identifiers used for debug-time
// correlation (the TUI layer does not emit spans itself).
type AgentEvent struct {
	// Type classifies the event at the actor level (e.g. "run", "tool").
	Type string
	// Content is the event payload to render.
	Content string
	// ContentType selects the renderer (see the ContentType* constants).
	ContentType string
	// TraceID associates the event with a trace for debug logging.
	TraceID string
	// SpanID associates the event with a span for debug logging.
	SpanID string
	// Incremental marks a partial event that contains only a fragment of
	// the assistant's response (streaming tokens). The TUI accumulates
	// these into the last assistant entry instead of creating new entries.
	Incremental bool
	// TokenUsage carries token consumption data for "token_usage" events.
	// It is nil for all other event types.
	TokenUsage *TokenUsageData
	// ToolCallID is the source-provided tool identifier on tool lifecycle and
	// output events. The live view associates it only within the active group.
	ToolCallID string
	// Stream identifies the output source for tool_output events: "stdout"
	// or "stderr".
	Stream string
	// IsError preserves the structured error marker from tool events. On a
	// uniquely associated tool_result, it sets the call status to Error.
	IsError bool
	// RunID and transcript coordinates identify the producer-owned occurrence.
	RunID                      string
	CanonicalTranscriptIndex   *uint64
	AssistantTranscriptOrdinal *uint64
	// ReceiptOutcome carries the exact typed tool receipt outcome when present.
	ReceiptOutcome string
	// SourceTruncated is copied only from structured source metadata.
	SourceTruncated *bool
	// ToolCalls carries the assistant-proposed tool calls attached to a
	// message event. These are provisional until a matching tool_call event
	// confirms execution has started.
	ToolCalls []ToolCallData
}

// ToolCallData is the presentation-safe subset of an assistant tool call.
// ArgsUnavailable is set when the source arguments are not a valid JSON value
// or cannot be copied safely; callers should render that state explicitly.
type ToolCallData struct {
	ID              string
	Name            string
	Args            any
	ArgsUnavailable bool
}

// TokenUsageData mirrors core.TokenUsage for the TUI layer.
type TokenUsageData struct {
	InputTokens          int
	OutputTokens         int
	MaxTokens            int
	Cost                 float64
	UnknownUsageAttempts int
	UnknownCostAttempts  int
	UsageSource          core.TokenUsageSource
	CostSources          []core.TokenUsageCostSource
	PriceSources         []core.TokenUsagePriceSource
	ProviderModels       []core.TokenUsageProviderModel
}
