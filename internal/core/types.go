// Package core defines the core abstraction layer for the go-cli runtime.
// It holds the smallest interfaces and data types (AgentLoop, Agent, Harness,
// EventStream, TurnRunner, and the value types they exchange) that both the
// service layer and the extension layer build upon. Per the layered design,
// core has zero downward dependencies on any service or extension package.
package core

import "time"

// AgentMessage is a single message exchanged within a conversation.
type AgentMessage struct {
	// Role identifies the speaker ("user", "assistant", "system", "tool").
	Role string
	// Content is the textual payload of the message.
	Content string
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
