// Package mock provides a mock-first server framework for multi-turn TDD.
// It defines the LLM / tool / config / tracing / conversation contracts that
// later real implementations satisfy, and supplies fully deterministic mock
// servers (MockLLMServer, MockToolServer, MockConfigProvider,
// MockTraceExporter) plus a ConversationRunner and LongConversationGenerator
// for driving multi-turn, 100+ turn and trace-completeness tests.
package mock

import (
	"encoding/json"
	"fmt"
	"os"
)

// ConversationTemplate defines a sequence of expected LLM responses. A
// MockLLMServer replays these turns in order across Generate/Stream calls.
type ConversationTemplate struct {
	// ID is the template identifier (e.g. "S-01").
	ID string `json:"id"`
	// Name is a human-readable template name.
	Name string `json:"name"`
	// Turns is the ordered list of expected responses (one per LLM call).
	Turns []ConversationTurn `json:"turns"`
}

// ConversationTurn describes a single expected LLM response.
type ConversationTurn struct {
	// AssistantContent is the textual reply the model is expected to produce.
	AssistantContent string `json:"assistant_content,omitempty"`
	// AssistantToolCalls are the tool calls the model is expected to issue.
	AssistantToolCalls []ExpectedToolCall `json:"assistant_tool_calls,omitempty"`
	// AssistantError, when non-empty, simulates the model returning an error.
	AssistantError string `json:"assistant_error,omitempty"`
	// FinishReason simulates the provider's finish_reason/stop_reason field
	// (stop|length|tool_calls|content_filter). When "length", the response
	// was truncated due to max_tokens.
	FinishReason string `json:"finish_reason,omitempty"`
}

// ExpectedToolCall is an expected tool call issued by the model.
type ExpectedToolCall struct {
	// ID uniquely identifies the tool call.
	ID string `json:"id"`
	// Name is the name of the tool to invoke.
	Name string `json:"name"`
	// Args holds the tool call arguments.
	Args map[string]any `json:"args"`
}

// NewConversationTemplate builds a template from the given turns.
func NewConversationTemplate(id, name string, turns ...ConversationTurn) *ConversationTemplate {
	return &ConversationTemplate{ID: id, Name: name, Turns: turns}
}

// LoadConversationTemplate reads a template from a JSON file.
func LoadConversationTemplate(path string) (*ConversationTemplate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read template: %w", err)
	}
	var tmpl ConversationTemplate
	if err := json.Unmarshal(data, &tmpl); err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}
	return &tmpl, nil
}
