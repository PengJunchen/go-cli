package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// writeResponse writes body to an httptest response and fails the test if the
// write errors. It satisfies errcheck in the test handler goroutines.
func writeResponse(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("write response: %v", err)
	}
}

// TestHTTPChatModelGenerateCannedCompletion verifies that a canned OpenAI-style
// completion is parsed into the expected Message.
func TestHTTPChatModelGenerateCannedCompletion(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/chat/completions", r.URL.Path)
		assert.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		body, rerr := io.ReadAll(r.Body)
		require.NoError(t, rerr)
		var req openAIRequest
		require.NoError(t, json.Unmarshal(body, &req))
		assert.Equal(t, "gpt-4o", req.Model)
		assert.Equal(t, 0.7, req.Temperature)
		require.Len(t, req.Messages, 1)
		assert.Equal(t, "user", req.Messages[0].Role)
		assert.Equal(t, "hello", req.Messages[0].Content)

		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{
			"choices":[{"message":{"role":"assistant","content":"hi there"}}],
			"usage":{"prompt_tokens":5,"completion_tokens":2}
		}`)
	}))
	defer srv.Close()

	provider := NewEinoProvider(
		WithBaseURL(srv.URL),
		WithAPIKey("sk-test"),
	)
	model, cleanup, err := provider.Build(context.Background(), ModelConfig{
		Model:       "gpt-4o",
		Temperature: 0.7,
	})
	require.NoError(t, err)
	require.NotNil(t, cleanup)

	msg, err := model.Generate(context.Background(), []Message{
		{Role: RoleUser, Content: "hello"},
	})
	require.NoError(t, err)
	require.NotNil(t, msg)
	assert.Equal(t, RoleAssistant, msg.Role)
	assert.Equal(t, "hi there", msg.Content)
	require.NotNil(t, msg.Usage)
	assert.Equal(t, 5, msg.Usage.InputTokens)
	assert.Equal(t, 2, msg.Usage.OutputTokens)
	assert.Equal(t, 7, msg.Usage.TotalTokens)
}

// TestHTTPChatModelGenerateToolCall verifies tool_calls are parsed.
func TestHTTPChatModelGenerateToolCall(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{
			"choices":[{"message":{"role":"assistant","content":"",
				"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}
				]}}],
			"usage":{"prompt_tokens":3,"completion_tokens":4}
		}`)
	}))
	defer srv.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	msg, err := model.Generate(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, msg.ToolCalls, 1)
	assert.Equal(t, "call_1", msg.ToolCalls[0].ID)
	assert.Equal(t, "read_file", msg.ToolCalls[0].Name)
	args, ok := msg.ToolCalls[0].Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a.txt", args["path"])
}

// TestHTTPChatModelHTTP500 verifies a non-2xx response surfaces as an error.
func TestHTTPChatModelHTTP500(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		writeResponse(t, w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m", APIKey: "k"},
		client:   http.DefaultClient,
	}
	_, err := model.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
}

// TestHTTPChatModelMalformedJSON verifies a malformed body surfaces as an error.
func TestHTTPChatModelMalformedJSON(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{not-json`)
	}))
	defer srv.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	_, err := model.Generate(context.Background(), nil)
	require.Error(t, err)
}

// TestHTTPChatModelContextCanceled verifies a canceled context surfaces as an
// error before/around the request.
func TestHTTPChatModelContextCanceled(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := model.Generate(ctx, nil)
	require.Error(t, err)
}

// TestHTTPChatModelBaseURLJoin verifies a configured BaseURL with a trailing
// slash is joined correctly to the chat completions path.
func TestHTTPChatModelBaseURLJoin(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(),
		cfg:      ModelConfig{Model: "m", BaseURL: srv.URL + "/"},
		client:   http.DefaultClient,
	}
	_, err := model.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "/chat/completions", gotPath)
}

// TestHTTPChatModelStreamSingleChunk verifies Stream delivers the content and
// closes the channel.
func TestHTTPChatModelStreamSingleChunk(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"streamed"}}]}`)
	}))
	defer srv.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	require.Len(t, chunks, 1)
	assert.Equal(t, RoleAssistant, chunks[0].Role)
	assert.Equal(t, "streamed", chunks[0].Content)
}

// TestBuildBodyOptions verifies per-call options override config defaults.
func TestBuildBodyOptions(t *testing.T) {
	p := NewEinoProvider(WithBaseURL("http://example.com"))
	m := &HTTPChatModel{
		provider: p,
		cfg:      ModelConfig{Model: "m", Temperature: 0.2, MaxTokens: 10},
		client:   http.DefaultClient,
	}
	body, err := m.buildBody([]Message{{Role: RoleUser, Content: "x"}}, WithTemperature(0.9), WithMaxTokens(42), WithStopStrings("END"))
	require.NoError(t, err)

	var req openAIRequest
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, 0.9, req.Temperature)
	assert.Equal(t, 42, req.MaxTokens)
	assert.Equal(t, []string{"END"}, req.Stop)
	assert.Len(t, req.Messages, 1)
}

// TestEinoProviderNameDefault verifies the default name and override.
func TestEinoProviderNameDefault(t *testing.T) {
	assert.Equal(t, "eino", NewEinoProvider().Name())
	assert.Equal(t, "custom", NewEinoProvider(WithProviderName("custom")).Name())
}

// TestEinoProviderModels verifies Models returns a copy and default model list.
func TestEinoProviderModels(t *testing.T) {
	p := NewEinoProvider(WithModels([]ModelInfo{{Name: "gpt-4o", ContextWindow: 128000}}))
	models := p.Models()
	require.Len(t, models, 1)
	assert.Equal(t, "gpt-4o", models[0].Name)
	// Mutating the returned slice must not affect the provider.
	models[0].Name = "mutated"
	assert.Equal(t, "gpt-4o", p.Models()[0].Name)
}

// TestEinoProviderBuildInvalid verifies Build errors on an empty model with no
// default.
func TestEinoProviderBuildInvalid(t *testing.T) {
	p := &EinoProvider{name: "eino", defaultModel: "", httpClient: http.DefaultClient}
	_, _, err := p.Build(context.Background(), ModelConfig{})
	require.Error(t, err)
}

// TestEinoProviderBuildDefaultModel verifies the provider default fills an
// empty model name.
func TestEinoProviderBuildDefaultModel(t *testing.T) {
	p := NewEinoProvider(WithBaseURL("http://example.com"))
	m, _, err := p.Build(context.Background(), ModelConfig{})
	require.NoError(t, err)
	hm, ok := m.(*HTTPChatModel)
	require.True(t, ok)
	assert.Equal(t, defaultProviderModel, hm.cfg.Model)
}

// TestEinoProvider implements ModelProvider contract.
func TestEinoProviderImplementsModelProvider(t *testing.T) {
	var _ ModelProvider = (*EinoProvider)(nil)
	var _ BaseChatModel = (*HTTPChatModel)(nil)

	p := NewEinoProvider()
	buildModel, cleanup, err := p.Build(context.Background(), ModelConfig{Model: "x"})
	require.NoError(t, err)
	require.NotNil(t, buildModel)
	require.NotNil(t, cleanup)
	assert.NotPanics(t, func() { cleanup() })
}

// TestProviderRegistryConcurrent simulates concurrent Get/Register under -race.
func TestProviderRegistryConcurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := NewProviderRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Get("eino"); err != nil {
				t.Errorf("Get: %v", err)
			}
			if got := reg.List(); len(got) == 0 {
				t.Error("List returned empty")
			}
			if reg.Default() == nil {
				t.Error("Default returned nil")
			}
		}()
	}
	// Repeated duplicate registrations should fail, not race.
	wg.Add(1)
	go func() {
		defer wg.Done()
		p := NewEinoProvider(WithProviderName("only-once"))
		if err := reg.Register(p); err != nil && !errors.Is(err, errProviderAlreadyRegistered) {
			t.Errorf("Register: %v", err)
		}
		if err := reg.Register(p); err != nil && !errors.Is(err, errProviderAlreadyRegistered) {
			t.Errorf("Register duplicate: %v", err)
		}
	}()
	wg.Wait()
}
