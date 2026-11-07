package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
)

func usageJSON(t *testing.T, usage *Usage) map[string]any {
	t.Helper()
	require.NotNil(t, usage)
	data, err := json.Marshal(usage)
	require.NoError(t, err)
	var result map[string]any
	require.NoError(t, json.Unmarshal(data, &result))
	return result
}

func TestStreamAcceptsUsageOnlyFromValidFrames(t *testing.T) {
	const terminal = `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
	const known = `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80}}}`
	const conflicting = `{"choices":[],"usage":{"prompt_tokens":200,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":3}}}`
	const invalidChoice = `{"choices":[{"delta":{"content":"invalid"}}],"usage":{"prompt_tokens":200,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":3}}}`
	const invalidTool = `{"choices":[{"delta":{"tool_calls":[{"index":-1}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80}}}`
	const incompleteTool = `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"id","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":200,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":3}}}`
	for _, test := range []struct {
		name      string
		frames    []string
		wantKnown bool
		wantError bool
	}{
		{"invalid-terminal-choice-cannot-overwrite", []string{terminal, known, invalidChoice, "[DONE]"}, true, true},
		{"invalid-terminal-choice-cannot-create", []string{terminal, invalidChoice, "[DONE]"}, false, true},
		{"conflicting-final-usage", []string{terminal, known, conflicting, "[DONE]"}, true, false},
		{"after-malformed-cannot-create", []string{terminal, `{invalid`, known, "[DONE]"}, false, true},
		{"invalid-tool-frame-cannot-create", []string{invalidTool, "[DONE]"}, false, true},
		{"incomplete-terminal-tool-cannot-create", []string{incompleteTool, "[DONE]"}, false, true},
		{"incomplete-terminal-tool-cannot-overwrite", []string{known, incompleteTool, "[DONE]"}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := make(chan SSEEvent, len(test.frames))
			for _, frame := range test.frames {
				events <- SSEEvent{Data: frame}
			}
			close(events)
			_, _, usage, err := accumulateOpenAIStreamToolCalls(context.Background(), events, make(chan MessageChunk, 10))
			if test.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if !test.wantKnown {
				assert.Nil(t, usage)
				return
			}
			require.NotNil(t, usage)
			assert.Equal(t, 100, usage.InputTokens)
			assert.Equal(t, 80, usage.CacheReadInputTokens)
		})
	}
}

func TestOpenAIInvalidFrameUsageDoesNotReachAttemptLedger(t *testing.T) {
	for _, provider := range []string{"compatible", "native"} {
		for _, priorUsage := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prior-%v", provider, priorUsage), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
					if priorUsage {
						writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":80}}}`)
					}
					writeSSE(w, `{"choices":[{"delta":{"content":"invalid"}}],"usage":{"prompt_tokens":200,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":3}}}`)
					writeSSE(w, "[DONE]")
				}))
				defer server.Close()
				model := detailTestModel(t, provider, server)
				authority := &attemptTestAuthority{id: "invalid-wire-usage"}
				stream, err := model.Stream(WithOperationDispatchAuthority(context.Background(), authority), []Message{{Role: RoleUser, Content: "request"}})
				require.NoError(t, err)
				var streamErr error
				for chunk := range stream {
					assert.False(t, chunk.Final)
					if chunk.Error != nil {
						streamErr = chunk.Error
					}
				}
				var finished *FinishedProviderAttemptError
				require.ErrorAs(t, streamErr, &finished)
				require.Len(t, authority.attempts, 1)
				if !priorUsage {
					assert.Equal(t, UsageUnknown, finished.Result.Usage.State)
					return
				}
				require.Equal(t, UsageKnown, finished.Result.Usage.State)
				assert.Equal(t, 100, finished.Result.Usage.Usage.InputTokens)
				assert.Equal(t, 80, finished.Result.Usage.Usage.CacheReadInputTokens)
			})
		}
	}
}

func TestAttemptUsageTraceStatusAndOpaqueIDs(t *testing.T) {
	for _, test := range []struct {
		name    string
		outcome AttemptOutcome
		status  tracing.SpanStatus
	}{
		{"success", AttemptSuccess, tracing.SpanStatusOK},
		{"provider-failure", AttemptProviderFailure, tracing.SpanStatusError},
		{"transport-failure", AttemptTransportFailure, tracing.SpanStatusError},
		{"cancelled", AttemptCancelled, tracing.SpanStatusError},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, exporter := newComposeTestCtx(t)
			result := ProviderAttemptResult{
				OperationID: "op-0123456789abcdef0123456789abcdef",
				AttemptID:   "attempt-fedcba9876543210fedcba9876543210",
				Outcome:     test.outcome, Usage: UnknownUsageEvidence(),
			}
			traceAttemptUsage(ctx, result)
			span, ok := exporter.spanByName("llm.attempt_usage")
			require.True(t, ok)
			assert.Equal(t, test.status, span.Status)
			operation, ok := findAttr(span, "operation_id")
			require.True(t, ok)
			assert.Equal(t, string(result.OperationID), operation)
			attempt, ok := findAttr(span, "attempt_id")
			require.True(t, ok)
			assert.Equal(t, string(result.AttemptID), attempt)
		})
	}
	t.Run("arbitrary-authority-ids-are-not-logged", func(t *testing.T) {
		ctx, exporter := newComposeTestCtx(t)
		traceAttemptUsage(ctx, ProviderAttemptResult{
			OperationID: "prompt-in-operation-id", AttemptID: "secret-in-attempt-id",
			Outcome: AttemptProviderFailure, Usage: UnknownUsageEvidence(),
		})
		span, ok := exporter.spanByName("llm.attempt_usage")
		require.True(t, ok)
		_, opFound := findAttr(span, "operation_id")
		_, attemptFound := findAttr(span, "attempt_id")
		assert.False(t, opFound)
		assert.False(t, attemptFound)
		data, err := json.Marshal(span)
		require.NoError(t, err)
		assert.NotContains(t, string(data), "prompt-in-operation-id")
		assert.NotContains(t, string(data), "secret-in-attempt-id")
	})
}

func detailTestModel(t *testing.T, provider string, server *httptest.Server) BaseChatModel {
	t.Helper()
	registry := NewProviderRegistry()
	if provider == "native" {
		require.NoError(t, registry.Register(NewOpenAIProvider(WithNativeHTTPClient(server.Client()))))
		provider = "openai"
	} else {
		require.NoError(t, registry.Register(NewEinoProvider(WithProviderName(provider), WithHTTPClient(server.Client()))))
	}
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), ModelRequest{
		ProviderID: provider, ModelID: "usage-fixture", Endpoint: server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	t.Cleanup(cleanup)
	return model
}

func TestOpenAIDetailedUsageWire(t *testing.T) {
	cases := []struct {
		name, details                    string
		cache, reasoning                 any
		cacheReported, reasoningReported bool
	}{
		{"reported", `,"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":7}`, float64(80), float64(7), true, true},
		{"zero", `,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}`, nil, nil, true, true},
		{"absent", ``, nil, nil, false, false},
		{"negative", `,"prompt_tokens_details":{"cached_tokens":-1},"completion_tokens_details":{"reasoning_tokens":-1}`, nil, nil, false, false},
		{"over-total", `,"prompt_tokens_details":{"cached_tokens":101},"completion_tokens_details":{"reasoning_tokens":21}`, nil, nil, false, false},
		{"wrong-types", `,"prompt_tokens_details":{"cached_tokens":"bad"},"completion_tokens_details":[]`, nil, nil, false, false},
		{"partial", `,"prompt_tokens_details":{"cached_tokens":60},"completion_tokens_details":{}`, float64(60), nil, true, false},
	}
	for _, test := range cases {
		for _, provider := range []string{"compatible", "native"} {
			for _, mode := range []string{"generate", "stream"} {
				t.Run(test.name+"/"+provider+"/"+mode, func(t *testing.T) {
					wireUsage := fmt.Sprintf(`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120%s}`, test.details)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						if mode == "generate" {
							w.Header().Set("Content-Type", "application/json")
							writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":`+wireUsage+`}`)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						writeSSE(w, `{"choices":[{"delta":{"content":"ok"}}]}`)
						writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
						writeSSE(w, `{"choices":[],"usage":`+wireUsage+`}`)
						writeSSE(w, "[DONE]")
					}))
					defer server.Close()
					model := detailTestModel(t, provider, server)
					authority := &attemptTestAuthority{id: "wire-usage"}
					ctx := WithOperationDispatchAuthority(context.Background(), authority)
					var usage *Usage
					if mode == "generate" {
						message, err := model.Generate(ctx, []Message{{Role: RoleUser, Content: "request"}})
						require.NoError(t, err)
						usage = message.Usage
					} else {
						stream, err := model.Stream(ctx, []Message{{Role: RoleUser, Content: "request"}})
						require.NoError(t, err)
						for chunk := range stream {
							require.NoError(t, chunk.Error)
							if chunk.Final {
								usage = chunk.Usage
							}
						}
					}
					got := usageJSON(t, usage)
					assert.Equal(t, float64(100), got["input_tokens"])
					assert.Equal(t, float64(20), got["output_tokens"])
					assert.Equal(t, float64(120), got["total_tokens"])
					assert.Equal(t, test.cache, got["cache_read_input_tokens"])
					assert.Equal(t, test.reasoning, got["reasoning_output_tokens"])
					assert.Equal(t, test.cacheReported, got["cache_read_input_tokens_reported"] == true)
					assert.Equal(t, test.reasoningReported, got["reasoning_output_tokens_reported"] == true)
					require.Len(t, authority.attempts, 1)
					assert.Equal(t, UsageKnown, authority.attempts[0].result.Usage.State)
					assert.Equal(t, got, usageJSON(t, authority.attempts[0].result.Usage.Usage))
				})
			}
		}
	}
}

func TestOpenAIUsageTracePreservesReportedZeroAndOmissions(t *testing.T) {
	for _, test := range []struct {
		name, details string
		reported      bool
	}{
		{"reported-zero", `,"prompt_tokens_details":{"cached_tokens":0},"completion_tokens_details":{"reasoning_tokens":0}`, true},
		{"absent", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5`+test.details+`}}`)
			}))
			defer server.Close()
			model := detailTestModel(t, "compatible", server)
			ctx, exporter := newComposeTestCtx(t)
			ctx = WithOperationDispatchAuthority(ctx, &attemptTestAuthority{id: "usage-trace"})
			_, err := model.Generate(ctx, []Message{{Role: RoleUser, Content: "request"}})
			require.NoError(t, err)
			require.Eventually(t, func() bool { _, ok := exporter.spanByName("llm.attempt_usage"); return ok }, time.Second, 5*time.Millisecond)
			span, ok := exporter.spanByName("llm.attempt_usage")
			require.True(t, ok)
			for _, key := range []string{"tokens_cache_read_input", "tokens_reasoning_output"} {
				value, found := findAttr(span, key)
				assert.Equal(t, test.reported, found)
				if test.reported {
					assert.Equal(t, 0, value)
				}
			}
		})
	}
}

func TestOverflowContinuationDetailedUsageCoverage(t *testing.T) {
	first := `{"input_tokens":100,"output_tokens":20,"total_tokens":120,"cache_read_input_tokens":80,"cache_read_input_tokens_reported":true,"reasoning_output_tokens":7,"reasoning_output_tokens_reported":true}`
	second := `{"input_tokens":50,"output_tokens":10,"total_tokens":60,"cache_read_input_tokens":40,"cache_read_input_tokens_reported":true,"reasoning_output_tokens":3,"reasoning_output_tokens_reported":true}`
	for _, test := range []struct {
		name, first, second string
		complete            bool
	}{
		{"complete", first, second, true}, {"missing-first", "", second, false}, {"missing-second", first, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			parse := func(input string) *Usage {
				if input == "" {
					return nil
				}
				var usage Usage
				require.NoError(t, json.Unmarshal([]byte(input), &usage))
				return &usage
			}
			calls := 0
			model := &mockModel{generateFn: func(context.Context, []Message, ...Option) (*Message, error) {
				calls++
				if calls == 1 {
					return &Message{Role: RoleAssistant, Content: "first", FinishReason: "length", Usage: parse(test.first)}, nil
				}
				return &Message{Role: RoleAssistant, Content: "second", FinishReason: "stop", Usage: parse(test.second)}, nil
			}}
			result, err := NewOverflowRecoveryMiddleware().WrapModel(model).Generate(context.Background(), nil)
			require.NoError(t, err)
			require.Equal(t, 2, calls)
			got := usageJSON(t, result.Usage)
			assert.Equal(t, test.complete, got["cache_read_input_tokens_reported"] == true)
			assert.Equal(t, test.complete, got["reasoning_output_tokens_reported"] == true)
			if test.complete {
				assert.Equal(t, float64(120), got["cache_read_input_tokens"])
				assert.Equal(t, float64(10), got["reasoning_output_tokens"])
			}
		})
	}
}

func TestOpenAIStreamFailurePreservesReportedUsage(t *testing.T) {
	for _, provider := range []string{"compatible", "native"} {
		for _, includeUsage := range []bool{true, false} {
			for _, failure := range []string{"missing-done", "malformed"} {
				t.Run(fmt.Sprintf("%s/usage-%v/%s", provider, includeUsage, failure), func(t *testing.T) {
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Content-Type", "text/event-stream")
						writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
						if includeUsage {
							writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80},"completion_tokens_details":{"reasoning_tokens":7}}}`)
						}
						if failure == "malformed" {
							writeSSE(w, `{malformed`)
							writeSSE(w, "[DONE]")
						}
						// Without [DONE], usage may be known while the candidate is incomplete.
					}))
					defer server.Close()
					model := detailTestModel(t, provider, server)
					authority := &attemptTestAuthority{id: "failed-wire-usage"}
					ctx, exporter := newComposeTestCtx(t)
					ctx = WithOperationDispatchAuthority(ctx, authority)
					stream, err := model.Stream(ctx, []Message{{Role: RoleUser, Content: "request"}})
					require.NoError(t, err)
					var streamErr error
					for chunk := range stream {
						assert.False(t, chunk.Final)
						if chunk.Error != nil {
							streamErr = chunk.Error
						}
					}
					var finished *FinishedProviderAttemptError
					require.ErrorAs(t, streamErr, &finished)
					require.Len(t, authority.attempts, 1)
					assert.Equal(t, AttemptProviderFailure, finished.Result.Outcome)
					assert.Equal(t, FailureProtocol, finished.Result.Failure)
					if !includeUsage {
						assert.Equal(t, UsageUnknown, finished.Result.Usage.State)
						return
					}
					require.Equal(t, UsageKnown, finished.Result.Usage.State)
					got := usageJSON(t, finished.Result.Usage.Usage)
					assert.Equal(t, float64(100), got["input_tokens"])
					assert.Equal(t, float64(80), got["cache_read_input_tokens"])
					assert.Equal(t, float64(7), got["reasoning_output_tokens"])
					require.Eventually(t, func() bool { _, ok := exporter.spanByName("llm.attempt_usage"); return ok }, time.Second, 5*time.Millisecond)
					span, ok := exporter.spanByName("llm.attempt_usage")
					require.True(t, ok)
					cached, ok := findAttr(span, "tokens_cache_read_input")
					require.True(t, ok)
					assert.Equal(t, 80, cached)
					reasoning, ok := findAttr(span, "tokens_reasoning_output")
					require.True(t, ok)
					assert.Equal(t, 7, reasoning)
				})
			}
		}
	}
}
