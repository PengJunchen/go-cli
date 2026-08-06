// Package tools defines the tooling contracts that both the mock framework
// and real tool implementations satisfy. It declares the tool call/result
// model, the tool definition interface, and the registry interface. The mock
// framework (internal/mock) and later real registries implement these
// contracts.
package tools

//exempt:scan008 // pure type definitions, no executable logic

import "context"

// ToolCall is a request to invoke a named tool with arguments.
type ToolCall struct {
	// ID uniquely identifies the call so its result can be matched back.
	ID string `json:"id"`
	// Name is the name of the tool to invoke.
	Name string `json:"name"`
	// Args holds the tool arguments as a key-value map.
	Args map[string]any `json:"args"`
}

// ToolResult is the outcome of executing a tool call.
type ToolResult struct {
	// Output is the human-readable result produced by the tool.
	Output string `json:"output"`
	// Metadata holds structured extra data about the result.
	Metadata map[string]any `json:"metadata,omitempty"`
	// ToolCallID links the result back to the originating call.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ExecutionMode describes how a set of tool calls should be run.
type ExecutionMode string

const (
	// ExecutionModeSequential runs tool calls one after another.
	ExecutionModeSequential ExecutionMode = "sequential"
	// ExecutionModeParallel runs independent tool calls concurrently.
	ExecutionModeParallel ExecutionMode = "parallel"
	// ExecutionModeBlocking waits for each tool call to finish before the
	// next one starts.
	ExecutionModeBlocking ExecutionMode = "blocking"
)

// ToolDefinition is the small contract every tool satisfies. It describes
// the tool and knows how to execute a call against it.
type ToolDefinition interface {
	// Name returns the unique name of the tool.
	Name() string
	// Description returns a human-readable description of the tool.
	Description() string
	// Execute runs the tool for the given call and returns its result.
	Execute(ctx context.Context, call ToolCall) (*ToolResult, error)
}

// Parameterized is an optional interface that tools can implement to expose
// their OpenAI-compatible JSON Schema for parameters. Tools with structured
// input (e.g. MCP tools with an inputSchema) should implement this so the
// LLM knows what arguments it can pass.
//
//exempt:scan012 // optional interface; implementations in various tool files
type Parameterized interface {
	// Parameters returns the JSON Schema object describing the tool's
	// input parameters, or nil if the tool has no structured parameters.
	Parameters() any
}

// PromptGuideliner is an optional interface that tools can implement to
// provide usage guidelines injected into the system prompt. This is analogous
// to Parameterized: tools that wish to steer the model's tool usage should
// implement this so the SystemPromptBuilder can include their guidance.
//
//exempt:scan012 // optional interface; implementations in various tool files
type PromptGuideliner interface {
	// PromptGuidelines returns human-readable usage hints for the tool.
	// Each string is rendered as a separate bullet point in the system
	// prompt's tool guidelines section.
	PromptGuidelines() []string
}

// ToolRegistry is the contract a repository of tools satisfies. It supports
// registering, looking up and enumerating tools.
type ToolRegistry interface {
	// Register adds a tool definition to the registry.
	Register(ctx context.Context, def ToolDefinition) error
	// Get returns the tool with the given name, or an error if unknown.
	Get(ctx context.Context, name string) (ToolDefinition, error)
	// List returns all registered tool definitions.
	List(ctx context.Context) ([]ToolDefinition, error)
}
