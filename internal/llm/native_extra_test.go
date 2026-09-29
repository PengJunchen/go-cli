package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// ---------------------------------------------------------------------------
// Options and provider basics
// ---------------------------------------------------------------------------

// TestNativeProviderOptions verifies the WithNative* option setters, including
// the HTTP client override which is otherwise unused in tests.
func TestNativeProviderOptions(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	customClient := &http.Client{Timeout: 3 * time.Second}

	p := NewOpenAIProvider(
		WithNativeBaseURL("http://custom"),
		WithNativeAPIKey("custom-key"),
		WithNativeHTTPClient(customClient),
		WithNativeModels([]ModelInfo{{Name: "m1"}}),
	)
	assert.Equal(t, "http://custom", p.defaultBaseURL)
	assert.Equal(t, "custom-key", p.defaultAPIKey)
	assert.Same(t, customClient, p.httpClient)
	require.Len(t, p.Models(), 1)
	assert.Equal(t, "m1", p.Models()[0].Name)

	// Base URL and API key resolution prefer the model config when set.
	assert.Equal(t, "http://from-cfg", p.resolveBaseURL(ModelConfig{BaseURL: "http://from-cfg"}))
	assert.Equal(t, "http://custom", p.resolveBaseURL(ModelConfig{}))
	assert.Equal(t, "kc1", p.resolveAPIKey(ModelConfig{APIKey: "kc1"}))
	assert.Equal(t, "custom-key", p.resolveAPIKey(ModelConfig{}))
}

// TestNativeProviderModels_Copy verifies Models returns a fresh copy each call.
func TestNativeProviderModels_Copy(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	p := NewOpenAIProvider(WithNativeModels([]ModelInfo{{Name: "keep"}}))
	models := p.Models()
	models[0].Name = "mutated"
	require.NotEqual(t, "mutated", p.Models()[0].Name)
}

// TestNativeProviderBuildInvalid_DefaultPresent verifies that when a default
// model is present an empty cfg.Model is filled, not rejected.
func TestNativeProviderBuildInvalid_DefaultPresent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	p := NewOpenAIProvider() // default model = openaiDefaultModel
	model, _, err := p.Build(context.Background(), ModelConfig{})
	require.NoError(t, err)
	nm, ok := model.(*nativeChatModel)
	require.True(t, ok)
	assert.Equal(t, openaiDefaultModel, nm.model)
}

// ---------------------------------------------------------------------------
// Encode / decode helpers (no network needed)
// ---------------------------------------------------------------------------

// TestEncodeOpenAIRequest_Options verifies option overrides flow into the
// serialized request and stop strings / tool ids are honored.
func TestEncodeOpenAIRequest_Options(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	body, err := encodeOpenAIRequest(ModelConfig{Model: "m", Temperature: 0.2}, "resolved", []Message{
		{Role: RoleUser, Content: "hi"},
		{Role: RoleTool, ToolCallID: "call_1", Content: "res"},
	}, []Option{WithMaxTokens(9), WithTemperature(0.9)})
	require.NoError(t, err)

	var req openAIRequest
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "resolved", req.Model)
	require.NotNil(t, req.Temperature)
	assert.Equal(t, 0.9, *req.Temperature)
	require.NotNil(t, req.MaxTokens)
	assert.Equal(t, 9, *req.MaxTokens)
	require.Len(t, req.Messages, 2)
	assert.Equal(t, "tool", req.Messages[1].Role)
	assert.Equal(t, "call_1", req.Messages[1].ToolCallID)
}

// TestEncodeOpenAIRequest_StopStrings verifies stop strings survive serialization.
func TestEncodeOpenAIRequest_StopStrings(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	body, err := encodeOpenAIRequest(ModelConfig{}, "m", nil, []Option{WithStopStrings("A", "B")})
	require.NoError(t, err)
	var req openAIRequest
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, []string{"A", "B"}, req.Stop)
	assert.Empty(t, req.Messages)
}

// TestDecodeOpenAIResponse_Garbage verifies a non-JSON body surfaces an error.
func TestDecodeOpenAIResponse_Garbage(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	_, err := decodeOpenAIResponse([]byte("not json"))
	require.Error(t, err)
}

// TestDecodeOpenAIResponse_ToolCallNonJSONArgs verifies tool-call args that
// fail to decode as JSON are preserved as the raw string.
func TestDecodeOpenAIResponse_ToolCallNonJSONArgs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	msg, err := decodeOpenAIResponse([]byte(`{"choices":[{"message":{"tool_calls":[
		{"id":"c1","function":{"name":"f","arguments":"not-json{broken"}}
	]}}]}`))
	require.NoError(t, err)
	require.Len(t, msg.ToolCalls, 1)
	raw, ok := msg.ToolCalls[0].Args.(string)
	require.True(t, ok)
	assert.Equal(t, "not-json{broken", raw)
}

// TestDecodeClaudeResponse_JoinsText verifies text blocks are joined and a
// canonical following tool-use block is preserved.
func TestDecodeClaudeResponse_JoinsText(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	msg, err := decodeClaudeResponse([]byte(`{"role":"assistant","content":[
		{"type":"text","text":"a"},
		{"type":"text","text":"b"},
		{"type":"tool_use","id":"t1","name":"lookup","input":{"count":1}}
	],"stop_reason":"tool_use"}`))
	require.NoError(t, err)
	assert.Equal(t, "ab", msg.Content)
	require.Equal(t, []ToolCall{{ID: "t1", Name: "lookup", Args: map[string]any{"count": json.Number("1")}}}, msg.ToolCalls)
}

// TestEncodeClaudeRequest_MaxTokensSources verifies max_tokens priority:
// option > cfg > default.
func TestEncodeClaudeRequest_MaxTokensSources(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Option overrides everything.
	body, err := encodeClaudeRequest(ModelConfig{MaxTokens: 2048}, "m", nil, []Option{WithMaxTokens(512)})
	require.NoError(t, err)
	var req claudeRequest
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, 512, req.MaxTokens)

	// cfg.MaxTokens is used when no option.
	body, err = encodeClaudeRequest(ModelConfig{MaxTokens: 2048}, "m", nil, nil)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, 2048, req.MaxTokens)

	// Default when neither is set.
	body, err = encodeClaudeRequest(ModelConfig{}, "m", nil, nil)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, claudeDefaultMaxTokens, req.MaxTokens)

	// System messages are merged.
	body, err = encodeClaudeRequest(ModelConfig{}, "m", []Message{
		{Role: RoleSystem, Content: "S1"},
		{Role: RoleUser, Content: "U"},
		{Role: RoleSystem, Content: "S2"},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "S1\nS2", req.System)
	require.Len(t, req.Messages, 1)
	assert.Equal(t, "user", req.Messages[0].Role)
}

// TestDecodeClaudeResponse_NoUsage verifies a response without usage is fine.
func TestDecodeClaudeResponse_NoUsage(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	msg, err := decodeClaudeResponse([]byte(`{"role":"assistant","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn"}`))
	require.NoError(t, err)
	assert.Equal(t, "x", msg.Content)
	assert.Nil(t, msg.Usage)
}

// TestEncodeGeminiRequest_SystemToolToUser verifies system and tool roles are
// folded into user, and generation config is conditionally emitted.
func TestEncodeGeminiRequest_SystemToolToUser(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	cfg := ModelConfig{Temperature: 0.4, MaxTokens: 123}
	body, err := encodeGeminiRequest(cfg, []Message{
		{Role: RoleSystem, Content: "sys"},
		{Role: RoleTool, Content: "tool-result"},
		{Role: RoleAssistant, Content: "assistant"},
	}, nil)
	require.NoError(t, err)
	var req geminiRequest
	require.NoError(t, json.Unmarshal(body, &req))
	require.Len(t, req.Contents, 3)
	assert.Equal(t, "user", req.Contents[0].Role)
	assert.Equal(t, "user", req.Contents[1].Role)
	assert.Equal(t, "assistant", req.Contents[2].Role) // assistant role is left verbatim
	require.NotNil(t, req.GenerationConfig)
	assert.Equal(t, 0.4, req.GenerationConfig.Temperature)
	assert.Equal(t, 123, req.GenerationConfig.MaxOutputTokens)

	// No generation-config-driving fields => nil config.
	body, err = encodeGeminiRequest(ModelConfig{}, []Message{{Role: RoleUser, Content: "x"}}, nil)
	require.NoError(t, err)
	var req2 geminiRequest
	require.NoError(t, json.Unmarshal(body, &req2))
	assert.Nil(t, req2.GenerationConfig)
}

// TestDecodeGeminiResponse_EmptyCandidates verifies missing candidates yields
// an assistant message with empty content, not an error.
func TestDecodeGeminiResponse_EmptyCandidates(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	msg, err := decodeGeminiResponse([]byte(`{"candidates":[]}`))
	require.NoError(t, err)
	assert.Equal(t, RoleAssistant, msg.Role)
	assert.Equal(t, "", msg.Content)
	assert.Nil(t, msg.Usage)
}

// ---------------------------------------------------------------------------
// Network-level error paths on the shared native helper
// ---------------------------------------------------------------------------

// TestNativeChatModel_GenerateCanceledContext verifies a canceled context
// surfaces before/around the HTTP round trip.
func TestNativeChatModel_GenerateCanceledContext(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	provider := NewOpenAIProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = model.Generate(ctx, nil)
	require.Error(t, err)
}

// TestNativeChatModelGenerate_EncodeError verifies an encode failure surfaces
// without contacting any server.
func TestNativeChatModelGenerate_EncodeError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	encodeErr := errors.New("llm: encode boom")
	m := &nativeChatModel{
		client:    http.DefaultClient,
		provider:  "x",
		model:     "m",
		buildMode: UnboundLibraryBuild,
		encode: func(_ []Message, _ []Option) ([]byte, error) {
			return nil, encodeErr
		},
		decode: func(_ []byte) (*Message, error) { return nil, nil },
	}
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
	require.ErrorIs(t, err, encodeErr)
	var setupErr *PreDispatchCandidateError
	require.ErrorAs(t, err, &setupErr)
	assert.Equal(t, PreDispatchInvalidRequest, setupErr.Class)
}

// TestNativeChatModelGenerate_Non2xx verifies non-2xx preserves structured
// status metadata but keeps the provider payload out of the error summary.
func TestNativeChatModelGenerate_Non2xx(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeResponse(t, w, `player-error-payload`)
	}))
	defer srv.Close()

	m := &nativeChatModel{
		client:    http.DefaultClient,
		endpoint:  srv.URL,
		provider:  "x",
		model:     "m",
		buildMode: UnboundLibraryBuild,
		header:    func() map[string]string { return nil },
		encode:    func(_ []Message, _ []Option) ([]byte, error) { return []byte("{}"), nil },
		decode:    func(_ []byte) (*Message, error) { return nil, nil },
	}
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "player-error-payload")
	assert.Contains(t, err.Error(), "HTTP 400")
	var providerErr *ProviderError
	require.ErrorAs(t, err, &providerErr)
	assert.Equal(t, "player-error-payload", providerErr.Message)
}

// TestNativeChatModelGenerate_DecodeError verifies a decode failure surfaces.
func TestNativeChatModelGenerate_DecodeError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `garbage`)
	}))
	defer srv.Close()

	m := &nativeChatModel{
		client:    http.DefaultClient,
		endpoint:  srv.URL,
		provider:  "x",
		model:     "m",
		buildMode: UnboundLibraryBuild,
		encode:    func(_ []Message, _ []Option) ([]byte, error) { return []byte("{}"), nil },
		decode: func(_ []byte) (*Message, error) {
			return nil, errors.New("llm: decode boom")
		},
	}
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode boom")
}

// TestNativeChatModel_StreamError verifies Stream closes the channel on error
// and returns the error.
func TestNativeChatModel_StreamError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeResponse(t, w, `err`)
	}))
	defer srv.Close()

	m := &nativeChatModel{
		client:    http.DefaultClient,
		endpoint:  srv.URL,
		provider:  openaiProviderName,
		model:     "m",
		buildMode: UnboundLibraryBuild,
		encode:    func(_ []Message, _ []Option) ([]byte, error) { return []byte("{}"), nil },
		decode:    func(_ []byte) (*Message, error) { return &Message{Role: RoleAssistant}, nil },
	}
	ch, err := m.Stream(context.Background(), nil)
	require.Error(t, err)
	_, ok := <-ch
	assert.False(t, ok, "stream channel must be closed even on error")
}

// TestNativeChatModelGenerate_HTTPDoError verifies a transport error surfaces.
func TestNativeChatModelGenerate_HTTPDoError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := &nativeChatModel{
		client: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("llm: dial boom")
			}),
		},
		endpoint:  "http://127.0.0.1:1/chat/completions",
		provider:  "x",
		model:     "m",
		buildMode: UnboundLibraryBuild,
		encode:    func(_ []Message, _ []Option) ([]byte, error) { return []byte("{}"), nil },
		decode:    func(_ []byte) (*Message, error) { return nil, nil },
	}
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial boom")
}

// roundTripFunc adapts a func to the http.RoundTripper interface.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestNativeChatModel_ParallelGenerate runs concurrent Generate calls under
// -race to verify the shared http.Client is safe.
func TestNativeChatModel_ParallelGenerate(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	provider := NewOpenAIProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, werr := model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "x"}})
			if werr != nil || m == nil || m.Content != "ok" {
				t.Errorf("generate: m=%+v err=%v", m, werr)
			}
		}()
	}
	wg.Wait()
}

// TestNativeChatModelHeaders verifies the provider-specific headers are set.
func TestNativeChatModelHeaders(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var gotAuth, gotVersion, gotGeminiKey, gotClaudeKey string

	oaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer oaSrv.Close()

	clSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("anthropic-version")
		gotClaudeKey = r.Header.Get("x-api-key")
		writeResponse(t, w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer clSrv.Close()

	geSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGeminiKey = r.Header.Get("x-goog-api-key")
		writeResponse(t, w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`)
	}))
	defer geSrv.Close()

	oa, _, err := NewOpenAIProvider(WithNativeBaseURL(oaSrv.URL), WithNativeAPIKey("oa-key")).Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = oa.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "Bearer oa-key", gotAuth)

	cl, _, err := NewClaudeProvider(WithNativeBaseURL(clSrv.URL), WithNativeAPIKey("cl-key")).Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = cl.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, claudeVersionHeader, gotVersion)
	assert.Equal(t, "cl-key", gotClaudeKey)

	ge, _, err := NewGeminiProvider(WithNativeBaseURL(geSrv.URL), WithNativeAPIKey("ge-key")).Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = ge.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "ge-key", gotGeminiKey)

	// A provider without an API key must not send an Authorization header.
	noKeySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer noKeySrv.Close()
	m, _, err := NewOpenAIProvider(WithNativeBaseURL(noKeySrv.URL)).Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "", gotAuth)
}

// ---------------------------------------------------------------------------
// Gemini function-call streaming and decoding
// ---------------------------------------------------------------------------

// TestStreamGemini_FunctionCallAccumulation verifies that a functionCall part
// in a Gemini SSE stream is accumulated into the Final chunk's ToolCalls with
// a synthesized ID and the correct Name/Args.
func TestStreamGemini_FunctionCallAccumulation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sseBody := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Let me check "}]}}]}`,
		"",
		`data: {"candidates":[{"content":{"parts":[{"text":"the weather."}]}}]}`,
		"",
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"get_weather","args":{"city":"SF"}}}]},"finishReason":"STOP"}]}`,
		"",
		`data: {"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewGeminiProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	// Expect: two text chunks + final chunk.
	require.Len(t, chunks, 3)
	assert.Equal(t, "Let me check ", chunks[0].Content)
	assert.Equal(t, "the weather.", chunks[1].Content)

	final := chunks[2]
	assert.True(t, final.Final)
	assert.Equal(t, "STOP", final.FinishReason)
	require.Len(t, final.ToolCalls, 1)

	tc := final.ToolCalls[0]
	assert.True(t, strings.HasPrefix(tc.ID, "call_"), "ID should start with call_, got %s", tc.ID)
	assert.Equal(t, "get_weather", tc.Name)
	args, ok := tc.Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "SF", args["city"])
	require.NotNil(t, final.Usage, "usage-only Gemini frame after terminal candidate must be preserved")
	assert.Equal(t, 5, final.Usage.TotalTokens)
}

func TestStreamGeminiRejectsCandidatesAfterTerminalFinishReason(t *testing.T) {
	for name, latePart := range map[string]string{
		"text":          `{"text":"injected"}`,
		"function call": `{"functionCall":{"name":"late_action","args":{}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			sseBody := strings.Join([]string{
				`data: {"candidates":[{"content":{"parts":[{"text":"complete"}]},"finishReason":"STOP"}]}`,
				"",
				`data: {"candidates":[{"content":{"parts":[` + latePart + `]}}]}`,
				"",
			}, "\n")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeResponse(t, w, sseBody)
			}))
			t.Cleanup(server.Close)

			model, _, err := NewGeminiProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "m"})
			require.NoError(t, err)
			ch, err := model.Stream(context.Background(), nil)
			require.NoError(t, err)
			var chunks []MessageChunk
			for chunk := range ch {
				chunks = append(chunks, chunk)
			}
			require.NotEmpty(t, chunks)
			assert.Equal(t, "complete", chunks[0].Content)
			assertStreamFailedWithoutSuccess(t, chunks)
			for _, chunk := range chunks {
				assert.NotContains(t, chunk.Content, "injected")
				assert.Empty(t, chunk.ToolCalls)
			}
		})
	}
}

// TestStreamGemini_MultipleFunctionCalls verifies that multiple functionCall
// parts in a single response are all accumulated with distinct synthesized IDs.
func TestStreamGemini_MultipleFunctionCalls(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sseBody := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[` +
			`{"functionCall":{"name":"get_weather","args":{"city":"SF"}}},` +
			`{"functionCall":{"name":"get_time","args":{"timezone":"PST"}}}` +
			`]},"finishReason":"STOP"}]}`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewGeminiProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	// Only the final chunk (no text content was emitted).
	require.Len(t, chunks, 1)
	final := chunks[0]
	assert.True(t, final.Final)
	require.Len(t, final.ToolCalls, 2)

	// First tool call.
	tc0 := final.ToolCalls[0]
	assert.True(t, strings.HasPrefix(tc0.ID, "call_"), "ID should start with call_, got %s", tc0.ID)
	assert.Equal(t, "get_weather", tc0.Name)
	args0, ok := tc0.Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "SF", args0["city"])

	// Second tool call.
	tc1 := final.ToolCalls[1]
	assert.True(t, strings.HasPrefix(tc1.ID, "call_"), "ID should start with call_, got %s", tc1.ID)
	assert.Equal(t, "get_time", tc1.Name)
	args1, ok := tc1.Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "PST", args1["timezone"])
}

// TestStreamGemini_TextAndToolCallMixed verifies that text and functionCall
// parts interleaved across stream chunks are all processed correctly.
func TestStreamGemini_TextAndToolCallMixed(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sseBody := strings.Join([]string{
		// First chunk: text only.
		`data: {"candidates":[{"content":{"parts":[{"text":"I'll search "}]}}]}`,
		"",
		// The terminal candidate includes the final text and function call.
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"search","args":{"q":"golang"}}},{"text":"done"}]},"finishReason":"STOP"}]}`,
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewGeminiProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	// Expect: "I'll search ", "done", final chunk with tool call.
	require.Len(t, chunks, 3)
	assert.Equal(t, "I'll search ", chunks[0].Content)
	assert.Equal(t, "done", chunks[1].Content)

	final := chunks[2]
	assert.True(t, final.Final)
	require.Len(t, final.ToolCalls, 1)
	assert.Equal(t, "search", final.ToolCalls[0].Name)
	args, ok := final.ToolCalls[0].Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "golang", args["q"])
}

// TestDecodeGeminiResponse_FunctionCall verifies that a non-streaming
// generateContent response with functionCall parts populates msg.ToolCalls.
func TestDecodeGeminiResponse_FunctionCall(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	body := `{
		"candidates":[{
			"content":{
				"parts":[
					{"text":"Calling tool now."},
					{"functionCall":{"name":"get_weather","args":{"city":"SF","unit":"celsius"}}}
				]
			},
			"finishReason":"STOP"
		}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}
	}`

	msg, err := decodeGeminiResponse([]byte(body))
	require.NoError(t, err)
	assert.Equal(t, RoleAssistant, msg.Role)
	assert.Equal(t, "Calling tool now.", msg.Content)
	assert.Equal(t, "STOP", msg.FinishReason)
	require.Len(t, msg.ToolCalls, 1)

	tc := msg.ToolCalls[0]
	assert.True(t, strings.HasPrefix(tc.ID, "call_"), "ID should start with call_, got %s", tc.ID)
	assert.Equal(t, "get_weather", tc.Name)
	args, ok := tc.Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "SF", args["city"])
	assert.Equal(t, "celsius", args["unit"])
}

// TestDecodeGeminiResponse_MultipleFunctionCalls verifies multiple functionCall
// parts in a non-streaming response.
func TestDecodeGeminiResponse_MultipleFunctionCalls(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	body := `{
		"candidates":[{
			"content":{
				"parts":[
					{"functionCall":{"name":"get_weather","args":{"city":"SF"}}},
					{"functionCall":{"name":"get_time","args":{"tz":"PST"}}}
				]
			},
			"finishReason":"STOP"
		}]
	}`

	msg, err := decodeGeminiResponse([]byte(body))
	require.NoError(t, err)
	assert.Empty(t, msg.Content)
	require.Len(t, msg.ToolCalls, 2)
	assert.Equal(t, "get_weather", msg.ToolCalls[0].Name)
	assert.Equal(t, "get_time", msg.ToolCalls[1].Name)
}

// TestEncodeGeminiRequest_AssistantToolCalls verifies that an assistant message
// with ToolCalls is encoded as functionCall parts.
func TestEncodeGeminiRequest_AssistantToolCalls(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	body, err := encodeGeminiRequest(ModelConfig{}, []Message{
		{Role: RoleUser, Content: "What's the weather?"},
		{
			Role:    RoleAssistant,
			Content: "Let me check.",
			ToolCalls: []ToolCall{
				{Name: "get_weather", Args: map[string]any{"city": "SF"}},
			},
		},
	}, nil)
	require.NoError(t, err)

	var req geminiRequest
	require.NoError(t, json.Unmarshal(body, &req))
	require.Len(t, req.Contents, 2)

	// Assistant content should have a text part followed by a functionCall part.
	assistant := req.Contents[1]
	assert.Equal(t, "assistant", assistant.Role)
	require.Len(t, assistant.Parts, 2)
	assert.Equal(t, "Let me check.", assistant.Parts[0].Text)
	require.NotNil(t, assistant.Parts[1].FunctionCall)
	assert.Equal(t, "get_weather", assistant.Parts[1].FunctionCall.Name)
	assert.Equal(t, "SF", assistant.Parts[1].FunctionCall.Args["city"])
}

// TestEncodeGeminiRequest_ToolResult verifies that a tool result message is
// encoded as a functionResponse part.
func TestEncodeGeminiRequest_ToolResult(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	body, err := encodeGeminiRequest(ModelConfig{}, []Message{
		{Role: RoleUser, Content: "What's the weather?"},
		{
			Role:    RoleAssistant,
			Content: "",
			ToolCalls: []ToolCall{
				{Name: "get_weather", Args: map[string]any{"city": "SF"}},
			},
		},
		{
			Role:    RoleTool,
			Name:    "get_weather",
			Content: `{"temperature":72,"condition":"sunny"}`,
		},
	}, nil)
	require.NoError(t, err)

	var req geminiRequest
	require.NoError(t, json.Unmarshal(body, &req))
	require.Len(t, req.Contents, 3)

	// Tool result message: role folded to "user", single functionResponse part.
	toolMsg := req.Contents[2]
	assert.Equal(t, "user", toolMsg.Role)
	require.Len(t, toolMsg.Parts, 1)
	require.NotNil(t, toolMsg.Parts[0].FunctionResponse)
	assert.Equal(t, "get_weather", toolMsg.Parts[0].FunctionResponse.Name)
	resp := toolMsg.Parts[0].FunctionResponse.Response
	assert.Equal(t, float64(72), resp["temperature"])
	assert.Equal(t, "sunny", resp["condition"])
}

// TestEncodeGeminiRequest_ToolResultNonJSON verifies that a non-JSON tool
// result is wrapped under a "content" key in the functionResponse.
func TestEncodeGeminiRequest_ToolResultNonJSON(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	body, err := encodeGeminiRequest(ModelConfig{}, []Message{
		{
			Role:    RoleTool,
			Name:    "echo",
			Content: "plain text result",
		},
	}, nil)
	require.NoError(t, err)

	var req geminiRequest
	require.NoError(t, json.Unmarshal(body, &req))
	require.Len(t, req.Contents, 1)
	require.Len(t, req.Contents[0].Parts, 1)
	require.NotNil(t, req.Contents[0].Parts[0].FunctionResponse)
	resp := req.Contents[0].Parts[0].FunctionResponse.Response
	assert.Equal(t, "plain text result", resp["content"])
}

// ---------------------------------------------------------------------------
// HTTP timeout (AC-2, AC-3)
// ---------------------------------------------------------------------------

// TestNativeProviderDefaultHTTPTimeout verifies that all three native
// providers set a default 120s timeout on their HTTP client (AC-2).
func TestNativeProviderDefaultHTTPTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	oa := NewOpenAIProvider()
	require.NotNil(t, oa.httpClient)
	assert.Equal(t, nativeDefaultHTTPTimeout, oa.httpClient.Timeout)

	cl := NewClaudeProvider()
	require.NotNil(t, cl.httpClient)
	assert.Equal(t, nativeDefaultHTTPTimeout, cl.httpClient.Timeout)

	ge := NewGeminiProvider()
	require.NotNil(t, ge.httpClient)
	assert.Equal(t, nativeDefaultHTTPTimeout, ge.httpClient.Timeout)
}

// TestNativeGenerateClientTimeout verifies that non-streaming Generate
// requests are terminated by the client timeout (AC-2).
func TestNativeGenerateClientTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	shortTimeoutClient := &http.Client{Timeout: 50 * time.Millisecond}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"late"}}]}`)
	}))
	defer srv.Close()

	provider := NewOpenAIProvider(WithNativeBaseURL(srv.URL), WithNativeHTTPClient(shortTimeoutClient))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	_, err = model.Generate(context.Background(), nil)
	require.Error(t, err)
}

// TestNativeStreamingBypassesClientTimeout verifies that streaming SSE
// connections are NOT terminated by the client-level timeout. A server that
// delays its response body beyond the client timeout should still deliver
// data to the stream, because streamRoundTrip uses a client copy with
// Timeout=0 (AC-2).
func TestNativeStreamingBypassesClientTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	shortTimeoutClient := &http.Client{Timeout: 50 * time.Millisecond}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(200 * time.Millisecond)
		writeResponse(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	provider := NewOpenAIProvider(WithNativeBaseURL(srv.URL), WithNativeHTTPClient(shortTimeoutClient))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var gotContent string
	for c := range ch {
		if c.Content != "" {
			gotContent = c.Content
		}
	}
	assert.Equal(t, "late", gotContent)
}

// TestNativeGenerateContextCancel verifies that cancelling the context
// cancels the HTTP request (AC-3).
func TestNativeGenerateContextCancel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"never"}}]}`)
	}))
	defer srv.Close()

	provider := NewOpenAIProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = model.Generate(ctx, nil)
	require.Error(t, err)
}

func TestClaudeStreamRejectsNonObjectToolArguments(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	events := []string{
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"read"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"null"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}`,
		`{"type":"message_stop"}`,
	}
	sseBody := claudeSSEBody(t, events)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewClaudeProvider(WithNativeBaseURL(srv.URL))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	stream, err := model.Stream(context.Background(), []Message{{Role: RoleUser, Content: "read"}})
	require.NoError(t, err)
	var streamErr error
	for chunk := range stream {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	require.Error(t, streamErr)
}

func TestClaudeStreamRejectsMalformedEventOrdering(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tests := []struct {
		name   string
		events []string
	}{
		{
			name: "delta before message start",
			events: []string{
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"early"}}`,
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "duplicate message start",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "delta without active block",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"unframed"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "delta references another block index",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"wrong block"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "first block index is not zero",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":2,"content_block":{"type":"text"}}`,
				`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"gap"}}`,
				`{"type":"content_block_stop","index":2}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "duplicate block start index",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "block stop references another index",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"text"}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "block stop omits index",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"text"}}`,
				`{"type":"content_block_stop"}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "delta after block stop",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"late"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "duplicate block stop",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "unsupported block type",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "message delta before block stop",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"open"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_stop"}`,
			},
		},
		{
			name: "content block after message delta",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_stop"}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sseBody := claudeSSEBody(t, tt.events)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeResponse(t, w, sseBody)
			}))
			defer srv.Close()

			provider := NewClaudeProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("test"))
			model, _, err := provider.Build(context.Background(), ModelConfig{Model: "claude-test"})
			require.NoError(t, err)
			stream, err := model.Stream(context.Background(), nil)
			require.NoError(t, err)

			var streamErr error
			var sawFinal bool
			for chunk := range stream {
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
				sawFinal = sawFinal || chunk.Final
			}
			require.Error(t, streamErr)
			assert.False(t, sawFinal, "malformed Claude stream must not produce a successful final chunk")
		})
	}
}

func TestClaudeStreamRejectsInterleavedToolAndTextBlocks(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	chunks := collectClaudeStream(t, []string{
		`{"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":4}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"before "}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"read"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"file.txt\"}"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"after"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":8}}`,
		`{"type":"message_stop"}`,
	})

	var gotText strings.Builder
	var streamErr error
	var sawFinal bool
	for i := range chunks {
		if chunks[i].Error != nil {
			streamErr = chunks[i].Error
		}
		if chunks[i].Content != "" {
			gotText.WriteString(chunks[i].Content)
		}
		sawFinal = sawFinal || chunks[i].Final
	}
	require.Error(t, streamErr)
	assert.Equal(t, "before ", gotText.String())
	assert.False(t, sawFinal)
}

func TestClaudeStreamRejectsOutOfOrderInterleavedText(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	chunks := collectClaudeStream(t, []string{
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A1"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"B1"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A2"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`,
		`{"type":"message_stop"}`,
	})

	var gotText strings.Builder
	var streamErr error
	var sawFinal bool
	for _, chunk := range chunks {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
		gotText.WriteString(chunk.Content)
		sawFinal = sawFinal || chunk.Final
	}
	require.Error(t, streamErr)
	assert.Equal(t, "A1", gotText.String(), "the lower-index block's partial text may be visible before rejection")
	assert.False(t, sawFinal)
}

func TestClaudeStreamPreservesTextOrderAfterLowerBlockStops(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	chunks := collectClaudeStream(t, []string{
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"A1"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"B1"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`,
		`{"type":"message_stop"}`,
	})

	var gotText strings.Builder
	var streamErr error
	var final *MessageChunk
	for _, chunk := range chunks {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
		gotText.WriteString(chunk.Content)
		if chunk.Final {
			final = &chunk
		}
	}
	require.NoError(t, streamErr)
	assert.Equal(t, "A1B1", gotText.String())
	require.NotNil(t, final)
}

func TestClaudeStreamAccumulatesMultipleMessageDeltas(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	chunks := collectClaudeStream(t, []string{
		`{"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":3}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":5,"output_tokens":7}}`,
		`{"type":"message_stop"}`,
	})

	var streamErr error
	var final *MessageChunk
	for i := range chunks {
		if chunks[i].Error != nil {
			streamErr = chunks[i].Error
		}
		if chunks[i].Final {
			final = &chunks[i]
		}
	}
	require.NoError(t, streamErr)
	require.NotNil(t, final)
	assert.Equal(t, "end_turn", final.FinishReason)
	require.NotNil(t, final.Usage)
	assert.Equal(t, 5, final.Usage.InputTokens)
	assert.Equal(t, 7, final.Usage.OutputTokens)
	assert.Equal(t, 12, final.Usage.TotalTokens)
}

func TestClaudeStreamExplicitNullStopReasonClearsTerminalState(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	chunks := collectClaudeStream(t, []string{
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
		`{"type":"message_delta","delta":{"stop_reason":null,"stop_sequence":null},"usage":{"output_tokens":8}}`,
		`{"type":"message_stop"}`,
	})

	var streamErr error
	var sawFinal bool
	for _, chunk := range chunks {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
		sawFinal = sawFinal || chunk.Final
	}
	require.Error(t, streamErr)
	assert.False(t, sawFinal, "an explicit null stop_reason must not reuse an earlier terminal reason")
}

func TestClaudeStreamRejectsMessageDeltaWithoutStopReason(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	chunks := collectClaudeStream(t, []string{
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null}}`,
		`{"type":"message_delta","delta":{"stop_sequence":null}}`,
		`{"type":"message_stop"}`,
	})

	var streamErr error
	var sawFinal bool
	for _, chunk := range chunks {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
		sawFinal = sawFinal || chunk.Final
	}
	require.Error(t, streamErr)
	assert.False(t, sawFinal)
}

func TestClaudeStreamRejectsMismatchedSSEEventName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tests := []struct {
		name string
		body string
	}{
		{
			name: "mismatch",
			body: "event: content_block_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n",
		},
		{
			name: "missing name",
			body: "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := collectClaudeStreamBody(t, tt.body)
			var streamErr error
			var sawFinal bool
			for _, chunk := range chunks {
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
				sawFinal = sawFinal || chunk.Final
			}
			require.Error(t, streamErr)
			assert.False(t, sawFinal)
		})
	}
}

func TestClaudeStreamRejectsMissingTerminalEvent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	tests := []struct {
		name   string
		events []string
	}{
		{
			name: "missing message delta",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_stop","index":0}`,
			},
		},
		{
			name: "missing message stop",
			events: []string{
				`{"type":"message_start","message":{"role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := collectClaudeStream(t, tt.events)
			var streamErr error
			var sawFinal bool
			for _, chunk := range chunks {
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
				sawFinal = sawFinal || chunk.Final
			}
			require.Error(t, streamErr)
			assert.False(t, sawFinal)
		})
	}
}

func collectClaudeStream(t *testing.T, events []string) []MessageChunk {
	t.Helper()
	return collectClaudeStreamBody(t, claudeSSEBody(t, events))
}

func collectClaudeStreamBody(t *testing.T, sseBody string) []MessageChunk {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	provider := NewClaudeProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("test"))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := model.Stream(ctx, nil)
	require.NoError(t, err)
	var chunks []MessageChunk
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	return chunks
}

func claudeSSEBody(t *testing.T, events []string) string {
	t.Helper()
	var body strings.Builder
	for _, data := range events {
		var event struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal([]byte(data), &event))
		require.NotEmpty(t, event.Type)
		_, _ = fmt.Fprintf(&body, "event: %s\ndata: %s\n\n", event.Type, data)
	}
	return body.String()
}
