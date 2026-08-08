package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

// Native provider implementations.
//
// The repository allows ZERO external Go modules, so the "native" SDKs
// described by the feature (OpenAI / Claude / Gemini) are not pulled in as
// dependencies. Instead each provider here is backed by the same zero-dependency
// HTTP approach used by EinoProvider: a small, hand-built JSON body is POSTed
// to that vendor's chat-completions-style REST endpoint and the response is
// decoded with the standard library. All three share the nativeChatModel helper
// so the HTTP mechanics and the "llm.request" span are defined once.

// parseDataURI splits a "data:image/<mime>;base64,<data>" URI into its media
// type and base64 payload. It returns ok=false when the string is not a data
// URI, in which case the caller should pass the URL as-is (e.g. for OpenAI).
func parseDataURI(s string) (mediaType, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return "", "", false
	}
	rest := s[len(prefix):]
	// The comma separates the metadata from the data payload.
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := rest[:comma]
	data = rest[comma+1:]
	// meta looks like "image/png;base64" — extract the media type.
	semi := strings.IndexByte(meta, ';')
	if semi >= 0 {
		mediaType = meta[:semi]
	} else {
		mediaType = meta
	}
	return mediaType, data, true
}

// Native provider constants. Grouped in one const block.
const (
	// openaiProviderName is the identifier returned by OpenAIProvider.Name().
	openaiProviderName = "openai"
	// openaiDefaultBaseURL is the OpenAI endpoint prefix; the chat completions
	// path is appended per request.
	openaiDefaultBaseURL = "https://api.openai.com/v1"
	// openaiDefaultModel is used when an OpenAI ModelConfig leaves Model empty.
	openaiDefaultModel = "gpt-4o-mini"
	// openaiChatPath is the OpenAI chat completions path.
	openaiChatPath = "/chat/completions"

	// claudeProviderName is the identifier returned by ClaudeProvider.Name().
	claudeProviderName = "claude"
	// claudeDefaultBaseURL is the Anthropic endpoint prefix; the messages path
	// is appended per request.
	claudeDefaultBaseURL = "https://api.anthropic.com/v1"
	// claudeDefaultModel is used when a Claude ModelConfig leaves Model empty.
	claudeDefaultModel = "claude-3-5-sonnet-latest"
	// claudeMessagesPath is the Anthropic messages endpoint path.
	claudeMessagesPath = "/messages"
	// claudeVersionHeader is the required Anthropic API version header value.
	claudeVersionHeader = "2023-06-01"
	// claudeSystemMaxTokens is a sane default when a Claude request omits
	// max_tokens, which the Anthropic API requires.
	claudeDefaultMaxTokens = 1024

	// geminiProviderName is the identifier returned by GeminiProvider.Name().
	geminiProviderName = "gemini"
	// geminiDefaultBaseURL is the Google GenAI endpoint prefix. The model path
	// and :generateContent action are appended per request.
	geminiDefaultBaseURL = "https://generativelanguage.googleapis.com"
	// geminiDefaultModel is used when a Gemini ModelConfig leaves Model empty.
	geminiDefaultModel = "gemini-1.5-flash"
	// geminiGeneratePathPrefix is joined to the default base URL to reach the
	// per-model generateContent endpoint.
	geminiGeneratePathPrefix = "/v1beta/models/"
	// geminiGenerateAction is the RPC action appended to the model path.
	geminiGenerateAction = ":generateContent"
	// geminiStreamAction is the streaming RPC action; alt=sse forces SSE
	// framing instead of newline-delimited JSON.
	geminiStreamAction = ":streamGenerateContent?alt=sse"
)

// nativeChatModel is a BaseChatModel shared by the OpenAI, Claude and Gemini
// providers. It owns the "llm.request" span, the JSON body construction, the
// HTTP round trip and the response decode. Each provider customizes it through
// small closures so the HTTP/span mechanics are not duplicated three times.
//
// Zero external dependencies: requests are built by hand and sent with the
// standard library net/http client.
type nativeChatModel struct {
	// client executes every request.
	client *http.Client
	// baseURL is the provider endpoint prefix (without the action path).
	baseURL string
	// apiKey is the credential for the request.
	apiKey string
	// provider is a human/telemetry label such as "openai".
	provider string
	// model is the effective model name for this instance.
	model string
	// endpoint is the full URL path/query used for this provider.
	endpoint string
	// header returns the extra HTTP headers (auth etc.) for this provider.
	header func() map[string]string
	// encode serializes msgs+options into the provider's request body.
	encode func(msgs []Message, opts []Option) ([]byte, error)
	// decode parses the provider's response body into an assistant Message.
	decode func(body []byte) (*Message, error)
}

// Compile-time assertion that the shared helper satisfies the chat contract.
var _ BaseChatModel = (*nativeChatModel)(nil)

// Generate sends the conversation to the configured provider and returns the
// assistant's response. It emits an "llm.request" span with the provider,
// model, token counts, latency and status attributes, matching HTTPChatModel.
func (m *nativeChatModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, spanNameLLMRequest, tracing.SpanKindClient)
	start := time.Now()
	logger := tracing.NewTraceLogger(span, slog.Default())
	span.SetAttributes(
		tracing.Attribute{Key: "provider", Value: m.provider},
		tracing.Attribute{Key: "model", Value: m.model},
	)

	msg, err := m.generate(spanCtx, msgs, opts...)
	if err != nil {
		span.SetStatus(tracing.SpanStatusError, err.Error())
	} else {
		span.SetStatus(tracing.SpanStatusOK, "")
		if msg.Usage != nil {
			span.SetAttributes(
				tracing.Attribute{Key: "tokens_input", Value: msg.Usage.InputTokens},
				tracing.Attribute{Key: "tokens_output", Value: msg.Usage.OutputTokens},
			)
		}
	}
	span.SetAttributes(tracing.Attribute{Key: "latency_ms", Value: time.Since(start).Milliseconds()})
	span.End()

	logger.Info("llm_generate_complete",
		"op", "llm.generate",
		"provider", m.provider,
		"model", m.model,
		"err", err != nil,
	)
	return msg, err
}

func (m *nativeChatModel) generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	body, err := m.encode(msgs, opts)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if m.header != nil {
		for k, v := range m.header() {
			req.Header.Set(k, v)
		}
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: do request: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			slog.Warn("llm_close_body", "err", cerr)
		}
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return nil, fmt.Errorf("llm: read error response: %w", rerr)
		}
		return nil, fmt.Errorf("llm: provider returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}

	data, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return nil, fmt.Errorf("llm: read response: %w", rerr)
	}
	return m.decode(data)
}

// Stream returns the generation result as a channel of MessageChunk. For the
// three known providers (openai, claude, gemini) it performs true SSE-based
// streaming: the request is sent with stream=true (or the streaming endpoint
// for Gemini) and the response body is parsed as Server-Sent Events, emitting
// one MessageChunk per content delta. For unknown providers it falls back to
// the fake single-chunk approach (Generate then emit). The channel is always
// closed.
func (m *nativeChatModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	switch m.provider {
	case claudeProviderName:
		return m.streamClaude(ctx, msgs, opts)
	case geminiProviderName:
		return m.streamGemini(ctx, msgs, opts)
	case openaiProviderName:
		return m.streamOpenAI(ctx, msgs, opts)
	default:
		return m.streamFake(ctx, msgs, opts)
	}
}

// streamFake is the fallback streaming implementation: it calls Generate and
// delivers the full response as a single chunk before closing the channel.
func (m *nativeChatModel) streamFake(ctx context.Context, msgs []Message, opts []Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, 1)
	resp, err := m.Generate(ctx, msgs, opts...)
	if err != nil {
		close(ch)
		return ch, err
	}
	ch <- MessageChunk{Role: resp.Role, Content: resp.Content}
	close(ch)
	return ch, nil
}

// streamRoundTrip builds the HTTP request, executes it, and verifies a 2xx
// status. It returns the response body for SSE parsing. The caller is
// responsible for closing the returned ReadCloser.
func (m *nativeChatModel) streamRoundTrip(ctx context.Context, body []byte, endpoint string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if m.header != nil {
		for k, v := range m.header() {
			req.Header.Set(k, v)
		}
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: do request: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer func() {
			if cerr := resp.Body.Close(); cerr != nil {
				slog.Warn("llm_close_error_body", "err", cerr)
			}
		}()
		payload, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return nil, fmt.Errorf("llm: read error response: %w", rerr)
		}
		return nil, fmt.Errorf("llm: provider returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	return resp.Body, nil
}

// withStreamFlag decodes the request body, injects "stream": true, and
// re-encodes it. This avoids duplicating the encode closures.
func withStreamFlag(body []byte) ([]byte, error) {
	var reqMap map[string]any
	if err := json.Unmarshal(body, &reqMap); err != nil {
		return nil, fmt.Errorf("llm: decode stream request: %w", err)
	}
	reqMap["stream"] = true
	out, err := json.Marshal(reqMap)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal stream request: %w", err)
	}
	return out, nil
}

// injectThinking retrieves the ThinkingConfig stored by WithThinking on
// genOpts (deleting the entry to prevent memory leaks), and if present with a
// non-None level, decodes the JSON body into a map, applies the provider
// adapter, and re-encodes. When targetKey is non-empty the adapter is applied
// to the nested sub-map (e.g. "generationConfig" for Gemini) instead of the
// top-level map. If no thinking config was set or the level is None the body
// is returned unchanged.
func injectThinking(data []byte, genOpts *GenerationOptions, adapter ThinkingAdapter, targetKey string) ([]byte, error) {
	cfg, ok := ThinkingFromOpts(genOpts)
	if !ok {
		return data, nil
	}
	DeleteThinking(genOpts)
	if cfg.Level == ThinkingNone {
		return data, nil
	}
	var reqMap map[string]any
	if err := json.Unmarshal(data, &reqMap); err != nil {
		return nil, fmt.Errorf("llm: decode request for thinking: %w", err)
	}
	target := reqMap
	if targetKey != "" {
		sub, ok := reqMap[targetKey].(map[string]any)
		if !ok {
			sub = map[string]any{}
		}
		target = sub
	}
	adapter.Apply(target, cfg)
	if targetKey != "" {
		reqMap[targetKey] = target
	}
	out, err := json.Marshal(reqMap)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request with thinking: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// OpenAI streaming
// ---------------------------------------------------------------------------

// streamOpenAI sends a streaming chat completions request and parses the SSE
// response. Each "data:" line carries an openAIStreamChunk; "data: [DONE]"
// signals the end of the stream. Tool-call fragments are accumulated across
// chunks and emitted in the Final MessageChunk.
func (m *nativeChatModel) streamOpenAI(ctx context.Context, msgs []Message, opts []Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, 64)

	body, err := m.encode(msgs, opts)
	if err != nil {
		close(ch)
		return ch, err
	}
	streamBody, err := withStreamFlag(body)
	if err != nil {
		close(ch)
		return ch, err
	}
	// Ensure stream_options.include_usage is set so usage stats are streamed.
	var streamReqMap map[string]any
	if jErr := json.Unmarshal(streamBody, &streamReqMap); jErr == nil {
		if streamReqMap["stream_options"] == nil {
			streamReqMap["stream_options"] = map[string]any{"include_usage": true}
			streamBody, err = json.Marshal(streamReqMap)
			if err != nil {
				close(ch)
				return ch, err
			}
		}
	}

	respBody, err := m.streamRoundTrip(ctx, streamBody, m.endpoint)
	if err != nil {
		close(ch)
		return ch, err
	}

	go func() {
		defer close(ch)
		defer func() {
			if cerr := respBody.Close(); cerr != nil {
				slog.Warn("llm_close_stream_body", "err", cerr)
			}
		}()

		reader := bufio.NewReaderSize(respBody, 64*1024)

		// Detect non-SSE JSON responses (e.g. error responses returned as
		// plain JSON instead of an event stream).
		isJSON, jsonBody, err := detectJSONResponse(reader)
		if err != nil {
			slog.Error("llm_stream_peek_error", "err", err)
			return
		}
		if isJSON {
			var parsed openAIResponse
			if err := json.Unmarshal(jsonBody, &parsed); err != nil {
				slog.Error("llm_stream_json_parse_error", "err", err)
				return
			}
			var content string
			var toolCalls []ToolCall
			var finishReason string
			if len(parsed.Choices) > 0 {
				content = contentToString(parsed.Choices[0].Message.Content)
				toolCalls = convertAssistantToolCalls(parsed.Choices[0].Message.ToolCalls)
				finishReason = parsed.Choices[0].FinishReason
			}
			if content != "" {
				ch <- MessageChunk{Role: RoleAssistant, Content: content}
			}
			final := MessageChunk{Role: RoleAssistant, Final: true, ToolCalls: toolCalls, FinishReason: finishReason}
			if parsed.Usage != nil {
				final.Usage = &Usage{
					InputTokens:  parsed.Usage.PromptTokens,
					OutputTokens: parsed.Usage.CompletionTokens,
					TotalTokens:  parsed.Usage.PromptTokens + parsed.Usage.CompletionTokens,
				}
			}
			ch <- final
			return
		}

		parser := NewDefaultSSEParser()
		events, _ := parser.Parse(reader) //nolint:errcheck

		toolCalls, finishReason, usage := accumulateOpenAIStreamToolCalls(events, ch)
		final := MessageChunk{Role: RoleAssistant, Final: true, ToolCalls: toolCalls, FinishReason: finishReason}
		if usage != nil {
			final.Usage = usage
		}
		ch <- final
	}()

	return ch, nil
}

// ---------------------------------------------------------------------------
// Claude (Anthropic) streaming
// ---------------------------------------------------------------------------

// claudeStreamEvent is a single SSE event in the Anthropic streaming protocol.
type claudeStreamEvent struct {
	Type         string               `json:"type"`
	Index        int                  `json:"index"`
	Message      *claudeStreamMessage `json:"message,omitempty"`
	ContentBlock *claudeStreamBlock   `json:"content_block,omitempty"`
	Delta        *claudeStreamDelta   `json:"delta,omitempty"`
}

// claudeStreamMessage carries the top-level message metadata from message_start.
type claudeStreamMessage struct {
	Role  string       `json:"role"`
	Usage *claudeUsage `json:"usage,omitempty"`
}

// claudeStreamBlock describes a content block from content_block_start.
type claudeStreamBlock struct {
	Type string `json:"type"`           // "text" or "tool_use"
	ID   string `json:"id,omitempty"`   // tool_use ID
	Name string `json:"name,omitempty"` // tool name
}

// claudeStreamDelta carries the incremental delta from content_block_delta.
type claudeStreamDelta struct {
	Type        string       `json:"type"`                   // "text_delta" or "input_json_delta"
	Text        string       `json:"text,omitempty"`         // text_delta text
	PartialJSON string       `json:"partial_json,omitempty"` // input_json_delta fragment
	StopReason  string       `json:"stop_reason,omitempty"`  // message_delta stop_reason
	Usage       *claudeUsage `json:"usage,omitempty"`        // message_delta output_tokens
}

// claudeToolAccum accumulates a single tool call's fragments across chunks.
type claudeToolAccum struct {
	id   string
	name string
	args strings.Builder
}

// streamClaude sends a streaming Anthropic messages request and parses the SSE
// response. The "stream": true flag is injected into the request body. Event
// types handled: message_start (role), content_block_delta (text/tool args),
// message_stop (final chunk with accumulated tool calls).
func (m *nativeChatModel) streamClaude(ctx context.Context, msgs []Message, opts []Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, 64)

	body, err := m.encode(msgs, opts)
	if err != nil {
		close(ch)
		return ch, err
	}
	streamBody, err := withStreamFlag(body)
	if err != nil {
		close(ch)
		return ch, err
	}

	respBody, err := m.streamRoundTrip(ctx, streamBody, m.endpoint)
	if err != nil {
		close(ch)
		return ch, err
	}

	go func() {
		defer close(ch)
		defer func() {
			if cerr := respBody.Close(); cerr != nil {
				slog.Warn("llm_close_stream_body", "err", cerr)
			}
		}()

		parser := NewDefaultSSEParser()
		events, _ := parser.Parse(respBody) //nolint:errcheck

		// Tool-call accumulation keyed by content block index.
		tools := map[int]*claudeToolAccum{}
		var toolIndices []int // preserve insertion order
		var stopReason string
		var inputTokens, outputTokens int
		var hasUsage bool
		finalSent := false

		for event := range events {
			if event.Data == "" {
				continue
			}
			var ce claudeStreamEvent
			if err := json.Unmarshal([]byte(event.Data), &ce); err != nil {
				slog.Warn("llm_stream_parse_skip", "err", err)
				continue
			}

			switch ce.Type {
			case "message_start":
				if ce.Message != nil {
					ch <- MessageChunk{Role: RoleAssistant}
					if ce.Message.Usage != nil {
						inputTokens = ce.Message.Usage.InputTokens
						hasUsage = true
					}
				}

			case "content_block_start":
				if ce.ContentBlock != nil && ce.ContentBlock.Type == "tool_use" {
					idx := ce.Index
					if _, ok := tools[idx]; !ok {
						tools[idx] = &claudeToolAccum{
							id:   ce.ContentBlock.ID,
							name: ce.ContentBlock.Name,
						}
						toolIndices = append(toolIndices, idx)
					}
				}

			case "content_block_delta":
				if ce.Delta == nil {
					continue
				}
				switch ce.Delta.Type {
				case "text_delta":
					if ce.Delta.Text != "" {
						ch <- MessageChunk{Role: RoleAssistant, Content: ce.Delta.Text}
					}
				case "input_json_delta":
					if accum, ok := tools[ce.Index]; ok {
						accum.args.WriteString(ce.Delta.PartialJSON)
					}
				}

			case "message_delta":
				if ce.Delta != nil {
					if ce.Delta.StopReason != "" {
						stopReason = ce.Delta.StopReason
					}
					if ce.Delta.Usage != nil {
						outputTokens = ce.Delta.Usage.OutputTokens
						hasUsage = true
					}
				}

			case "message_stop":
				final := MessageChunk{Role: RoleAssistant, Final: true, FinishReason: stopReason}
				if len(toolIndices) > 0 {
					final.ToolCalls = make([]ToolCall, 0, len(toolIndices))
					for _, idx := range toolIndices {
						accum := tools[idx]
						var args any
						if accum.args.Len() > 0 {
							var decoded any
							if err := json.Unmarshal([]byte(accum.args.String()), &decoded); err == nil {
								args = decoded
							} else {
								args = accum.args.String()
							}
						}
						final.ToolCalls = append(final.ToolCalls, ToolCall{
							ID:   accum.id,
							Name: accum.name,
							Args: args,
						})
					}
				}
				if hasUsage {
					final.Usage = &Usage{
						InputTokens:  inputTokens,
						OutputTokens: outputTokens,
						TotalTokens:  inputTokens + outputTokens,
					}
				}
				ch <- final
				finalSent = true
			}
		}

		// If the stream ended without message_stop, emit a final chunk so
		// the caller is not left waiting.
		if !finalSent {
			fallback := MessageChunk{Role: RoleAssistant, Final: true, FinishReason: stopReason}
			if hasUsage {
				fallback.Usage = &Usage{
					InputTokens:  inputTokens,
					OutputTokens: outputTokens,
					TotalTokens:  inputTokens + outputTokens,
				}
			}
			ch <- fallback
		}
	}()

	return ch, nil
}

// ---------------------------------------------------------------------------
// Gemini (Google) streaming
// ---------------------------------------------------------------------------

// streamGemini sends a streaming generateContent request and parses the SSE
// response. The endpoint switches from :generateContent to
// :streamGenerateContent?alt=sse. Each "data:" line carries a geminiResponse
// chunk; text parts are extracted and emitted as MessageChunks.
func (m *nativeChatModel) streamGemini(ctx context.Context, msgs []Message, opts []Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, 64)

	body, err := m.encode(msgs, opts)
	if err != nil {
		close(ch)
		return ch, err
	}

	// Switch the endpoint to the streaming variant.
	streamEndpoint := strings.Replace(m.endpoint, geminiGenerateAction, geminiStreamAction, 1)

	respBody, err := m.streamRoundTrip(ctx, body, streamEndpoint)
	if err != nil {
		close(ch)
		return ch, err
	}

	go func() {
		defer close(ch)
		defer func() {
			if cerr := respBody.Close(); cerr != nil {
				slog.Warn("llm_close_stream_body", "err", cerr)
			}
		}()

		parser := NewDefaultSSEParser()
		events, _ := parser.Parse(respBody) //nolint:errcheck

		var finishReason string
		var toolCalls []ToolCall
		var usage *Usage
		for event := range events {
			if event.Data == "" {
				continue
			}
			var parsed geminiResponse
			if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
				slog.Warn("llm_stream_parse_skip", "err", err)
				continue
			}
			// Gemini sends usageMetadata in the response chunks; the final
			// chunk carries the cumulative totals.
			if parsed.UsageMetadata != nil {
				usage = &Usage{
					InputTokens:  parsed.UsageMetadata.PromptTokenCount,
					OutputTokens: parsed.UsageMetadata.CandidatesTokenCount,
					TotalTokens:  parsed.UsageMetadata.PromptTokenCount + parsed.UsageMetadata.CandidatesTokenCount,
				}
			}
			if len(parsed.Candidates) > 0 {
				candidate := parsed.Candidates[0]
				if candidate.FinishReason != "" {
					finishReason = candidate.FinishReason
				}
				if candidate.Content != nil {
					var sb strings.Builder
					for _, part := range candidate.Content.Parts {
						sb.WriteString(part.Text)
						// Gemini sends complete functionCall parts (name+args
						// together, not fragmented like OpenAI), so we just
						// append when encountered.
						if part.FunctionCall != nil {
							toolCalls = append(toolCalls, ToolCall{
								ID:   fmt.Sprintf("call_%d", len(toolCalls)),
								Name: part.FunctionCall.Name,
								Args: part.FunctionCall.Args,
							})
						}
					}
					if sb.Len() > 0 {
						ch <- MessageChunk{Role: RoleAssistant, Content: sb.String()}
					}
				}
			}
		}

		final := MessageChunk{Role: RoleAssistant, Final: true, FinishReason: finishReason, ToolCalls: toolCalls}
		if usage != nil {
			final.Usage = usage
		}
		ch <- final
	}()

	return ch, nil
}

// nativeProviderBase holds the shared state of the three native providers.
// The concrete OpenAIProvider/ClaudeProvider/GeminiProvider embed it and only
// differ in their name, default base URL, model and wire format.
type nativeProviderBase struct {
	name           string
	defaultBaseURL string
	defaultModel   string
	httpClient     *http.Client
	defaultAPIKey  string
	models         []ModelInfo
}

// buildCommon resolves the effective model name and returns a shared error,
// avoiding duplication across the three Build methods.
func (b *nativeProviderBase) buildCommon(cfg ModelConfig) (model string, err error) {
	model = cfg.Model
	if model == "" {
		model = b.defaultModel
	}
	if model == "" {
		return "", errors.New("llm: provider build requires a non-empty model name")
	}
	return model, nil
}

// resolveBaseURL returns cfg.BaseURL if set, else the provider default.
func (b *nativeProviderBase) resolveBaseURL(cfg ModelConfig) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return b.defaultBaseURL
}

// resolveAPIKey returns cfg.APIKey if set, else the provider default.
func (b *nativeProviderBase) resolveAPIKey(cfg ModelConfig) string {
	if cfg.APIKey != "" {
		return cfg.APIKey
	}
	return b.defaultAPIKey
}

// newChatModel builds a nativeChatModel bound to this provider's shared
// endpoint/header/encode/decode closures.
func (b *nativeProviderBase) newChatModel(cfg ModelConfig, model, baseURL, apiKey, endpoint string, header func() map[string]string, encode func(msgs []Message, opts []Option) ([]byte, error), decode func(body []byte) (*Message, error)) *nativeChatModel {
	return &nativeChatModel{
		client:   b.httpClient,
		baseURL:  baseURL,
		apiKey:   apiKey,
		provider: b.name,
		model:    model,
		endpoint: endpoint,
		header:   header,
		encode:   encode,
		decode:   decode,
	}
}

// NativeProviderOption configures a native provider (OpenAI, Claude, Gemini)
// at construction time. The three constructors share this type so they expose
// the same WithNativeBaseURL/WithNativeAPIKey/WithNativeModels/
// WithNativeHTTPClient knobs.
type NativeProviderOption func(*nativeProviderBase)

// WithNativeBaseURL overrides the provider's default endpoint prefix.
func WithNativeBaseURL(url string) NativeProviderOption {
	return func(b *nativeProviderBase) { b.defaultBaseURL = url }
}

// WithNativeAPIKey sets the credential used when a ModelConfig does not supply
// one.
func WithNativeAPIKey(key string) NativeProviderOption {
	return func(b *nativeProviderBase) { b.defaultAPIKey = key }
}

// WithNativeModels sets the static model list reported by Models().
func WithNativeModels(models []ModelInfo) NativeProviderOption {
	return func(b *nativeProviderBase) { b.models = models }
}

// WithNativeHTTPClient injects a custom *http.Client (tests, proxies, timeouts).
func WithNativeHTTPClient(c *http.Client) NativeProviderOption {
	return func(b *nativeProviderBase) { b.httpClient = c }
}

// applyNativeOptions applies opts to b.
func (b *nativeProviderBase) applyNativeOptions(opts []NativeProviderOption) {
	for _, opt := range opts {
		opt(b)
	}
}

// Models returns a copy of the static model list.
func (b *nativeProviderBase) modelsCopy() []ModelInfo {
	out := make([]ModelInfo, len(b.models))
	copy(out, b.models)
	return out
}

// ---------------------------------------------------------------------------
// OpenAI
// ---------------------------------------------------------------------------

// OpenAIProvider is a ModelProvider backed by the OpenAI chat completions
// endpoint (/v1/chat/completions) using only the standard library.
type OpenAIProvider struct {
	nativeProviderBase
}

// Compile-time assertion.
var _ ModelProvider = (*OpenAIProvider)(nil)

// NewOpenAIProvider builds an OpenAIProvider with sensible defaults.
func NewOpenAIProvider(opts ...NativeProviderOption) *OpenAIProvider {
	p := &OpenAIProvider{}
	p.nativeProviderBase = nativeProviderBase{
		name:           openaiProviderName,
		defaultBaseURL: openaiDefaultBaseURL,
		defaultModel:   openaiDefaultModel,
		httpClient:     http.DefaultClient,
	}
	p.applyNativeOptions(opts)
	return p
}

// Name returns the provider identifier.
func (p *OpenAIProvider) Name() string { return p.name }

// Models returns the static model list.
func (p *OpenAIProvider) Models() []ModelInfo { return p.modelsCopy() }

// Build returns a native HTTP chat model bound to the OpenAI chat completions
// endpoint.
func (p *OpenAIProvider) Build(_ context.Context, cfg ModelConfig) (BaseChatModel, func(), error) {
	model, err := p.buildCommon(cfg)
	if err != nil {
		return nil, nil, err
	}
	baseURL := p.resolveBaseURL(cfg)
	apiKey := p.resolveAPIKey(cfg)
	endpoint := strings.TrimRight(baseURL, "/") + openaiChatPath

	header := func() map[string]string {
		h := map[string]string{}
		if apiKey != "" {
			h["Authorization"] = "Bearer " + apiKey
		}
		return h
	}
	encode := func(msgs []Message, opts []Option) ([]byte, error) {
		return encodeOpenAIRequest(cfg, model, msgs, opts)
	}
	decode := func(body []byte) (*Message, error) {
		return decodeOpenAIResponse(body)
	}
	return p.newChatModel(cfg, model, baseURL, apiKey, endpoint, header, encode, decode), func() {}, nil
}

// encodeOpenAIRequest marshals the OpenAI request body, reusing the shared
// openAIRequest/openAIMessage types.
func encodeOpenAIRequest(cfg ModelConfig, model string, msgs []Message, opts []Option) ([]byte, error) {
	genOpts := &GenerationOptions{}
	for _, opt := range opts {
		opt(genOpts)
	}
	reqMsgs := make([]openAIMessage, 0, len(msgs))
	for _, msg := range msgs {
		om := openAIMessage{
			Role:    string(msg.Role),
			Content: buildOpenAIContent(msg),
			Name:    msg.Name,
		}
		if msg.ToolCallID != "" {
			om.ToolCallID = msg.ToolCallID
		}
		if len(msg.ToolCalls) > 0 {
			om.ToolCalls = make([]openAIToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				argsJSON, _ := json.Marshal(tc.Args) //nolint:errcheck
				om.ToolCalls = append(om.ToolCalls, openAIToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openAIFunction{
						Name:      tc.Name,
						Arguments: string(argsJSON),
					},
				})
			}
		}
		reqMsgs = append(reqMsgs, om)
	}
	req := openAIRequest{
		Model:     model,
		Messages:  reqMsgs,
		MaxTokens: cfg.MaxTokens,
	}
	if genOpts.Temperature != nil {
		req.Temperature = *genOpts.Temperature
	} else {
		req.Temperature = cfg.Temperature
	}
	if genOpts.MaxTokens != nil {
		req.MaxTokens = *genOpts.MaxTokens
	}
	if len(genOpts.StopStrings) > 0 {
		req.Stop = genOpts.StopStrings
	}
	if len(genOpts.Tools) > 0 {
		req.Tools = make([]openAIToolDef, 0, len(genOpts.Tools))
		for _, td := range genOpts.Tools {
			params := td.Parameters
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			req.Tools = append(req.Tools, openAIToolDef{
				Type: "function",
				Function: openAIToolFunction{
					Name:        td.Name,
					Description: td.Description,
					Parameters:  params,
				},
			})
		}
		req.ToolChoice = "auto"
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}
	return injectThinking(data, genOpts, OpenAIThinkingAdapter{}, "")
}

// decodeOpenAIResponse parses an OpenAI chat completions response.
func decodeOpenAIResponse(body []byte) (*Message, error) {
	var parsed openAIResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	msg := &Message{Role: RoleAssistant}
	if len(parsed.Choices) > 0 {
		choice := parsed.Choices[0]
		msg.Content = contentToString(choice.Message.Content)
		msg.ToolCalls = convertAssistantToolCalls(choice.Message.ToolCalls)
		msg.FinishReason = choice.FinishReason
	}
	if parsed.Usage != nil {
		msg.Usage = &Usage{
			InputTokens:  parsed.Usage.PromptTokens,
			OutputTokens: parsed.Usage.CompletionTokens,
			TotalTokens:  parsed.Usage.PromptTokens + parsed.Usage.CompletionTokens,
		}
	}
	return msg, nil
}

// ---------------------------------------------------------------------------
// Claude (Anthropic)
// ---------------------------------------------------------------------------

// ClaudeProvider is a ModelProvider backed by the Anthropic Messages endpoint
// (/v1/messages) using only the standard library.
type ClaudeProvider struct {
	nativeProviderBase
}

// Compile-time assertion.
var _ ModelProvider = (*ClaudeProvider)(nil)

// NewClaudeProvider builds a ClaudeProvider with sensible defaults.
func NewClaudeProvider(opts ...NativeProviderOption) *ClaudeProvider {
	p := &ClaudeProvider{}
	p.nativeProviderBase = nativeProviderBase{
		name:           claudeProviderName,
		defaultBaseURL: claudeDefaultBaseURL,
		defaultModel:   claudeDefaultModel,
		httpClient:     http.DefaultClient,
	}
	p.applyNativeOptions(opts)
	return p
}

// Name returns the provider identifier.
func (p *ClaudeProvider) Name() string { return p.name }

// Models returns the static model list.
func (p *ClaudeProvider) Models() []ModelInfo { return p.modelsCopy() }

// Build returns a native HTTP chat model bound to the Anthropic messages
// endpoint.
func (p *ClaudeProvider) Build(_ context.Context, cfg ModelConfig) (BaseChatModel, func(), error) {
	model, err := p.buildCommon(cfg)
	if err != nil {
		return nil, nil, err
	}
	baseURL := p.resolveBaseURL(cfg)
	apiKey := p.resolveAPIKey(cfg)
	endpoint := strings.TrimRight(baseURL, "/") + claudeMessagesPath

	header := func() map[string]string {
		h := map[string]string{
			"x-api-key":         apiKey,
			"anthropic-version": claudeVersionHeader,
		}
		return h
	}
	encode := func(msgs []Message, opts []Option) ([]byte, error) {
		return encodeClaudeRequest(cfg, model, msgs, opts)
	}
	decode := func(body []byte) (*Message, error) {
		return decodeClaudeResponse(body)
	}
	return p.newChatModel(cfg, model, baseURL, apiKey, endpoint, header, encode, decode), func() {}, nil
}

// claudeRequest is the Anthropic messages request body. System prompts are
// hoisted into the top-level system field; the remaining messages carry a
// content array of text blocks.
type claudeRequest struct {
	Model     string              `json:"model"`
	MaxTokens int                 `json:"max_tokens"`
	System    string              `json:"system,omitempty"`
	Messages  []claudeUserMessage `json:"messages"`
}

// claudeUserMessage is a single user/assistant turn in the Anthropic protocol.
type claudeUserMessage struct {
	Role    string        `json:"role"`
	Content []claudeBlock `json:"content"`
}

// claudeBlock is a single content item (text or image).
type claudeBlock struct {
	Type   string             `json:"type"`
	Text   string             `json:"text,omitempty"`
	Source *claudeImageSource `json:"source,omitempty"` // when Type == "image"
}

// claudeImageSource is the Anthropic image source: a base64-encoded blob with
// its media type.
type claudeImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// claudeResponse is the Anthropic messages response body.
type claudeResponse struct {
	Content    []claudeBlock `json:"content"`
	Usage      *claudeUsage  `json:"usage,omitempty"`
	StopReason string        `json:"stop_reason,omitempty"`
}

// claudeUsage reports token consumption.
type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// buildClaudeBlocks converts a Message into a slice of claudeBlock values.
// When ContentBlocks is non-nil, it maps text and image blocks to the Anthropic
// format (image blocks require a base64 data URI). Otherwise it falls back to a
// single text block built from Content.
func buildClaudeBlocks(msg Message) []claudeBlock {
	if msg.ContentBlocks == nil {
		return []claudeBlock{{Type: "text", Text: msg.Content}}
	}
	blocks := make([]claudeBlock, 0, len(msg.ContentBlocks))
	for _, cb := range msg.ContentBlocks {
		switch cb.Type {
		case "text":
			blocks = append(blocks, claudeBlock{Type: "text", Text: cb.Text})
		case "image_url":
			if cb.ImageURL == nil {
				continue
			}
			mediaType, data, ok := parseDataURI(cb.ImageURL.URL)
			if !ok {
				continue // Claude requires base64 data URIs
			}
			blocks = append(blocks, claudeBlock{
				Type: "image",
				Source: &claudeImageSource{
					Type:      "base64",
					MediaType: mediaType,
					Data:      data,
				},
			})
		}
	}
	return blocks
}

// encodeClaudeRequest marshals a conversation into the Anthropic message body.
func encodeClaudeRequest(cfg ModelConfig, model string, msgs []Message, opts []Option) ([]byte, error) {
	genOpts := &GenerationOptions{}
	for _, opt := range opts {
		opt(genOpts)
	}
	maxTokens := claudeDefaultMaxTokens
	if genOpts.MaxTokens != nil {
		maxTokens = *genOpts.MaxTokens
	} else if cfg.MaxTokens > 0 {
		maxTokens = cfg.MaxTokens
	}

	var system string
	conv := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role == RoleSystem {
			// Anthropic takes system as a top-level field; concatenate any
			// system messages there.
			if system != "" {
				system += "\n"
			}
			system += msg.Content
			continue
		}
		conv = append(conv, msg)
	}

	reqMsgs := make([]claudeUserMessage, 0, len(conv))
	for _, msg := range conv {
		role := string(msg.Role)
		blocks := buildClaudeBlocks(msg)
		reqMsgs = append(reqMsgs, claudeUserMessage{Role: role, Content: blocks})
	}

	req := claudeRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    system,
		Messages:  reqMsgs,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}
	return injectThinking(data, genOpts, ClaudeThinkingAdapter{}, "")
}

// decodeClaudeResponse parses an Anthropic messages response, joining any text
// blocks into the message content.
func decodeClaudeResponse(body []byte) (*Message, error) {
	var parsed claudeResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	var sb strings.Builder
	for _, block := range parsed.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	msg := &Message{Role: RoleAssistant, Content: sb.String()}
	msg.FinishReason = parsed.StopReason
	if parsed.Usage != nil {
		msg.Usage = &Usage{
			InputTokens:  parsed.Usage.InputTokens,
			OutputTokens: parsed.Usage.OutputTokens,
			TotalTokens:  parsed.Usage.InputTokens + parsed.Usage.OutputTokens,
		}
	}
	return msg, nil
}

// ---------------------------------------------------------------------------
// Gemini (Google)
// ---------------------------------------------------------------------------

// GeminiProvider is a ModelProvider backed by the Google GenAI generateContent
// endpoint (/v1beta/models/{model}:generateContent) using only the standard
// library.
type GeminiProvider struct {
	nativeProviderBase
}

// Compile-time assertion.
var _ ModelProvider = (*GeminiProvider)(nil)

// NewGeminiProvider builds a GeminiProvider with sensible defaults.
func NewGeminiProvider(opts ...NativeProviderOption) *GeminiProvider {
	p := &GeminiProvider{}
	p.nativeProviderBase = nativeProviderBase{
		name:           geminiProviderName,
		defaultBaseURL: geminiDefaultBaseURL,
		defaultModel:   geminiDefaultModel,
		httpClient:     http.DefaultClient,
	}
	p.applyNativeOptions(opts)
	return p
}

// Name returns the provider identifier.
func (p *GeminiProvider) Name() string { return p.name }

// Models returns the static model list.
func (p *GeminiProvider) Models() []ModelInfo { return p.modelsCopy() }

// Build returns a native HTTP chat model bound to the generateContent endpoint,
// with the model name embedded in the request path.
func (p *GeminiProvider) Build(_ context.Context, cfg ModelConfig) (BaseChatModel, func(), error) {
	model, err := p.buildCommon(cfg)
	if err != nil {
		return nil, nil, err
	}
	baseURL := p.resolveBaseURL(cfg)
	apiKey := p.resolveAPIKey(cfg)
	endpoint := strings.TrimRight(baseURL, "/") + geminiGeneratePathPrefix + model + geminiGenerateAction

	header := func() map[string]string {
		h := map[string]string{}
		if apiKey != "" {
			h["x-goog-api-key"] = apiKey
		}
		return h
	}
	encode := func(msgs []Message, opts []Option) ([]byte, error) {
		return encodeGeminiRequest(cfg, msgs, opts)
	}
	decode := func(body []byte) (*Message, error) {
		return decodeGeminiResponse(body)
	}
	return p.newChatModel(cfg, model, baseURL, apiKey, endpoint, header, encode, decode), func() {}, nil
}

// geminiRequest is the Google GenAI generateContent request body.
type geminiRequest struct {
	Contents         []geminiContent  `json:"contents"`
	GenerationConfig *geminiGenConfig `json:"generationConfig,omitempty"`
}

// geminiContent is a single turn with role and text parts.
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiFunctionCall represents a function call in a Gemini response.
type geminiFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// geminiFunctionResponse represents a function response in a Gemini request.
type geminiFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// geminiPart is a single content part (text, inline image data, or a
// function call/response).
type geminiPart struct {
	Text             string                 `json:"text,omitempty"`
	InlineData       *geminiInlineData      `json:"inline_data,omitempty"`       // when image
	FunctionCall     *geminiFunctionCall    `json:"functionCall,omitempty"`      // when model calls a tool
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"` // when feeding back a tool result
}

// geminiInlineData is the Google GenAI inline image: base64-encoded data with
// its MIME type.
type geminiInlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

// geminiGenConfig carries optional sampling knobs. maxOutputTokens is the
// field name used by the Google GenAI generateContent API.
type geminiGenConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

// geminiResponse is the Google GenAI generateContent response body.
type geminiResponse struct {
	Candidates    []geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage      `json:"usageMetadata,omitempty"`
}

// geminiCandidate is a ranked generation result.
type geminiCandidate struct {
	Content      *geminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
}

// geminiUsage reports token consumption.
type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
}

// buildGeminiParts converts a Message into a slice of geminiPart values.
// When ContentBlocks is non-nil, it maps text and image blocks to the Gemini
// format (image blocks require a base64 data URI). Otherwise it falls back to a
// single text part built from Content.
//
// Tool-related messages are handled specially:
//   - Assistant messages with ToolCalls produce functionCall parts (one per
//     call), optionally preceded by a text part when Content is non-empty.
//   - Tool result messages (RoleTool) produce a single functionResponse part
//     whose Response carries the tool output.
func buildGeminiParts(msg Message) []geminiPart {
	// Tool result messages become functionResponse parts.
	if msg.Role == RoleTool {
		return []geminiPart{{
			FunctionResponse: &geminiFunctionResponse{
				Name:     msg.Name,
				Response: contentToResponseMap(msg.Content),
			},
		}}
	}

	// Assistant messages with tool calls become functionCall parts.
	if len(msg.ToolCalls) > 0 {
		var parts []geminiPart
		// Include any leading text content before the function calls.
		if msg.Content != "" {
			parts = append(parts, geminiPart{Text: msg.Content})
		}
		for _, tc := range msg.ToolCalls {
			parts = append(parts, geminiPart{
				FunctionCall: &geminiFunctionCall{
					Name: tc.Name,
					Args: argsToMap(tc.Args),
				},
			})
		}
		return parts
	}

	if msg.ContentBlocks == nil {
		return []geminiPart{{Text: msg.Content}}
	}
	parts := make([]geminiPart, 0, len(msg.ContentBlocks))
	for _, cb := range msg.ContentBlocks {
		switch cb.Type {
		case "text":
			parts = append(parts, geminiPart{Text: cb.Text})
		case "image_url":
			if cb.ImageURL == nil {
				continue
			}
			mediaType, data, ok := parseDataURI(cb.ImageURL.URL)
			if !ok {
				continue // Gemini requires base64 data URIs
			}
			parts = append(parts, geminiPart{
				InlineData: &geminiInlineData{
					MimeType: mediaType,
					Data:     data,
				},
			})
		}
	}
	return parts
}

// argsToMap converts a ToolCall.Args value (typically map[string]any from JSON
// decoding) into the map[string]any required by geminiFunctionCall. When the
// value is not already a map it is round-tripped through JSON; values that
// still cannot be represented as a map are wrapped under a "value" key.
func argsToMap(args any) map[string]any {
	if args == nil {
		return nil
	}
	if m, ok := args.(map[string]any); ok {
		return m
	}
	data, err := json.Marshal(args)
	if err != nil {
		return map[string]any{"value": args}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{"value": args}
	}
	return m
}

// contentToResponseMap converts a tool result Content string into the
// map[string]any required by geminiFunctionResponse. When the content is valid
// JSON that decodes to a map it is used directly; otherwise the raw string is
// wrapped under a "content" key so the result is never lost.
func contentToResponseMap(content string) map[string]any {
	if content == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(content), &m); err == nil {
		return m
	}
	return map[string]any{"content": content}
}

// encodeGeminiRequest marshals the conversation into the generateContent body.
func encodeGeminiRequest(cfg ModelConfig, msgs []Message, opts []Option) ([]byte, error) {
	genOpts := &GenerationOptions{}
	for _, opt := range opts {
		opt(genOpts)
	}

	contents := make([]geminiContent, 0, len(msgs))
	for _, msg := range msgs {
		role := string(msg.Role)
		if role == "system" {
			// Gemini has no system role; fold it into a user part so context
			// is not dropped.
			role = "user"
		}
		if role == "tool" {
			role = "user"
		}
		contents = append(contents, geminiContent{
			Role:  role,
			Parts: buildGeminiParts(msg),
		})
	}

	var gc *geminiGenConfig
	if genOpts.Temperature != nil || cfg.Temperature > 0 || (genOpts.MaxTokens != nil || cfg.MaxTokens > 0) {
		gc = &geminiGenConfig{}
		if genOpts.Temperature != nil {
			gc.Temperature = *genOpts.Temperature
		} else {
			gc.Temperature = cfg.Temperature
		}
		if genOpts.MaxTokens != nil {
			gc.MaxOutputTokens = *genOpts.MaxTokens
		} else {
			gc.MaxOutputTokens = cfg.MaxTokens
		}
	}

	req := geminiRequest{Contents: contents, GenerationConfig: gc}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}
	return injectThinking(data, genOpts, GeminiThinkingAdapter{}, "generationConfig")
}

// decodeGeminiResponse parses a generateContent response, joining the first
// candidate's text parts into the message content and extracting any
// functionCall parts into ToolCalls.
func decodeGeminiResponse(body []byte) (*Message, error) {
	var parsed geminiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	msg := &Message{Role: RoleAssistant}
	if len(parsed.Candidates) > 0 {
		candidate := parsed.Candidates[0]
		msg.FinishReason = candidate.FinishReason
		if candidate.Content != nil {
			var sb strings.Builder
			for _, part := range candidate.Content.Parts {
				sb.WriteString(part.Text)
				if part.FunctionCall != nil {
					msg.ToolCalls = append(msg.ToolCalls, ToolCall{
						ID:   fmt.Sprintf("call_%d", len(msg.ToolCalls)),
						Name: part.FunctionCall.Name,
						Args: part.FunctionCall.Args,
					})
				}
			}
			msg.Content = sb.String()
		}
	}
	if parsed.UsageMetadata != nil {
		msg.Usage = &Usage{
			InputTokens:  parsed.UsageMetadata.PromptTokenCount,
			OutputTokens: parsed.UsageMetadata.CandidatesTokenCount,
			TotalTokens:  parsed.UsageMetadata.PromptTokenCount + parsed.UsageMetadata.CandidatesTokenCount,
		}
	}
	return msg, nil
}
