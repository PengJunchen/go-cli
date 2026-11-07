package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStreamCoverageRequiresTerminalUsage uses loopback SSE peers to distinguish
// a useful early billing snapshot from evidence of final primary totals.
func TestStreamCoverageRequiresTerminalUsage(t *testing.T) {
	openAI := []struct {
		name          string
		frames        []string
		input, output int
		coverage      PrimaryUsageCoverage
		failed        bool
	}{
		{"no-visible-delta", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, 100, 0, UsageCoverageUnknown, false},
		{"metadata-only", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"role":"assistant"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, 100, 0, UsageCoverageUnknown, false},
		{"later-output", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"content":"later"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, 100, 0, UsageCoveragePartial, false},
		{"same-terminal-usage", []string{`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`, `[DONE]`}, 100, 5, UsageCoverageComplete, false},
		{"usage-after-terminal", []string{`{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5}}`, `[DONE]`}, 100, 5, UsageCoverageComplete, false},
		{"terminal-zero", []string{`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, `[DONE]`}, 0, 0, UsageCoverageComplete, false},
		{"unknown-terminal-reason", []string{`{"choices":[{"delta":{},"finish_reason":"vendor_custom"}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`, `[DONE]`}, 100, 5, UsageCoverageUnknown, false},
		{"negative-terminal-preserves-snapshot", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":-1,"completion_tokens":5}}`, `[DONE]`}, 100, 0, UsageCoverageUnknown, false},
		{"negative-terminal-then-valid", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":-1,"completion_tokens":5}}`, `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":5}}`, `[DONE]`}, 100, 5, UsageCoverageComplete, false},
		{"failed-after-final-usage", []string{`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5}}`, `{invalid`}, 100, 5, UsageCoverageComplete, true},
	}
	for _, provider := range []string{"native", "compatible"} {
		for _, tc := range openAI {
			t.Run("openai/"+provider+"/"+tc.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, frame := range tc.frames {
						writeSSE(w, frame)
					}
				}))
				defer srv.Close()
				model := detailTestModel(t, provider, srv)
				authority := &attemptTestAuthority{id: "terminal-finality"}
				stream, err := model.Stream(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
				require.NoError(t, err)
				var got *Usage
				var streamErr error
				for chunk := range stream {
					if chunk.Final || chunk.Error != nil {
						got = chunk.Usage
					}
					if chunk.Error != nil {
						streamErr = chunk.Error
					}
				}
				if tc.failed {
					var finished *FinishedProviderAttemptError
					require.ErrorAs(t, streamErr, &finished)
					got = finished.Result.Usage.Usage
				} else {
					require.NoError(t, streamErr)
				}
				require.NotNil(t, got)
				assert.Equal(t, tc.input, got.InputTokens)
				assert.Equal(t, tc.output, got.OutputTokens)
				assert.Equal(t, tc.coverage, got.PrimaryCoverage)
				require.Len(t, authority.attempts, 1)
				require.NotNil(t, authority.attempts[0].result.Usage.Usage)
				assert.Equal(t, tc.coverage, authority.attempts[0].result.Usage.Usage.PrimaryCoverage)
			})
		}
	}

	gemini := []struct {
		name          string
		frames        []string
		input, output int
		coverage      PrimaryUsageCoverage
		failed        bool
	}{
		{"no-visible-delta", []string{`{"candidates":[{}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"finishReason":"STOP"}]}`}, 100, 0, UsageCoverageUnknown, false},
		{"metadata-only", []string{`{"candidates":[{}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"content":{"role":"model","parts":[]}}]}`, `{"candidates":[{"finishReason":"STOP"}]}`}, 100, 0, UsageCoverageUnknown, false},
		{"later-output", []string{`{"candidates":[{}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"content":{"parts":[{"text":"later"}]},"finishReason":"STOP"}]}`}, 100, 0, UsageCoveragePartial, false},
		{"same-terminal-usage", []string{`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5}}`}, 100, 5, UsageCoverageComplete, false},
		{"usage-after-terminal", []string{`{"candidates":[{"finishReason":"STOP"}]}`, `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5}}`}, 100, 5, UsageCoverageComplete, false},
		{"terminal-zero", []string{`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":0,"candidatesTokenCount":0}}`}, 0, 0, UsageCoverageComplete, false},
		{"unknown-terminal-reason", []string{`{"candidates":[{"finishReason":"VENDOR_CUSTOM"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5}}`}, 100, 5, UsageCoverageUnknown, false},
		{"negative-terminal-preserves-snapshot", []string{`{"candidates":[{}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":-1,"candidatesTokenCount":5}}`}, 100, 0, UsageCoverageUnknown, false},
		{"negative-terminal-then-valid", []string{`{"candidates":[{}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":0}}`, `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":-1,"candidatesTokenCount":5}}`, `{"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5}}`}, 100, 5, UsageCoverageComplete, false},
		{"failed-after-final-usage", []string{`{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":5}}`, `{invalid`}, 100, 5, UsageCoverageComplete, true},
	}
	for _, tc := range gemini {
		t.Run("gemini/"+tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, frame := range tc.frames {
					writeSSE(w, frame)
				}
			}))
			defer srv.Close()
			model, _, err := NewGeminiProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("fixture")).Build(context.Background(), ModelConfig{Model: "m"})
			require.NoError(t, err)
			stream, err := model.Stream(context.Background(), nil)
			require.NoError(t, err)
			var got *Usage
			var streamErr error
			for chunk := range stream {
				if chunk.Final || chunk.Error != nil {
					got = chunk.Usage
				}
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
			}
			if tc.failed {
				require.Error(t, streamErr)
			} else {
				require.NoError(t, streamErr)
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.input, got.InputTokens)
			assert.Equal(t, tc.output, got.OutputTokens)
			assert.Equal(t, tc.coverage, got.PrimaryCoverage)
		})
	}

	claude := []struct {
		name, startUsage, middle, terminalUsage string
		input, output                           int
		coverage                                PrimaryUsageCoverage
	}{
		{"no-visible-delta", `{"input_tokens":100,"output_tokens":0}`, "", "", 100, 0, UsageCoverageUnknown},
		{"later-output", `{"input_tokens":100,"output_tokens":0}`, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"later\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n", "", 100, 0, UsageCoveragePartial},
		{"nonterminal-output-not-final", `{"input_tokens":100,"output_tokens":0}`, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":null},\"usage\":{\"output_tokens\":3}}\n\n", "", 100, 3, UsageCoverageUnknown},
		{"split-final-output", `{"input_tokens":100}`, "", `,"usage":{"output_tokens":5}`, 100, 5, UsageCoverageComplete},
		{"terminal-zero", `{"input_tokens":0}`, "", `,"usage":{"output_tokens":0}`, 0, 0, UsageCoverageComplete},
		{"unknown-terminal-reason", `{"input_tokens":100}`, "", `,"usage":{"output_tokens":5}`, 100, 5, UsageCoverageUnknown},
		{"negative-terminal-preserves-snapshot", `{"input_tokens":100,"output_tokens":0}`, "", `,"usage":{"output_tokens":-1}`, 100, 0, UsageCoverageUnknown},
		{"failed-after-terminal-usage", `{"input_tokens":100}`, "", `,"usage":{"output_tokens":5}`, 100, 5, UsageCoverageComplete},
	}
	for _, tc := range claude {
		t.Run("claude/"+tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				reason := "end_turn"
				if tc.name == "unknown-terminal-reason" {
					reason = "vendor_custom"
				}
				body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":" + tc.startUsage + "}}\n\n" + tc.middle + "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"" + reason + "\"}" + tc.terminalUsage + "}\n\n"
				if tc.name == "failed-after-terminal-usage" {
					body += "event: message_stop\ndata: {invalid\n\n"
				} else {
					body += "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
				}
				_, _ = strings.NewReader(body).WriteTo(w)
			}))
			defer srv.Close()
			model, _, err := NewClaudeProvider(WithNativeBaseURL(srv.URL), WithNativeAPIKey("fixture")).Build(context.Background(), ModelConfig{Model: "m"})
			require.NoError(t, err)
			stream, err := model.Stream(context.Background(), nil)
			require.NoError(t, err)
			var got *Usage
			var streamErr error
			for chunk := range stream {
				if chunk.Final || chunk.Error != nil {
					got = chunk.Usage
				}
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
			}
			if tc.name == "failed-after-terminal-usage" {
				require.Error(t, streamErr)
			} else {
				require.NoError(t, streamErr)
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.input, got.InputTokens)
			assert.Equal(t, tc.output, got.OutputTokens)
			assert.Equal(t, tc.coverage, got.PrimaryCoverage)
		})
	}
}
