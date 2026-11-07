package llm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeStreamPrimaryCoveragePresence(t *testing.T) {
	for _, provider := range []string{"claude", "gemini"} {
		for _, test := range []struct {
			name, input, output string
			want                PrimaryUsageCoverage
			absent              bool
		}{
			{"zero", `"input":0`, `"output":0`, UsageCoverageComplete, false},
			{"missing-input", "", `"output":0`, UsageCoverageUnknown, true},
			{"missing-output", `"input":0`, "", UsageCoverageUnknown, true},
			{"negative-input", `"input":-1`, `"output":0`, UsageCoverageUnknown, true},
		} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				var body string
				if provider == "claude" {
					input := strings.Replace(test.input, `"input"`, `"input_tokens"`, 1)
					output := strings.Replace(test.output, `"output"`, `"output_tokens"`, 1)
					body = fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{%s}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{%s}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", input, output)
				} else {
					input := strings.Replace(test.input, `"input"`, `"promptTokenCount"`, 1)
					output := strings.Replace(test.output, `"output"`, `"candidatesTokenCount"`, 1)
					usage := input
					if input != "" && output != "" {
						usage += ","
					}
					usage += output
					body = fmt.Sprintf("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{%s}}\n\n", usage)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte(body))
				}))
				defer server.Close()
				var model BaseChatModel
				var err error
				if provider == "claude" {
					model, _, err = NewClaudeProvider(WithNativeBaseURL(server.URL), WithNativeAPIKey("fixture")).Build(context.Background(), ModelConfig{Model: "m"})
				} else {
					model, _, err = NewGeminiProvider(WithNativeBaseURL(server.URL), WithNativeAPIKey("fixture")).Build(context.Background(), ModelConfig{Model: "m"})
				}
				require.NoError(t, err)
				stream, err := model.Stream(context.Background(), nil)
				require.NoError(t, err)
				var final *MessageChunk
				for chunk := range stream {
					require.NoError(t, chunk.Error)
					if chunk.Final {
						copyChunk := chunk
						final = &copyChunk
					}
				}
				require.NotNil(t, final)
				if test.absent {
					assert.Nil(t, final.Usage)
					return
				}
				require.NotNil(t, final.Usage)
				assert.Equal(t, test.want, final.Usage.PrimaryCoverage.Normalized())
				if test.name == "zero" {
					assert.Zero(t, final.Usage.InputTokens)
					assert.Zero(t, final.Usage.OutputTokens)
				}
			})
		}
	}
}
