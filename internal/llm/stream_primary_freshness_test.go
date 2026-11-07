package llm

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIStreamPrimaryFreshness(t *testing.T) {
	for _, provider := range []string{"native", "compatible"} {
		for _, test := range []struct {
			name   string
			frames []string
			want   PrimaryUsageCoverage
			output int
		}{
			{"early-then-text", []string{`{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"content":"b"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, UsageCoveragePartial, 0},
			{"early-then-reasoning", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, UsageCoveragePartial, 0},
			{"early-then-tool", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"tool","arguments":"{}"}}]}}]}`, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, `[DONE]`}, UsageCoveragePartial, 0},
			{"early-then-refusal", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"refusal":"declined"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, UsageCoveragePartial, 0},
			{"early-then-function-call", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"function_call":{"name":"tool","arguments":"{}"}}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, UsageCoveragePartial, 0},
			{"early-then-unknown-delta", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"vendor_extra":"value"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, UsageCoverageUnknown, 0},
			{"metadata-only", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"role":"assistant","index":0}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, UsageCoverageUnknown, 0},
			{"output-then-unknown", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"content":"later"}}]}`, `{"choices":[{"delta":{"vendor_extra":"value"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, UsageCoveragePartial, 0},
			{"unknown-then-output", []string{`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"vendor_extra":"value"}}]}`, `{"choices":[{"delta":{"content":"later"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`}, UsageCoveragePartial, 0},
			{"final-usage-after-stop", []string{`{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`, `{"choices":[{"delta":{"content":"b"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":8}}`, `[DONE]`}, UsageCoverageComplete, 8},
			{"latest-frame-has-usage", []string{`{"choices":[{"delta":{"content":"a"}}]}`, `{"choices":[{"delta":{"content":"b"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":8}}`, `[DONE]`}, UsageCoverageComplete, 8},
		} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					for _, frame := range test.frames {
						writeSSE(w, frame)
					}
				}))
				defer server.Close()
				model := detailTestModel(t, provider, server)
				authority := &attemptTestAuthority{id: "freshness"}
				stream, err := model.Stream(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
				require.NoError(t, err)
				var usage *Usage
				for chunk := range stream {
					require.NoError(t, chunk.Error)
					if chunk.Final {
						usage = chunk.Usage
					}
				}
				require.NotNil(t, usage)
				assert.Equal(t, 100, usage.InputTokens)
				assert.Equal(t, test.output, usage.OutputTokens)
				assert.Equal(t, test.want, usage.PrimaryCoverage)
				require.Len(t, authority.attempts, 1)
				assert.Equal(t, test.want, authority.attempts[0].result.Usage.Usage.PrimaryCoverage)
			})
		}
	}
}

func TestOpenAIStreamFailureKeepsEarlyUsageCoverage(t *testing.T) {
	for _, test := range []struct {
		name     string
		after    string
		coverage PrimaryUsageCoverage
	}{
		{"before-output", "", UsageCoverageUnknown},
		{"after-output", `{"choices":[{"delta":{"content":"later"}}]}`, UsageCoveragePartial},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := make(chan SSEEvent, 4)
			events <- SSEEvent{Data: `{"choices":[{"delta":{}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`}
			if test.after != "" {
				events <- SSEEvent{Data: test.after}
			}
			events <- SSEEvent{Data: `{invalid`}
			close(events)
			_, _, usage, err := accumulateOpenAIStreamToolCalls(context.Background(), events, make(chan MessageChunk, 8))
			require.Error(t, err)
			require.NotNil(t, usage)
			assert.Equal(t, 100, usage.InputTokens)
			assert.Equal(t, 0, usage.OutputTokens)
			assert.Equal(t, test.coverage, usage.PrimaryCoverage)
		})
	}
}

func TestInvalidStreamFrameDoesNotChangePriorUsageEvidence(t *testing.T) {
	events := make(chan SSEEvent, 3)
	events <- SSEEvent{Data: `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"completion_tokens_details":{"reasoning_tokens":7}}}`}
	events <- SSEEvent{Data: `{"choices":[{"delta":{"tool_calls":[{"index":-1}]},"finish_reason":"tool_calls"}]}`}
	events <- SSEEvent{Data: "[DONE]"}
	close(events)
	_, _, usage, err := accumulateOpenAIStreamToolCalls(context.Background(), events, make(chan MessageChunk, 8))
	require.Error(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 7, usage.ReasoningOutputTokens)
	assert.True(t, usage.ReasoningOutputTokensReported)
	assert.Equal(t, UsageCoverageUnknown, usage.PrimaryCoverage, "invalid terminal frame cannot prove final coverage")
}

func TestOpenAIStreamOverflowingTerminalPreservesPriorUsage(t *testing.T) {
	for _, provider := range []string{"native", "compatible"} {
		for _, test := range []struct {
			name            string
			later, overflow bool
			coverage        PrimaryUsageCoverage
		}{
			{"overflow-no-later-output", false, true, UsageCoverageUnknown},
			{"overflow-after-output", true, true, UsageCoveragePartial},
			{"optional-terminal-usage-omitted", false, false, UsageCoverageUnknown},
		} {
			t.Run(provider+"/"+test.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					writeSSE(w, `{"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":100,"completion_tokens":0}}`)
					if test.later {
						writeSSE(w, `{"choices":[{"delta":{"content":"later"}}]}`)
					}
					if test.overflow {
						writeSSE(w, fmt.Sprintf(`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":1}}`, math.MaxInt))
					} else {
						writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
					}
					writeSSE(w, "[DONE]")
				}))
				defer server.Close()
				model := detailTestModel(t, provider, server)
				authority := &attemptTestAuthority{id: "overflowing-terminal"}
				stream, err := model.Stream(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
				require.NoError(t, err)
				var got *Usage
				for chunk := range stream {
					require.NoError(t, chunk.Error)
					if chunk.Final {
						got = chunk.Usage
					}
				}
				require.NotNil(t, got, "valid earlier usage must remain available")
				assert.Equal(t, 100, got.InputTokens)
				assert.Zero(t, got.OutputTokens)
				assert.Equal(t, test.coverage, got.PrimaryCoverage)
				require.Len(t, authority.attempts, 1)
				assert.Equal(t, test.coverage, authority.attempts[0].result.Usage.Usage.PrimaryCoverage)
			})
		}
	}
}

func TestClaudeStreamOverflowingTerminalPreservesPriorUsage(t *testing.T) {
	for _, test := range []struct {
		name, extra string
		coverage    PrimaryUsageCoverage
	}{
		{"no-later-output", "", UsageCoverageUnknown},
		{"after-output", "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n", UsageCoveragePartial},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\",\"usage\":{\"input_tokens\":100,\"output_tokens\":0}}}\n\n%sevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":%d,\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", test.extra, math.MaxInt)
			}))
			defer server.Close()
			model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL), WithNativeAPIKey("fixture")).Build(context.Background(), ModelConfig{Model: "m"})
			require.NoError(t, err)
			stream, err := model.Stream(context.Background(), nil)
			require.NoError(t, err)
			var got *Usage
			for chunk := range stream {
				require.NoError(t, chunk.Error)
				if chunk.Final {
					got = chunk.Usage
				}
			}
			require.NotNil(t, got, "valid earlier usage must remain available")
			assert.Equal(t, 100, got.InputTokens)
			assert.Zero(t, got.OutputTokens)
			assert.Equal(t, test.coverage, got.PrimaryCoverage)
		})
	}
}
