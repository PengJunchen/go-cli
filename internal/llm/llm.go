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
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// ContentBlocks holds typed content parts for multimodal messages
	// (text + images). When non-nil, encoders use ContentBlocks instead of
	// Content. When nil, Content is used as before (backward compatible).
	ContentBlocks []ContentBlock `json:"content_blocks,omitempty"`
	ToolCalls     []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID    string         `json:"tool_call_id,omitempty"`
	Name          string         `json:"name,omitempty"`
	Usage         *Usage         `json:"usage,omitempty"`
	// FinishReason reports why the model stopped generating:
	// stop|length|tool_calls|content_filter. When "length", the output was
	// truncated due to max_tokens and the caller may request a continuation.
	FinishReason string `json:"finish_reason,omitempty"`
}

// ImageURL represents an image specified by URL or base64 data URI.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // high|low|auto (OpenAI)
}

// ContentBlock is a typed content part within a multimodal message.
// When Message.ContentBlocks is non-nil, encoders use it instead of
// the plain Content string.
type ContentBlock struct {
	Type     string    `json:"type"`                // "text" | "image_url"
	Text     string    `json:"text,omitempty"`      // when Type == "text"
	ImageURL *ImageURL `json:"image_url,omitempty"` // when Type == "image_url"
}

// MessageChunk is a single incremental chunk emitted by streaming.
type MessageChunk struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`

	// Final marks the last chunk of a generation. When tool calls were
	// accumulated during streaming, the Final chunk carries the complete
	// ToolCalls so the caller can reconstruct the full assistant message.
	Final bool `json:"final,omitempty"`

	// ToolCalls holds complete tool calls, populated only on the Final chunk
	// after per-call argument fragments have been merged.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// FinishReason, populated on the Final chunk, reports why the model
	// stopped generating (stop|length|tool_calls|content_filter).
	FinishReason string `json:"finish_reason,omitempty"`

	// Usage, populated on the Final chunk, reports the token consumption
	// for the generation as reported by the API. When the provider does not
	// stream usage, it remains nil and callers fall back to estimation.
	Usage *Usage `json:"usage,omitempty"`
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

// ToolDefinition describes a tool that the model may invoke. It is the
// LLM-layer representation of a tool, independent of the tools package.
type ToolDefinition struct {
	// Name is the tool identifier the model uses in a tool_call.
	Name string `json:"name"`
	// Description explains what the tool does so the model can decide when to
	// call it.
	Description string `json:"description,omitempty"`
	// Parameters is the JSON Schema for the tool's input parameters.
	Parameters any `json:"parameters,omitempty"`
}

// GenerationOptions carries optional knobs for a single generation call.
type GenerationOptions struct {
	Temperature *float64
	MaxTokens   *int
	StopStrings []string
	Tools       []ToolDefinition
	// Thinking carries the reasoning-depth configuration for this call.
	// When nil, no thinking parameters are injected.
	Thinking *ThinkingConfig
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

// WithTools provides the tool definitions the model may invoke during this
// generation call. When non-empty, the OpenAI-compatible providers include a
// "tools" field in the request body so the model knows what tools are available.
func WithTools(tools []ToolDefinition) Option {
	return func(o *GenerationOptions) { o.Tools = tools }
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

// ModelSelector routes an LLM call to the appropriate model based on the
// given TaskType. Implementations typically hold a primary model for full chat
// turns and a smaller, cheaper model for lightweight tasks (summaries, title
// generation, extraction).
type ModelSelector interface {
	// SelectModel returns the BaseChatModel to use for the given taskType.
	// When no small model is configured, implementations return the primary
	// model for all task types.
	SelectModel(taskType TaskType) BaseChatModel
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
