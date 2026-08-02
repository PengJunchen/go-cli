package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	assert.Equal(t, 0.9, req.Temperature)
	assert.Equal(t, 9, req.MaxTokens)
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

// TestDecodeClaudeResponse_JoinsText verifies multiple text blocks are joined
// and non-text blocks are skipped.
func TestDecodeClaudeResponse_JoinsText(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	msg, err := decodeClaudeResponse([]byte(`{"content":[
		{"type":"text","text":"a"},
		{"type":"tool_use","id":"t1"},
		{"type":"text","text":"b"}
	]}`))
	require.NoError(t, err)
	assert.Equal(t, "ab", msg.Content)
	assert.Empty(t, msg.ToolCalls)
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
	msg, err := decodeClaudeResponse([]byte(`{"content":[{"type":"text","text":"x"}]}`))
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
	m := &nativeChatModel{
		client:   http.DefaultClient,
		provider: "x",
		model:    "m",
		encode: func(_ []Message, _ []Option) ([]byte, error) {
			return nil, errors.New("llm: encode boom")
		},
		decode: func(_ []byte) (*Message, error) { return nil, nil },
	}
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "encode boom")
}

// TestNativeChatModelGenerate_Non2xx verifies non-2xx surfaces the payload.
func TestNativeChatModelGenerate_Non2xx(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeResponse(t, w, `player-error-payload`)
	}))
	defer srv.Close()

	m := &nativeChatModel{
		client:   http.DefaultClient,
		endpoint: srv.URL,
		provider: "x",
		model:    "m",
		header:   func() map[string]string { return nil },
		encode:   func(_ []Message, _ []Option) ([]byte, error) { return []byte("{}"), nil },
		decode:   func(_ []byte) (*Message, error) { return nil, nil },
	}
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "player-error-payload")
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
		client:   http.DefaultClient,
		endpoint: srv.URL,
		provider: "x",
		model:    "m",
		encode:   func(_ []Message, _ []Option) ([]byte, error) { return []byte("{}"), nil },
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
		client:   http.DefaultClient,
		endpoint: srv.URL,
		provider: "x",
		model:    "m",
		encode:   func(_ []Message, _ []Option) ([]byte, error) { return []byte("{}"), nil },
		decode:   func(_ []byte) (*Message, error) { return &Message{Role: RoleAssistant}, nil },
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
		endpoint: "http://127.0.0.1:1/chat/completions",
		provider: "x",
		model:    "m",
		encode:   func(_ []Message, _ []Option) ([]byte, error) { return []byte("{}"), nil },
		decode:   func(_ []byte) (*Message, error) { return nil, nil },
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
		writeResponse(t, w, `{"content":[{"type":"text","text":"ok"}]}`)
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
