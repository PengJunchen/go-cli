package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// EinoProvider.Build hardening
// ---------------------------------------------------------------------------

// TestEinoProviderBuild_ErrorWhenNoModel verifies Build fails only when both the
// config model and the provider default are empty. Construction is done
// directly to force an empty defaultModel, which the exported builder prevents.
func TestEinoProviderBuild_ErrorWhenNoModel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	p := &EinoProvider{defaultModel: ""}
	model, cleanup, err := p.Build(context.Background(), ModelConfig{})
	require.Error(t, err)
	assert.Nil(t, model)
	assert.Nil(t, cleanup)
	assert.Contains(t, err.Error(), "non-empty model name")
}

// TestEinoProviderBuild_ResolvesCfgModel verifies an explicit cfg.Model wins
// over the provider default and is written back to the model's cfg.
func TestEinoProviderBuild_ResolvesCfgModel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	p := NewEinoProvider(WithDefaultModel("default-name"))
	m, _, err := p.Build(context.Background(), ModelConfig{Model: "explicit-name", APIKey: "k", BaseURL: "http://b"})
	require.NoError(t, err)
	hm, ok := m.(*HTTPChatModel)
	require.True(t, ok)
	assert.Equal(t, "explicit-name", hm.cfg.Model)
	assert.Equal(t, "k", hm.cfg.APIKey)
	assert.Equal(t, "http://b", hm.cfg.BaseURL)

	// When Model is empty the provider default is used.
	m2, _, err := p.Build(context.Background(), ModelConfig{})
	require.NoError(t, err)
	hm2, ok := m2.(*HTTPChatModel)
	require.True(t, ok)
	assert.Equal(t, "default-name", hm2.cfg.Model)
}

// ---------------------------------------------------------------------------
// HTTPChatModel.generate internals
// ---------------------------------------------------------------------------

// TestHTTPChatModel_roundTripErrorBody verifies a non-2xx response surfaces the
// trimmed payload as an error.
func TestHTTPChatModel_roundTripErrorBody(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "  rate limited  ") //nolint:errcheck // http test body write
	}))
	defer srv.Close()

	m := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limited")
	assert.Contains(t, err.Error(), "429")
}

// TestHTTPChatModel_roundTripRequestBuildError verifies an unparseable endpoint
// produces a request-build error. The scheme must be invalid for
// http.NewRequestWithContext to fail.
func TestHTTPChatModel_roundTripRequestBuildError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	// "http://[::1" is an unterminated bracketed host so
	// http.NewRequestWithContext fails during request construction.
	m := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL("http://[::1")),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build request")
}

// TestHTTPChatModel_buildBodyToolMessages verifies tool input messages are
// serialized with their ToolCallID and Name preserved in the request body.
func TestHTTPChatModel_buildBodyToolMessages(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := &HTTPChatModel{
		provider: NewEinoProvider(),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	body, err := m.buildBody([]Message{
		{Role: RoleUser, Content: "hi", Name: "tool1"},
		{Role: RoleTool, ToolCallID: "call_9", Content: "result"},
		{Role: RoleSystem, Content: "sys"},
	}, WithStopStrings("x"))
	require.NoError(t, err)

	var req openAIRequest
	require.NoError(t, json.Unmarshal(body, &req))
	require.Len(t, req.Messages, 3)
	assert.Equal(t, "user", req.Messages[0].Role)
	assert.Equal(t, "tool1", req.Messages[0].Name)
	assert.Equal(t, "tool", req.Messages[1].Role)
	assert.Equal(t, "call_9", req.Messages[1].ToolCallID)
	assert.Equal(t, "system", req.Messages[2].Role)
	assert.Equal(t, []string{"x"}, req.Stop)
}

// TestHTTPChatModel_GenerateUsageTokens verifies a successful generation fills
// the message Usage from the OpenAI usage block in the response.
func TestHTTPChatModel_GenerateUsageTokens(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{
			"choices":[{"message":{"role":"assistant","content":" hello "}}],
			"usage":{"prompt_tokens":10,"completion_tokens":7}
		}`
		_, _ = io.WriteString(w, body) //nolint:errcheck // http test body write
	}))
	defer srv.Close()

	provider := NewEinoProvider(WithBaseURL(srv.URL))
	m, _, err := provider.Build(context.Background(), ModelConfig{Model: "m", APIKey: "k"})
	require.NoError(t, err)

	msg, err := m.Generate(context.Background(), []Message{{Role: RoleUser, Content: "x"}})
	require.NoError(t, err)
	assert.Equal(t, RoleAssistant, msg.Role)
	assert.Equal(t, " hello ", msg.Content)
	require.NotNil(t, msg.Usage)
	assert.Equal(t, 10, msg.Usage.InputTokens)
	assert.Equal(t, 7, msg.Usage.OutputTokens)
	assert.Equal(t, 17, msg.Usage.TotalTokens)
}

// TestHTTPChatModel_UsageOverrides verifies per-call options override config
// temperature / max tokens for the HTTPChatModel.
func TestHTTPChatModel_UsageOverrides(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := &HTTPChatModel{
		provider: NewEinoProvider(),
		cfg:      ModelConfig{Model: "m", Temperature: 0.1, MaxTokens: 3},
		client:   http.DefaultClient,
	}
	body, err := m.buildBody([]Message{{Role: RoleUser, Content: "x"}}, WithTemperature(0.9), WithMaxTokens(42))
	require.NoError(t, err)
	var req openAIRequest
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, 0.9, req.Temperature)
	assert.Equal(t, 42, req.MaxTokens)
}

// TestHTTPChatModel_StreamSuccess verifies Stream delivers the full content as
// a single chunk and closes the channel.
func TestHTTPChatModel_StreamSuccess(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"streamed!"}}]}`) //nolint:errcheck // http test body write
	}))
	defer srv.Close()

	provider := NewEinoProvider(WithBaseURL(srv.URL))
	m, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := m.Stream(context.Background(), []Message{{Role: RoleUser, Content: "x"}})
	require.NoError(t, err)
	chunk, ok := <-ch
	require.True(t, ok)
	assert.Equal(t, RoleAssistant, chunk.Role)
	assert.Equal(t, "streamed!", chunk.Content)
	_, ok = <-ch
	assert.False(t, ok, "stream channel must be closed after a single success")
}

// ---------------------------------------------------------------------------
// Tracing integration: every generation emits an "llm.request" span
// ---------------------------------------------------------------------------

// TestHTTPChatModel_EmitsRequestSpan verifies HTTPChatModel.Generate emits an
// "llm.request" span with provider/model and, on success, token attributes when
// a Tracer is present in the context.
func TestHTTPChatModel_EmitsRequestSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{
			"choices":[{"message":{"content":"ok"}}],
			"usage":{"prompt_tokens":5,"completion_tokens":6}
		}`
		_, _ = io.WriteString(w, body) //nolint:errcheck // http test body write
	}))
	defer srv.Close()

	ctx, exporter := newComposeTestCtx(t)
	p := NewEinoProvider(WithBaseURL(srv.URL), WithProviderName("httpprov"))
	m, _, err := p.Build(ctx, ModelConfig{Model: "m"})
	require.NoError(t, err)

	_, err = m.Generate(ctx, []Message{{Role: RoleUser, Content: "x"}})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, ok := exporter.spanByName("llm.request")
		return ok
	}, 2*time.Second, 5*time.Millisecond)

	span, ok := exporter.spanByName("llm.request")
	require.True(t, ok)
	assert.Equal(t, "compose-trace", span.TraceID)
	assert.Equal(t, string(tracing.SpanKindClient), string(span.SpanKind))
	prov, found := findAttr(span, "provider")
	require.True(t, found)
	assert.Equal(t, "httpprov", prov)
	model, found := findAttr(span, "model")
	require.True(t, found)
	assert.Equal(t, "m", model)
	in, found := findAttr(span, "tokens_input")
	require.True(t, found)
	assert.Equal(t, 5, in)
	out, found := findAttr(span, "tokens_output")
	require.True(t, found)
	assert.Equal(t, 6, out)
}

// TestHTTPChatModel_EmitsErrorRequestSpan verifies a failing generate marks the
// resulting "llm.request" span as an error with a status message.
func TestHTTPChatModel_EmitsErrorRequestSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "bad") //nolint:errcheck // http test body write
	}))
	defer srv.Close()

	ctx, exporter := newComposeTestCtx(t)
	p := NewEinoProvider(WithBaseURL(srv.URL))
	m, _, err := p.Build(ctx, ModelConfig{Model: "m"})
	require.NoError(t, err)

	_, err = m.Generate(ctx, nil)
	require.Error(t, err)

	require.Eventually(t, func() bool {
		_, ok := exporter.spanByName("llm.request")
		return ok
	}, 2*time.Second, 5*time.Millisecond)
	span, ok := exporter.spanByName("llm.request")
	require.True(t, ok)
	assert.Equal(t, string(tracing.SpanStatusError), string(span.Status))
	assert.NotEmpty(t, span.StatusMessage)
}

// TestNativeChatModel_EmitsRequestSpan verifies the shared nativeChatModel also
// emits an "llm.request" span when a Tracer is present.
func TestNativeChatModel_EmitsRequestSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`) //nolint:errcheck // http test body write
	}))
	defer srv.Close()

	ctx, exporter := newComposeTestCtx(t)
	p := NewGeminiProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("k"))
	m, _, err := p.Build(ctx, ModelConfig{Model: "m"})
	require.NoError(t, err)

	_, err = m.Generate(ctx, []Message{{Role: RoleUser, Content: "x"}})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, ok := exporter.spanByName("llm.request")
		return ok
	}, 2*time.Second, 5*time.Millisecond)
	span, ok := exporter.spanByName("llm.request")
	require.True(t, ok)
	assert.Equal(t, "compose-trace", span.TraceID)
	prov, found := findAttr(span, "provider")
	require.True(t, found)
	assert.Equal(t, "gemini", prov)
}

// ---------------------------------------------------------------------------
// Native provider Build resolution
// ---------------------------------------------------------------------------

// TestClaudeProviderBuild_ResolvesDefaults verifies Claude fills the default
// model and base URL when cfg is empty.
func TestClaudeProviderBuild_ResolvesDefaults(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	p := NewClaudeProvider()
	assert.Equal(t, claudeProviderName, p.Name())
	model, _, err := p.Build(context.Background(), ModelConfig{})
	require.NoError(t, err)
	cm, ok := model.(*nativeChatModel)
	require.True(t, ok)
	assert.Equal(t, claudeDefaultModel, cm.model)
	assert.True(t, strings.Contains(cm.endpoint, claudeMessagesPath), "claude endpoint must use /messages")
}

// TestGeminiProviderBuild_EndpointEmbedsModel verifies Gemini embeds the
// resolved model in the request path.
func TestGeminiProviderBuild_EndpointEmbedsModel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	p := NewGeminiProvider(WithNativeBaseURL("https://example.test"))
	assert.Equal(t, geminiProviderName, p.Name())
	model, _, err := p.Build(context.Background(), ModelConfig{Model: "flash-test"})
	require.NoError(t, err)
	gm, ok := model.(*nativeChatModel)
	require.True(t, ok)
	assert.Equal(t, "https://example.test/v1beta/models/flash-test:generateContent", gm.endpoint)
}

// TestNativeProviderBuild_ErrorsWhenNoModel verifies each native provider's
// Build rejects an empty model when no default is configured.
func TestNativeProviderBuild_ErrorsWhenNoModel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	cases := []struct {
		name string
		p    ModelProvider
	}{
		{"openai", &OpenAIProvider{}},
		{"claude", &ClaudeProvider{}},
		{"gemini", &GeminiProvider{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, cleanup, err := tc.p.Build(context.Background(), ModelConfig{})
			require.Error(t, err)
			assert.Nil(t, model)
			assert.Nil(t, cleanup)
		})
	}
}

// TestDecodeOpenAIResponse_Usage verifies the OpenAI usage decoding path maps
// prompt/completion tokens onto llm.Usage.
func TestDecodeOpenAIResponse_Usage(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	msg, err := decodeOpenAIResponse([]byte(`{
		"choices":[{"message":{"content":"hi"}}],
		"usage":{"prompt_tokens":3,"completion_tokens":4}
	}`))
	require.NoError(t, err)
	require.NotNil(t, msg.Usage)
	assert.Equal(t, 3, msg.Usage.InputTokens)
	assert.Equal(t, 4, msg.Usage.OutputTokens)
	assert.Equal(t, 7, msg.Usage.TotalTokens)
}

// TestErrDefaultModel verifies the default-model sentinel is stable.
func TestErrDefaultModel(t *testing.T) {
	err := errDefaultModel
	require.Error(t, err)
	assert.Equal(t, "llm: default model not implemented", err.Error())
	// A second identifier is not needed; ensure it round-trips through errors.New.
	assert.True(t, errors.Is(err, errDefaultModel))
}
