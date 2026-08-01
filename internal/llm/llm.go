// Package llm defines the LLM provider contracts that both the mock
// framework and real providers satisfy. It is a pure data/contract package:
// it declares the message model, chat model and provider interfaces. The
// mock framework (internal/mock) and later real implementations implement
// these contracts, and the contract tests in internal/mock verify behavior
// against them.
package llm

import "context"

// Role identifies the speaker of a message in a conversation.
type Role string

const (
	// RoleUser is a message authored by the end user.
	RoleUser Role = "user"
	// RoleAssistant is a message authored by the model.
	RoleAssistant Role = "assistant"
	// RoleTool is a message containing the result of a tool call.
	RoleTool Role = "tool"
	// RoleSystem is a system instruction message.
	RoleSystem Role = "system"
)

// ToolCall describes a request from the model to invoke a tool.
type ToolCall struct {
	// ID uniquely identifies the tool call so its result can be matched back.
	ID string `json:"id"`
	// Name is the name of the tool to invoke.
	Name string `json:"name"`
	// Args holds the tool arguments as a JSON-marshalable value.
	Args any `json:"args,omitempty"`
}

// Usage reports token consumption for a model response.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// Message is a single message in a conversation.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
	Usage      *Usage     `json:"usage,omitempty"`
}

// MessageChunk is a single incremental chunk emitted by streaming.
type MessageChunk struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// ModelInfo describes a model exposed by a provider.
type ModelInfo struct {
	Name          string `json:"name"`
	ContextWindow int    `json:"context_window"`
}

// ModelConfig configures how a provider should build a chat model.
type ModelConfig struct {
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
	APIKey      string  `json:"api_key,omitempty"`
	BaseURL     string  `json:"base_url,omitempty"`
}

// GenerationOptions carries optional knobs for a single generation call.
type GenerationOptions struct {
	Temperature *float64
	MaxTokens   *int
	StopStrings []string
}

// Option configures a generation call.
type Option func(*GenerationOptions)

// WithTemperature overrides the temperature for a single call.
func WithTemperature(t float64) Option {
	return func(o *GenerationOptions) { o.Temperature = &t }
}

// WithMaxTokens overrides the max token count for a single call.
func WithMaxTokens(n int) Option {
	return func(o *GenerationOptions) { o.MaxTokens = &n }
}

// WithStopStrings sets the stop sequences for a single call.
func WithStopStrings(ss ...string) Option {
	return func(o *GenerationOptions) { o.StopStrings = ss }
}

// BaseChatModel is the contract every chat model (mock or real, streaming or
// not) must satisfy.
type BaseChatModel interface {
	// Generate produces a complete response for the given conversation.
	Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error)
	// Stream produces the conversation response as a chunk channel. The
	// channel must eventually be closed by the implementation.
	Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error)
}

// ModelProvider is the contract a provider registry satisfies. It can build
// concrete chat models and report the models it exposes.
type ModelProvider interface {
	// Name returns the provider identifier.
	Name() string
	// Build constructs a chat model from cfg. The returned cleanup function
	// releases any resources; it may be nil-safe to call.
	Build(ctx context.Context, cfg ModelConfig) (BaseChatModel, func(), error)
	// Models lists the models this provider exposes.
	Models() []ModelInfo
}
