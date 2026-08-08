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
	Kind string
	// Content carries the event payload.
	Content string
	// Timestamp records when the event was produced.
	Timestamp time.Time
	// Incremental marks a partial "message" event that contains only a
	// fragment of the assistant's response (one or more tokens from a
	// streaming LLM). The TUI accumulates these into the full message.
	Incremental bool
	// TokenUsage carries token consumption data for "token_usage" events.
	// It is nil for all other event kinds.
	TokenUsage *TokenUsage
	// ToolCallID associates streaming output with the originating tool
	// call. Populated for "tool_output" events; empty for other kinds.
	ToolCallID string
	// Stream identifies the output source for "tool_output" events:
	// "stdout" or "stderr". Empty for other event kinds.
	Stream string
	// Usage carries the API-reported token consumption for "message"
	// events. It is nil for all other event kinds and for "message"
	// events when the provider did not return usage data.
	Usage *llm.Usage
	// ToolCalls carries the tool invocations requested by the assistant
	// for "message" events. It is nil for events with no tool calls.
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
}

// TokenUsage carries token consumption and cost data for a token_usage event.
type TokenUsage struct {
	// InputTokens is the total prompt tokens consumed so far.
	InputTokens int
	// OutputTokens is the total completion tokens consumed so far.
	OutputTokens int
	// MaxTokens is the token budget for the session.
	MaxTokens int
	// Cost is the accumulated monetary cost in USD.
	Cost float64
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
