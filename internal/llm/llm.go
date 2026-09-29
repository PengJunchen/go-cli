// Package llm defines the LLM provider contracts that both the mock
// framework and real providers satisfy. It is a pure data/contract package:
// it declares the message model, chat model and provider interfaces. The
// mock framework (internal/mock) and later real implementations implement
// these contracts, and the contract tests in internal/mock verify behavior
// against them.
package llm

import (
	"context"
	"errors"
	"fmt"
)

// SourceKind identifies the runtime origin of a message. Source coordinates
// are carried between core layers only and are never sent to a provider.
type SourceKind uint8

const (
	SourceNone SourceKind = iota
	SourcePriorPublished
	SourceRunTranscript
	SourceRunSummary
	SourceEphemeral
	SourcePendingRunInput
)

// SourceThrough is a cursor in the immutable run-start history followed by
// the current run's canonical transcript.
type SourceThrough struct {
	PriorHistoryCount uint64
	TranscriptCount   uint64
}

// SourceRef identifies one source-backed item or a runtime-only ephemeral
// item. Fields not selected by Kind must remain at their zero value.
type SourceRef struct {
	Kind             SourceKind
	RunID            string
	HistoryRevision  string
	BranchID         string
	SourceEntryID    string
	PriorOrdinal     uint64
	PriorSourceCount uint64
	TranscriptIndex  uint64
	Through          SourceThrough
}

var ErrInvalidSourceRef = errors.New("llm: invalid runtime source reference")

// Validate verifies that the source reference contains exactly the fields
// allowed by its tag. A zero ordinal, count, or index is valid when selected.
func (source SourceRef) Validate() error {
	invalid := func(reason string) error {
		return fmt.Errorf("%w: %s", ErrInvalidSourceRef, reason)
	}
	noPrior := source.HistoryRevision == "" && source.BranchID == "" && source.SourceEntryID == "" && source.PriorOrdinal == 0
	noRunCount := source.PriorSourceCount == 0 && source.TranscriptIndex == 0
	noThrough := source.Through == (SourceThrough{})
	switch source.Kind {
	case SourceNone, SourceEphemeral:
		if source.RunID != "" || !noPrior || !noRunCount || !noThrough {
			return invalid("non-durable source carries coordinates")
		}
	case SourcePriorPublished:
		if source.HistoryRevision == "" || source.BranchID == "" || source.SourceEntryID == "" || source.RunID != "" || !noRunCount || !noThrough {
			return invalid("prior source requires revision, branch, and entry ID only")
		}
	case SourceRunTranscript:
		if source.RunID == "" || !noPrior || !noThrough {
			return invalid("transcript source requires run ID and transcript coordinates only")
		}
	case SourceRunSummary:
		if source.RunID == "" || !noPrior || !noRunCount || source.Through == (SourceThrough{}) {
			return invalid("run summary requires run ID and non-empty source boundary only")
		}
	case SourcePendingRunInput:
		if source.RunID == "" || !noPrior || !noThrough || source.TranscriptIndex != 0 {
			return invalid("pending input requires run ID and prior source count only")
		}
	default:
		return invalid("unknown source kind")
	}
	return nil
}

// RuntimeMetadata carries provenance and a canonical receipt outcome through
// runtime transformations. It is deliberately absent from provider payloads
// and durable session records.
type RuntimeMetadata struct {
	Source         SourceRef
	ReceiptOutcome string
}

// LocalStreamAbortCause marks cancellation initiated by a stream consumer
// because the stream could not be accepted locally. Its fixed error text keeps
// provider response details out of cancellation diagnostics, while Unwrap
// preserves the original cause for errors.Is and errors.As classification.
type LocalStreamAbortCause struct {
	cause error
}

// NewLocalStreamAbortCause wraps the reason a stream consumer stopped reading.
func NewLocalStreamAbortCause(cause error) *LocalStreamAbortCause {
	return &LocalStreamAbortCause{cause: cause}
}

// Error returns a stable, provider-independent description.
func (*LocalStreamAbortCause) Error() string { return "local stream abort" }

// Unwrap exposes the local failure for typed and sentinel error classification.
func (c *LocalStreamAbortCause) Unwrap() error { return c.cause }

func cancelStreamAttempt(parent context.Context, cancel context.CancelCauseFunc, cause error) {
	if inherited := context.Cause(parent); inherited != nil {
		cancel(inherited)
		return
	}
	if cause == nil {
		cause = context.Canceled
	}
	cancel(NewLocalStreamAbortCause(cause))
}

func drainMessageChunks(ch <-chan MessageChunk) {
	if ch == nil {
		return
	}
	for range ch {
	}
}

// forwardMessageStreamAttempt owns a successful attempt channel until it closes
// so the attempt context can be released without canceling the live stream.
func forwardMessageStreamAttempt(parent context.Context, cancel context.CancelCauseFunc, source <-chan MessageChunk) <-chan MessageChunk {
	out := make(chan MessageChunk, cap(source))
	go func() {
		defer close(out)
		var finalChunk *MessageChunk
		fail := func(failure error) {
			select {
			case out <- MessageChunk{Error: failure}:
			case <-parent.Done():
			}
			cancelStreamAttempt(parent, cancel, failure)
			drainMessageChunks(source)
		}
		for {
			var chunk MessageChunk
			var ok bool
			select {
			case <-parent.Done():
				cause := context.Cause(parent)
				if cause == nil {
					cause = parent.Err()
				}
				cancelStreamAttempt(parent, cancel, cause)
				drainMessageChunks(source)
				return
			case chunk, ok = <-source:
			}
			if !ok {
				cancel(nil)
				if finalChunk != nil {
					select {
					case out <- *finalChunk:
					case <-parent.Done():
					}
				}
				return
			}
			if chunk.Error != nil {
				fail(chunk.Error)
				return
			}
			if finalChunk != nil {
				fail(errors.New("llm: stream emitted data after its final chunk"))
				return
			}
			if chunk.Final {
				final := chunk
				finalChunk = &final
				continue
			}
			select {
			case out <- chunk:
			case <-parent.Done():
				cause := context.Cause(parent)
				if cause == nil {
					cause = parent.Err()
				}
				cancelStreamAttempt(parent, cancel, cause)
				drainMessageChunks(source)
				return
			}
		}
	}()
	return out
}

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
	// Runtime carries source provenance only within the runtime. Provider
	// encoders must never include it in protocol payloads.
	Runtime RuntimeMetadata `json:"-"`
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

	// Error carries an error detected by middleware (e.g. loop detection)
	// so it can be propagated to the caller through the chunk stream. It is
	// never JSON-serialized. When non-nil, the caller should stop consuming
	// the stream and return the error.
	Error error `json:"-"`
}

func sendStreamError(_ context.Context, ch chan MessageChunk, err error) {
	if err == nil {
		return
	}
	select {
	case ch <- MessageChunk{Error: err}:
	default:
		// On terminal failure, prefer preserving the error signal over queued
		// candidate deltas. The consumer must never mistake a truncated channel
		// close for a successful stream.
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- MessageChunk{Error: err}:
		default:
		}
	}
}

// ModelInfo describes a model exposed by a provider.
type ModelInfo struct {
	Name            string  `json:"name"`
	ContextWindow   int     `json:"context_window"`
	MaxOutputTokens int     `json:"max_output_tokens,omitempty"`
	InputPrice      float64 `json:"input_price,omitempty"`
	OutputPrice     float64 `json:"output_price,omitempty"`
	Modality        string  `json:"modality,omitempty"`
	APIBase         string  `json:"api_base,omitempty"`
	Description     string  `json:"description,omitempty"`
	// Reasoning reports whether the model supports extended reasoning
	// (chain-of-thought / thinking) tokens.
	Reasoning bool `json:"reasoning"`
	// ToolCall reports whether the model supports function/tool calling.
	ToolCall bool `json:"tool_call"`
	// StructuredOutput reports whether the model supports structured/JSON
	// output mode.
	StructuredOutput bool `json:"structured_output"`
	// CacheReadPrice is the per-million-token price for reading cached
	// prompt tokens, sourced from the external registry.
	CacheReadPrice float64 `json:"cache_read_price,omitempty"`
	// CacheWritePrice is the per-million-token price for writing prompt
	// tokens to the cache, sourced from the external registry.
	CacheWritePrice float64 `json:"cache_write_price,omitempty"`
	// InputTokenLimit is the maximum number of input tokens the model
	// accepts, sourced from the external registry.
	InputTokenLimit int `json:"input_token_limit,omitempty"`
	// Knowledge is the model's knowledge cutoff date (e.g. "2024-04").
	Knowledge string `json:"knowledge,omitempty"`
	// ReleaseDate is the model's release date (e.g. "2024-05-13").
	ReleaseDate string `json:"release_date,omitempty"`
}

// ModelConfig configures how a provider should build a chat model.
type ModelConfig struct {
	Model          string        `json:"model"`
	Temperature    float64       `json:"temperature"`
	TemperatureSet bool          `json:"-"`
	MaxTokens      int           `json:"max_tokens"`
	MaxTokensSet   bool          `json:"-"`
	APIKey         string        `json:"api_key,omitempty"`
	BaseURL        string        `json:"base_url,omitempty"`
	Credential     CredentialRef `json:"-"`
	// credentialResolved marks a resolver-produced config. Adapter-level
	// constructor defaults must not fill a missing key after scoped resolution.
	credentialResolved bool
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
	// implementation must close the channel only after it has stopped sending
	// and finished cleaning up request resources such as response bodies.
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
