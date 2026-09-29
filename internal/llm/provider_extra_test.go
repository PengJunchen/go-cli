package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestEinoProviderOptions verifies the With* option setters that are otherwise
// uncovered, including the HTTP client and default-model overrides.
func TestEinoProviderOptions(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	custom := &http.Client{}
	p := NewEinoProvider(WithHTTPClient(custom), WithDefaultModel("custom-default"), WithBaseURL("http://custom-base"), WithAPIKey("k"))
	assert.Same(t, custom, p.httpClient)
	assert.Equal(t, "custom-default", p.defaultModel)
	assert.Equal(t, "http://custom-base", p.defaultBaseURL)
	assert.Equal(t, "k", p.defaultAPIKeySource())

	// WithModels also sets the default model to the first entry.
	p2 := NewEinoProvider(WithModels([]ModelInfo{{Name: "first-model"}}))
	assert.Equal(t, "first-model", p2.defaultModel)
	assert.Len(t, p2.Models(), 1)

	// Empty models keep the default model unchanged.
	p3 := NewEinoProvider(WithModels(nil), WithDefaultModel("keep"))
	assert.Equal(t, "keep", p3.defaultModel)
}

// TestEinoProviderBuildResolvesDefaultBaseURL verifies that a model with no
// BaseURL uses the provider default and a model with no API key sends no
// Authorization header.
func TestEinoProviderBuildResolvesDefaultBaseURL(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	provider := NewEinoProvider(WithBaseURL(srv.URL), WithProviderName("no-key"))
	m, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = m.Generate(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, "", gotAuth, "no Authorization header without an API key")
}

func TestHTTPChatModelBuildBodyPreservesSamplingPresence(t *testing.T) {
	tests := []struct {
		name            string
		config          ModelConfig
		options         []Option
		wantTemperature *float64
		wantMaxTokens   *int
	}{
		{
			name: "explicit zero config values",
			config: ModelConfig{
				TemperatureSet: true,
				MaxTokensSet:   true,
			},
			wantTemperature: floatPointer(0),
			wantMaxTokens:   intPointer(0),
		},
		{
			name:            "unset config values",
			config:          ModelConfig{},
			wantTemperature: nil,
			wantMaxTokens:   nil,
		},
		{
			name: "per call explicit zero values",
			config: ModelConfig{
				Temperature:    0.8,
				TemperatureSet: true,
				MaxTokens:      512,
				MaxTokensSet:   true,
			},
			options:         []Option{WithTemperature(0), WithMaxTokens(0)},
			wantTemperature: floatPointer(0),
			wantMaxTokens:   intPointer(0),
		},
		{
			name: "legacy nonzero config values",
			config: ModelConfig{
				Temperature: 0.25,
				MaxTokens:   128,
			},
			wantTemperature: floatPointer(0.25),
			wantMaxTokens:   intPointer(128),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &HTTPChatModel{buildMode: UnboundLibraryBuild, provider: NewEinoProvider(), cfg: tt.config}
			body, err := model.buildBody([]Message{{Role: RoleUser, Content: "x"}}, tt.options...)
			require.NoError(t, err)
			var request openAIRequest
			require.NoError(t, json.Unmarshal(body, &request))
			assert.Equal(t, tt.wantTemperature, request.Temperature)
			assert.Equal(t, tt.wantMaxTokens, request.MaxTokens)
		})
	}
}

func floatPointer(value float64) *float64 { return &value }

func intPointer(value int) *int { return &value }

// TestHTTPChatModelStreamError verifies Stream closes the channel on error.
func TestHTTPChatModelStreamError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	provider := NewEinoProvider(WithBaseURL("http://127.0.0.1:1"))
	m, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	ch, err := m.Stream(context.Background(), nil)
	require.Error(t, err)
	_, ok := <-ch
	assert.False(t, ok, "stream channel must be closed on error")
}

// TestHTTPChatModelGenerate_ResponseReadError verifies a transport error on a
// provider call surfaces, exercising the generate error path.
func TestHTTPChatModelGenerate_ResponseReadError(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := &HTTPChatModel{buildMode: UnboundLibraryBuild,
		provider: NewEinoProvider(),
		cfg:      ModelConfig{Model: "m", BaseURL: "http://127.0.0.1:1"},
		client: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errTestTransport
			}),
		},
	}
	_, err := m.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, errTestTransport)
}

var errTestTransport = testTransportError("llm: transport boom")

type testTransportError string

func (e testTransportError) Error() string { return string(e) }

// TestBuildBodyNoOptions verifies buildBody applies config defaults when no
// per-call options are given.
func TestBuildBodyNoOptions(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	m := &HTTPChatModel{buildMode: UnboundLibraryBuild,
		provider: NewEinoProvider(),
		cfg:      ModelConfig{Model: "m", Temperature: 0.5, MaxTokens: 11},
		client:   http.DefaultClient,
	}
	body, err := m.buildBody([]Message{{Role: RoleUser, Content: "x"}})
	require.NoError(t, err)
	var req openAIRequest
	require.NoError(t, json.Unmarshal(body, &req))
	require.NotNil(t, req.Temperature)
	assert.Equal(t, 0.5, *req.Temperature)
	require.NotNil(t, req.MaxTokens)
	assert.Equal(t, 11, *req.MaxTokens)
	assert.Nil(t, req.Stop)
}

// TestConvertAssistantToolCalls verifies the mapping of OpenAI tool calls to
// llm.ToolCall values, including the JSON-args path and the plain-string path.
func TestConvertAssistantToolCalls(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	assert.Nil(t, convertAssistantToolCalls(nil))

	raw := convertAssistantToolCalls([]openAIToolCall{{
		ID: "c1", Type: "function",
		Function: openAIFunction{Name: "read", Arguments: `{"path":"a"}`},
	}})
	require.Len(t, raw, 1)
	args, ok := raw[0].Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "a", args["path"])

	// Empty arguments => Args stays nil.
	empty := convertAssistantToolCalls([]openAIToolCall{{
		Function: openAIFunction{Name: "f"},
	}})
	require.Len(t, empty, 1)
	assert.Nil(t, empty[0].Args)
}

func TestConvertAssistantToolCalls_PreservesNonObjectWireArgumentsForRejection(t *testing.T) {
	for _, rawArgs := range []string{"null", "[]", "true", "42", `"value"`, "not-json"} {
		t.Run(rawArgs, func(t *testing.T) {
			calls := convertAssistantToolCalls([]openAIToolCall{{
				ID: "call_1", Type: "function",
				Function: openAIFunction{Name: "read", Arguments: rawArgs},
			}})
			require.Len(t, calls, 1)
			got, ok := calls[0].Args.(string)
			require.True(t, ok, "wire arguments must remain distinguishable from typed nil no-arg tool calls")
			assert.Equal(t, rawArgs, got)
		})
	}
}

// TestHTTPChatModelGenerate_BodyDrained verifies reading the response fully
// even when the body lacks a closing decode (helper for coverage of the
// io.ReadAll error branch is impractical; this asserts decode error), at least
// confirms a malformed-but-valid body errors cleanly.
func TestHTTPChatModelGenerate_MalformedButValid(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":` /* unterminated JSON */)
	}))
	defer srv.Close()

	provider := NewEinoProvider(WithBaseURL(srv.URL))
	m, _, err := provider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	_, err = m.Generate(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode response")
}
