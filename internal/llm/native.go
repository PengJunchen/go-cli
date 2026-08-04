package llm

import (
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
		events, _ := parser.Parse(respBody)

		// Per-request tool-call accumulation.
		var toolNameByIndex map[int]string
		var toolArgsBuf []string

		for event := range events {
			if event.Data == "[DONE]" {
				break
			}
			if event.Data == "" {
				continue
			}

			var chunk openAIStreamChunk
			if err := json.Unmarshal([]byte(event.Data), &chunk); err != nil {
				slog.Warn("llm_stream_parse_skip", "err", err)
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := chunk.Choices[0].Delta

			if delta.Content != "" {
				ch <- MessageChunk{Role: RoleAssistant, Content: delta.Content}
			}

			// Accumulate tool_call fragments emitted across chunks.
			for ci, tc := range delta.ToolCalls {
				if tc.Index == nil {
					tc.Index = &ci
				}
				idx := *tc.Index
				for len(toolArgsBuf) <= idx {
					toolArgsBuf = append(toolArgsBuf, "")
				}
				if tc.Function.Name != "" {
					if toolNameByIndex == nil {
						toolNameByIndex = make(map[int]string)
					}
					if toolNameByIndex[idx] == "" {
						toolNameByIndex[idx] = tc.Function.Name
					}
				}
				if tc.Function.Arguments != "" {
					toolArgsBuf[idx] += tc.Function.Arguments
				}
			}
		}

		// Emit the final accumulated assistant message.
		final := MessageChunk{Role: RoleAssistant, Final: true}
		if toolNameByIndex != nil {
			final.ToolCalls = make([]ToolCall, len(toolArgsBuf))
			for idx, name := range toolNameByIndex {
				var args any
				if idx < len(toolArgsBuf) && toolArgsBuf[idx] != "" {
					var decoded any
					if err := json.Unmarshal([]byte(toolArgsBuf[idx]), &decoded); err == nil {
						args = decoded
					} else {
						args = toolArgsBuf[idx]
					}
				}
				final.ToolCalls[idx] = ToolCall{
					ID:   fmt.Sprintf("call_%d", idx),
					Name: name,
					Args: args,
				}
			}
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
	Type         string                `json:"type"`
	Index        int                   `json:"index"`
	Message      *claudeStreamMessage  `json:"message,omitempty"`
	ContentBlock *claudeStreamBlock    `json:"content_block,omitempty"`
	Delta        *claudeStreamDelta    `json:"delta,omitempty"`
}

// claudeStreamMessage carries the top-level message metadata from message_start.
type claudeStreamMessage struct {
	Role string `json:"role"`
}

// claudeStreamBlock describes a content block from content_block_start.
type claudeStreamBlock struct {
	Type string `json:"type"`           // "text" or "tool_use"
	ID   string `json:"id,omitempty"`   // tool_use ID
	Name string `json:"name,omitempty"` // tool name
}

// claudeStreamDelta carries the incremental delta from content_block_delta.
type claudeStreamDelta struct {
	Type        string `json:"type"`                   // "text_delta" or "input_json_delta"
	Text        string `json:"text,omitempty"`         // text_delta text
	PartialJSON string `json:"partial_json,omitempty"` // input_json_delta fragment
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
		events, _ := parser.Parse(respBody)

		// Tool-call accumulation keyed by content block index.
		tools := map[int]*claudeToolAccum{}
		var toolIndices []int // preserve insertion order
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

			case "message_stop":
				final := MessageChunk{Role: RoleAssistant, Final: true}
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
				ch <- final
				finalSent = true
			}
		}

		// If the stream ended without message_stop, emit a final chunk so
		// the caller is not left waiting.
		if !finalSent {
			ch <- MessageChunk{Role: RoleAssistant, Final: true}
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
		events, _ := parser.Parse(respBody)

		for event := range events {
			if event.Data == "" {
				continue
			}
			var parsed geminiResponse
			if err := json.Unmarshal([]byte(event.Data), &parsed); err != nil {
				slog.Warn("llm_stream_parse_skip", "err", err)
				continue
			}
			if len(parsed.Candidates) > 0 && parsed.Candidates[0].Content != nil {
				var sb strings.Builder
				for _, part := range parsed.Candidates[0].Content.Parts {
					sb.WriteString(part.Text)
				}
				if sb.Len() > 0 {
					ch <- MessageChunk{Role: RoleAssistant, Content: sb.String()}
				}
			}
		}

		ch <- MessageChunk{Role: RoleAssistant, Final: true}
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
			Content: msg.Content,
			Name:    msg.Name,
		}
		if msg.ToolCallID != "" {
			om.ToolCallID = msg.ToolCallID
		}
		if len(msg.ToolCalls) > 0 {
			om.ToolCalls = make([]openAIToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				argsJSON, _ := json.Marshal(tc.Args)
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
	return data, nil
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
		msg.Content = choice.Message.Content
		msg.ToolCalls = convertAssistantToolCalls(choice.Message.ToolCalls)
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

// claudeBlock is a single content item (text only in this model).
type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// claudeResponse is the Anthropic messages response body.
type claudeResponse struct {
	Content []claudeBlock `json:"content"`
	Usage   *claudeUsage  `json:"usage,omitempty"`
}

// claudeUsage reports token consumption.
type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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
		block := claudeBlock{Type: "text", Text: msg.Content}
		reqMsgs = append(reqMsgs, claudeUserMessage{Role: role, Content: []claudeBlock{block}})
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
	return data, nil
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

// geminiPart is a single content part (text only in this model).
type geminiPart struct {
	Text string `json:"text,omitempty"`
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
	Content *geminiContent `json:"content,omitempty"`
}

// geminiUsage reports token consumption.
type geminiUsage struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
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
			Parts: []geminiPart{{Text: msg.Content}},
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
	return data, nil
}

// decodeGeminiResponse parses a generateContent response, joining the first
// candidate's text parts into the message content.
func decodeGeminiResponse(body []byte) (*Message, error) {
	var parsed geminiResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}
	msg := &Message{Role: RoleAssistant}
	if len(parsed.Candidates) > 0 && parsed.Candidates[0].Content != nil {
		var sb strings.Builder
		for _, part := range parsed.Candidates[0].Content.Parts {
			sb.WriteString(part.Text)
		}
		msg.Content = sb.String()
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
