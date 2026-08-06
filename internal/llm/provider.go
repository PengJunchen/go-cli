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
		msg.Content = choice.Message.Content
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

		// Peek at the first bytes to detect SSE vs plain JSON.
		isJSON := false
		peek, err := reader.Peek(64)
		if err != nil && err != io.EOF {
			slog.Error("llm_stream_peek_error", "err", err)
			return
		}
		// Trim leading whitespace to find the first meaningful character.
		trimmed := bytes.TrimLeft(peek, " \t\r\n")
		if len(trimmed) > 0 && trimmed[0] == '{' {
			isJSON = true
		}

		if isJSON {
			// Server returned a regular JSON response instead of SSE.
			body, err := io.ReadAll(reader)
			if err != nil {
				slog.Error("llm_stream_json_read_error", "err", err)
				return
			}
			var parsed openAIResponse
			if err := json.Unmarshal(body, &parsed); err != nil {
				slog.Error("llm_stream_json_parse_error", "err", err)
				return
			}
			var content string
			var toolCalls []ToolCall
			if len(parsed.Choices) > 0 {
				content = parsed.Choices[0].Message.Content
				toolCalls = convertAssistantToolCalls(parsed.Choices[0].Message.ToolCalls)
			}
			if content != "" {
				ch <- MessageChunk{Role: RoleAssistant, Content: content}
			}
			ch <- MessageChunk{Role: RoleAssistant, Final: true, ToolCalls: toolCalls}
			return
		}

		// SSE path: parse data: lines from the buffered reader.
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)

		// Per-request accumulation: tool_call index → name + args fragments.
		var toolNameByIndex map[int]string
		var toolArgsBuf []string
		emittedRole := false

		for scanner.Scan() {
			line := scanner.Text()
			line = strings.TrimSpace(line)
			if line == "" || !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			// SSE termination sentinel.
			if payload == "[DONE]" {
				break
			}

			var chunk openAIStreamChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				slog.Warn("llm_stream_parse_skip", "err", err)
				continue
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			delta := chunk.Choices[0].Delta

			if !emittedRole {
				// Emit a role-only chunk early so the loop can start
				// rendering the assistant message placeholder.
				ch <- MessageChunk{Role: RoleAssistant, Content: ""}
				emittedRole = true
			}

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

		if scanErr := scanner.Err(); scanErr != nil {
			slog.Error("llm_stream_scan_error", "err", scanErr)
		}

		// Fallback: if no SSE lines were found, try to parse the response
		// as JSON. This handles edge cases where the peek didn't work correctly.
		if !emittedRole {
			body, readErr := io.ReadAll(reader)
			if readErr != nil {
				slog.Error("llm_stream_fallback_read_error", "err", readErr)
			} else {
				var parsed openAIResponse
				if unmarshalErr := json.Unmarshal(body, &parsed); unmarshalErr != nil {
					slog.Error("llm_stream_fallback_parse_error", "err", unmarshalErr)
				} else {
					var content string
					var toolCalls []ToolCall
					if len(parsed.Choices) > 0 {
						content = parsed.Choices[0].Message.Content
						toolCalls = convertAssistantToolCalls(parsed.Choices[0].Message.ToolCalls)
					}
					if content != "" {
						ch <- MessageChunk{Role: RoleAssistant, Content: content}
					}
					ch <- MessageChunk{Role: RoleAssistant, Final: true, ToolCalls: toolCalls}
					return
				}
			}
		}

		// Emit the final accumulated assistant message so callers can build
		// the complete Message including tool calls.
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

// openAIStreamChunk is one SSE data payload of a streaming response.
type openAIStreamChunk struct {
	Choices []openAIStreamChoice `json:"choices"`
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
			Content: msg.Content,
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
		return nil, fmt.Errorf("llm: provider returned %s: %s", resp.Status, strings.TrimSpace(string(payload)))
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
	Content    string           `json:"content"`
	Name       string           `json:"name,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
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
