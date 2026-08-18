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

// ProviderOption configures an EinoProvider at construction time.
type ProviderOption func(*EinoProvider)

// EinoProvider is a default ModelProvider backed by a zero-dependency,
// OpenAI-compatible HTTP client. It builds HTTPChatModel instances that speak
// the OpenAI chat completions protocol against an arbitrary base URL, so it can
// reach OpenAI, DeepSeek, Ark, local inference servers, and other compatible
// endpoints without requiring the Eino SDK.
type EinoProvider struct {
	name string

	// httpClient is used for every request. It may be injected (e.g. with a
	// custom transport) for tests or proxies.
	httpClient *http.Client

	// defaultBaseURL is the endpoint prefix used when a ModelConfig does not
	// provide its own BaseURL. The chat completions path is appended to it.
	defaultBaseURL string

	// defaultAPIKeySource lazily supplies the bearer token used when a
	// ModelConfig does not carry an APIKey. It may return an empty string when
	// no key is configured (e.g. local endpoints).
	defaultAPIKeySource func() string

	// defaultModel is the model name used when a ModelConfig leaves Model empty.
	defaultModel string

	// models is the static model list reported by Models().
	models []ModelInfo
}

// Default provider constants. These live in one place so the scanner does not
// flag them as scattered hardcoded values and so callers can inspect them.
const (
	// defaultProviderName is the identifier returned by Name() unless overridden.
	defaultProviderName = "eino"
	// defaultProviderBaseURL is the OpenAI-compatible endpoint prefix used when
	// no base URL is configured. The chat completions path is joined to it.
	defaultProviderBaseURL = "https://api.openai.com/v1"
	// defaultProviderModel is the model name used when ModelConfig leaves it empty
	// and no WithModels default is set.
	defaultProviderModel = "gpt-4o-mini"
	// chatCompletionsPath is appended to the base URL to reach the chat endpoint.
	chatCompletionsPath = "/chat/completions"

	// spanNameLLMRequest is the span name emitted around every generation call.
	spanNameLLMRequest = "llm.request"
)

// NewEinoProvider builds an EinoProvider with sensible defaults. Provide options
// to override the name, HTTP client, base URL, API key, model, or model list.
func NewEinoProvider(opts ...ProviderOption) *EinoProvider {
	p := &EinoProvider{
		name:                defaultProviderName,
		httpClient:          http.DefaultClient,
		defaultBaseURL:      defaultProviderBaseURL,
		defaultAPIKeySource: func() string { return "" },
		defaultModel:        defaultProviderModel,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// WithHTTPClient injects a custom *http.Client. Useful for tests (httptest
// transport) and for configuring timeouts or proxies.
func WithHTTPClient(c *http.Client) ProviderOption {
	return func(p *EinoProvider) { p.httpClient = c }
}

// WithBaseURL overrides the default OpenAI-compatible endpoint prefix. The
// chat completions path is joined to this value per request.
func WithBaseURL(url string) ProviderOption {
	return func(p *EinoProvider) { p.defaultBaseURL = url }
}

// WithAPIKey sets the bearer token used when a ModelConfig does not supply one.
func WithAPIKey(key string) ProviderOption {
	return func(p *EinoProvider) { p.defaultAPIKeySource = func() string { return key } }
}

// WithProviderName overrides the identifier returned by Name().
func WithProviderName(name string) ProviderOption {
	return func(p *EinoProvider) { p.name = name }
}

// WithModels sets the static model list reported by Models(). It also sets the
// default model name to the first entry when present.
func WithModels(models []ModelInfo) ProviderOption {
	return func(p *EinoProvider) {
		p.models = models
		if len(models) > 0 && models[0].Name != "" {
			p.defaultModel = models[0].Name
		}
	}
}

// WithDefaultModel overrides the model name used when a ModelConfig leaves it
// empty.
func WithDefaultModel(name string) ProviderOption {
	return func(p *EinoProvider) { p.defaultModel = name }
}

// Name returns the provider identifier.
func (p *EinoProvider) Name() string { return p.name }

// Models returns the static model list configured for this provider.
func (p *EinoProvider) Models() []ModelInfo {
	out := make([]ModelInfo, len(p.models))
	copy(out, p.models)
	return out
}

// Build constructs an HTTPChatModel bound to this provider. The returned
// cleanup function is a no-op because the HTTP client owns no goroutines. It
// returns an error only when cfg is clearly invalid (model name empty and no
// provider default is available).
func (p *EinoProvider) Build(_ context.Context, cfg ModelConfig) (BaseChatModel, func(), error) {
	model := cfg.Model
	if model == "" {
		model = p.defaultModel
	}
	if model == "" {
		return nil, nil, errors.New("llm: provider build requires a non-empty model name")
	}
	m := &HTTPChatModel{
		provider: p,
		cfg:      cfg,
		client:   p.httpClient,
	}
	// Override resolved model back onto the config so the model reports the
	// effective name.
	m.cfg.Model = model
	return m, func() {}, nil
}

// HTTPChatModel is a BaseChatModel that calls an OpenAI-compatible chat
// completions endpoint using only the standard library. It is returned by
// EinoProvider.Build.
//
// Zero external dependencies: it constructs the JSON body by hand, POSTs it to
// {base}/chat/completions, and decodes the OpenAI-shaped response.
type HTTPChatModel struct {
	provider *EinoProvider
	cfg      ModelConfig
	client   *http.Client
}

// Compile-time assertions that the concrete types satisfy the llm contracts.
var (
	_ BaseChatModel = (*HTTPChatModel)(nil)
	_ ModelProvider = (*EinoProvider)(nil)
)

// Generate sends the conversation to the configured provider and returns the
// assistant's response as a llm.Message. It emits a "llm.request" span with the
// provider, model, token counts, latency and status attributes.
func (m *HTTPChatModel) Generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	span, spanCtx := tracing.SpanFromContext(ctx, spanNameLLMRequest, tracing.SpanKindClient)
	start := time.Now()
	logger := tracing.NewTraceLogger(span, slog.Default())
	attrs := []tracing.Attribute{
		{Key: "provider", Value: m.provider.name},
		{Key: "model", Value: m.cfg.Model},
	}
	span.SetAttributes(attrs...)

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
	latency := time.Since(start)
	span.SetAttributes(tracing.Attribute{Key: "latency_ms", Value: latency.Milliseconds()})
	span.End()

	logger.Info("llm_generate_complete",
		"op", "llm.generate",
		"provider", m.provider.name,
		"model", m.cfg.Model,
		"err", err != nil,
	)
	return msg, err
}

func (m *HTTPChatModel) generate(ctx context.Context, msgs []Message, opts ...Option) (*Message, error) {
	body, err := m.buildBody(msgs, opts...)
	if err != nil {
		return nil, err
	}

	respBody, err := m.roundTrip(ctx, body)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := respBody.Close(); cerr != nil {
			slog.Warn("llm_close_body", "err", cerr)
		}
	}()

	var parsed openAIResponse
	if err := json.NewDecoder(respBody).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("llm: decode response: %w", err)
	}

	msg := &Message{Role: RoleAssistant}
	if len(parsed.Choices) > 0 {
		choice := parsed.Choices[0]
		msg.Content = contentToString(choice.Message.Content)
		msg.Role = RoleAssistant
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

// Stream returns the generation result as a channel of MessageChunk. It sends
// `stream: true` to the provider and parses the `text/event-stream` response
// incrementally, emitting one MessageChunk per SSE `data:` payload (which
// usually arrives as one or more tokens). Tool-call argument fragments are
// accumulated per call across chunks so the final assistant message can be
// reconstructed by the caller. The channel is always closed on success and on
// failure; a non-nil error is returned only for failures detected before the
// connection is established.
func (m *HTTPChatModel) Stream(ctx context.Context, msgs []Message, opts ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, 64)
	slog.Info("llm_stream_start",
		"op", "llm.stream",
		"provider", m.provider.name,
		"model", m.cfg.Model,
		"messages", len(msgs),
	)

	body, err := m.buildBody(msgs, opts...)
	if err != nil {
		close(ch)
		return ch, err
	}

	// Re-marshal with stream=true. buildBody does not expose a streaming flag
	// so we inject it here by decoding and re-encoding.
	var reqMap map[string]any
	if unmarshalErr := json.Unmarshal(body, &reqMap); unmarshalErr != nil {
		close(ch)
		return ch, fmt.Errorf("llm: decode stream request: %w", unmarshalErr)
	}
	reqMap["stream"] = true
	if reqMap["stream_options"] == nil {
		reqMap["stream_options"] = map[string]any{"include_usage": true}
	}
	streamBody, err := json.Marshal(reqMap)
	if err != nil {
		close(ch)
		return ch, fmt.Errorf("llm: marshal stream request: %w", err)
	}

	respBody, err := m.roundTrip(ctx, streamBody)
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
				select {
				case ch <- MessageChunk{Role: RoleAssistant, Content: content}:
				case <-ctx.Done():
					return
				}
			}
			final := MessageChunk{Role: RoleAssistant, Final: true, ToolCalls: toolCalls, FinishReason: finishReason}
			if parsed.Usage != nil {
				final.Usage = &Usage{
					InputTokens:  parsed.Usage.PromptTokens,
					OutputTokens: parsed.Usage.CompletionTokens,
					TotalTokens:  parsed.Usage.PromptTokens + parsed.Usage.CompletionTokens,
				}
			}
			select {
			case ch <- final:
			case <-ctx.Done():
			}
			return
		}

		// SSE path: parse using the shared DefaultSSEParser and accumulate
		// tool-call fragments via the shared helper.
		parser := NewDefaultSSEParser()
		events, _ := parser.Parse(ctx, reader) //nolint:errcheck

		toolCalls, finishReason, usage := accumulateOpenAIStreamToolCalls(ctx, events, ch)
		final := MessageChunk{Role: RoleAssistant, Final: true, ToolCalls: toolCalls, FinishReason: finishReason}
		if usage != nil {
			final.Usage = usage
		}
		select {
		case ch <- final:
		case <-ctx.Done():
		}
	}()

	return ch, nil
}

// openAIStreamChunk is one SSE data payload of a streaming response.
type openAIStreamChunk struct {
	Choices []openAIStreamChoice `json:"choices"`
	// Usage is populated only on the final chunk (empty choices) when
	// stream_options.include_usage is true.
	Usage *openAIUsage `json:"usage,omitempty"`
}

// openAIStreamChoice carries the incremental delta of an OpenAI stream.
type openAIStreamChoice struct {
	Delta struct {
		Content   string           `json:"content"`
		ToolCalls []openAIToolCall `json:"tool_calls"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason,omitempty"`
}

// buildBody serializes the conversation and options into the OpenAI chat
// completions request body. It is exported internally for testability.
func (m *HTTPChatModel) buildBody(msgs []Message, opts ...Option) ([]byte, error) {
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
		// Forward assistant tool_calls so the API can match them to
		// subsequent tool_result messages.
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
		Model:     m.cfg.Model,
		Messages:  reqMsgs,
		MaxTokens: m.cfg.MaxTokens,
	}
	if genOpts.Temperature != nil {
		req.Temperature = *genOpts.Temperature
	} else {
		req.Temperature = m.cfg.Temperature
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

// roundTrip builds the HTTP request, executes it against the provider, and
// verifies a 2xx status. It returns the response body for decoding and closes
// it via the deferred call in the caller.
func (m *HTTPChatModel) roundTrip(ctx context.Context, body []byte) (io.ReadCloser, error) {
	baseURL := m.cfg.BaseURL
	if baseURL == "" {
		baseURL = m.provider.defaultBaseURL
	}
	endpoint := strings.TrimRight(baseURL, "/") + chatCompletionsPath

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	apiKey := m.cfg.APIKey
	if apiKey == "" && m.provider.defaultAPIKeySource != nil {
		apiKey = m.provider.defaultAPIKeySource()
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
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
		// Truncate the error payload to avoid leaking large response bodies
		// (which may contain sensitive data) into logs and error chains.
		truncated := strings.TrimSpace(string(payload))
		if len(truncated) > 512 {
			truncated = truncated[:512] + "..."
		}
		return nil, fmt.Errorf("llm: provider returned %s: %s", resp.Status, truncated)
	}
	return resp.Body, nil
}

// openAIRequest is the OpenAI chat completions request body.
type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Temperature float64         `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	Tools       []openAIToolDef `json:"tools,omitempty"`
	ToolChoice  any             `json:"tool_choice,omitempty"`
}

// openAIToolDef is the OpenAI tool definition format.
type openAIToolDef struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

// openAIToolFunction names a tool and describes its parameters.
type openAIToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// openAIMessage is a single message in an OpenAI chat request/response.
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"` // string or []openAIContentPart
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIContentPart is a single typed part within a multimodal message
// (text or image_url).
type openAIContentPart struct {
	Type     string          `json:"type"` // "text" or "image_url"
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

// openAIImageURL wraps an image URL or data URI for the OpenAI vision API.
type openAIImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// contentToString extracts the text from an openAIMessage.Content value, which
// is `any` (string or []openAIContentPart). For assistant responses the content
// is always a plain string; this helper safely coerces it back.
func contentToString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// buildOpenAIContent returns the value to assign to openAIMessage.Content. When
// the message has ContentBlocks, it builds a []openAIContentPart slice; otherwise
// it returns the plain Content string (backward compatible).
func buildOpenAIContent(msg Message) any {
	if msg.ContentBlocks == nil {
		return msg.Content
	}
	parts := make([]openAIContentPart, 0, len(msg.ContentBlocks))
	for _, cb := range msg.ContentBlocks {
		switch cb.Type {
		case "text":
			parts = append(parts, openAIContentPart{Type: "text", Text: cb.Text})
		case "image_url":
			if cb.ImageURL != nil {
				parts = append(parts, openAIContentPart{
					Type: "image_url",
					ImageURL: &openAIImageURL{
						URL:    cb.ImageURL.URL,
						Detail: cb.ImageURL.Detail,
					},
				})
			}
		}
	}
	return parts
}

// openAIResponse is the OpenAI chat completions response body.
type openAIResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   *openAIUsage   `json:"usage,omitempty"`
}

// openAIChoice is one completion choice.
type openAIChoice struct {
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason,omitempty"`
}

// openAIUsage reports token consumption.
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// openAIToolCall is an assistant tool invocation request. In streaming
// responses the Index field identifies which tool call this fragment belongs
// to so arguments can be accumulated across chunks.
type openAIToolCall struct {
	Index    *int           `json:"index,omitempty"`
	ID       string         `json:"id"`
	Type     string         `json:"type,omitempty"`
	Function openAIFunction `json:"function"`
}

// openAIFunction names the tool and holds its JSON-encoded arguments.
type openAIFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// convertAssistantToolCalls maps OpenAI tool calls onto llm.ToolCall values,
// decoding the JSON-encoded arguments into a generic map.
func convertAssistantToolCalls(tc []openAIToolCall) []ToolCall {
	if len(tc) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(tc))
	for _, call := range tc {
		var args any
		if call.Function.Arguments != "" {
			var decoded any
			if err := json.Unmarshal([]byte(call.Function.Arguments), &decoded); err == nil {
				args = decoded
			} else {
				args = call.Function.Arguments
			}
		}
		out = append(out, ToolCall{ID: call.ID, Name: call.Function.Name, Args: args})
	}
	return out
}
