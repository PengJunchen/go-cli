package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestHTTPChatModelThinkingOnWire verifies that the compatibility provider sends
// configured reasoning effort without dropping tool or sampling options.
func TestHTTPChatModelThinkingOnWire(t *testing.T) {
	for _, operation := range []string{"generate", "stream"} {
		for _, test := range []struct {
			name         string
			level        ThinkingLevel
			effort       string
			omitThinking bool
		}{
			{name: "high", level: ThinkingHigh, effort: "high"},
			{name: "medium", level: ThinkingMedium, effort: "medium"},
			{name: "none", level: ThinkingNone},
			{name: "unset", omitThinking: true},
		} {
			t.Run(operation+"/"+test.name, func(t *testing.T) {
				defer verify.AssertNoGoroutineLeak(t)()
				requests := make(chan map[string]any, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var request map[string]any
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						http.Error(w, "invalid request JSON", http.StatusBadRequest)
						return
					}
					requests <- request
					if operation == "stream" {
						w.Header().Set("Content-Type", "text/event-stream")
						writeResponse(t, w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
				}))
				defer server.Close()

				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				provider := NewEinoProvider(WithBaseURL(server.URL))
				model, cleanup, err := provider.Build(ctx, ModelConfig{Model: "wire-test"})
				require.NoError(t, err)
				defer cleanup()
				options := []Option{
					WithTemperature(0.25),
					WithMaxTokens(321),
					WithTools([]ToolDefinition{{Name: "read", Parameters: map[string]any{"type": "object"}}}),
				}
				if !test.omitThinking {
					options = append(options, WithThinking(ThinkingConfigForLevel(test.level)))
				}
				messages := []Message{{Role: RoleUser, Content: "read the file"}}
				if operation == "stream" {
					stream, streamErr := model.Stream(ctx, messages, options...)
					require.NoError(t, streamErr)
					for chunk := range stream {
						require.NoError(t, chunk.Error)
					}
				} else {
					_, err = model.Generate(ctx, messages, options...)
					require.NoError(t, err)
				}
				select {
				case request := <-requests:
					if test.effort == "" {
						assert.NotContains(t, request, "reasoning_effort")
					} else {
						assert.Equal(t, test.effort, request["reasoning_effort"])
					}
					assert.Equal(t, float64(321), request["max_tokens"])
					assert.Equal(t, 0.25, request["temperature"])
					assert.Equal(t, "auto", request["tool_choice"])
					require.Len(t, request["tools"], 1)
					tool := request["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
					assert.Equal(t, "read", tool["name"])
					if operation == "stream" {
						assert.Equal(t, true, request["stream"])
						assert.Equal(t, map[string]any{"include_usage": true}, request["stream_options"])
					}
				case <-ctx.Done():
					t.Fatal("provider did not send a request")
				}
			})
		}
	}
}
