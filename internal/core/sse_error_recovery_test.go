package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/compaction"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

const sseRecoveryRateLimitEnvelope = `{"error":{"message":"Rate limit reached; retry after this window. diagnostic-secret-canary","type":"api_error","code":null}}`

type sseRecoveryActionTool struct{ calls atomic.Int32 }

func (*sseRecoveryActionTool) Name() string        { return "run_once" }
func (*sseRecoveryActionTool) Description() string { return "Runs one test action." }
func (*sseRecoveryActionTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (tool *sseRecoveryActionTool) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	tool.calls.Add(1)
	return &tools.ToolResult{Output: "ran once"}, nil
}

type sseRecoveryWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type sseRecoveryWireRequest struct {
	Messages []sseRecoveryWireMessage `json:"messages"`
}

func TestLoopRetriesTypedSSELimitAfterRolePlaceholder(t *testing.T) {
	t.Run("role placeholder retries and dispatches one action", func(t *testing.T) {
		var requests atomic.Int32
		var userContentCount atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request sseRecoveryWireRequest
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				t.Errorf("read request: %v", readErr)
				return
			}
			if decodeErr := json.Unmarshal(body, &request); decodeErr != nil {
				t.Errorf("decode request: %v", decodeErr)
				return
			}
			for _, message := range request.Messages {
				if message.Role == string(llm.RoleUser) && message.Content == "run the action once" {
					userContentCount.Add(1)
				}
			}
			index := requests.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			switch index {
			case 1:
				writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{"role":"assistant"}}]}`)
				writeSSERecoveryFrame(t, w, sseRecoveryRateLimitEnvelope)
				writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{"content":"must be ignored"}}]}`)
				writeSSERecoveryFrame(t, w, "[DONE]")
			case 2:
				writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-once","type":"function","function":{"name":"run_once","arguments":"{}"}}]}}]}`)
				writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
				writeSSERecoveryFrame(t, w, "[DONE]")
			default:
				writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{"content":"done"}}]}`)
				writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
				writeSSERecoveryFrame(t, w, "[DONE]")
			}
		}))
		defer server.Close()

		model, cleanup := sseRecoveryModel(t, server)
		defer cleanup()
		tool := &sseRecoveryActionTool{}
		registry := tools.NewDefaultToolRegistry()
		require.NoError(t, registry.Register(context.Background(), tool))
		corePolicy := &operationControlledRetryPolicy{allowedRetries: 1}
		middlewarePolicy := &operationControlledRetryPolicy{allowedRetries: 10}
		retryModel := llm.NewRetryModelMiddleware(llm.WithRetryPolicy(middlewarePolicy)).WrapModel(model)
		attempts := &attemptResultRecorder{}
		loop := NewLoopAgent(
			WithLLM(attemptGuardedModel{BaseChatModel: retryModel}),
			WithTools(registry),
			WithMaxIterations(2),
			WithProviderAttemptRecorder(attempts.record),
			WithProviderRetryPolicy(corePolicy),
		)
		loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)

		_, err := loop.Run(context.Background(), Submission{Content: "run the action once"})
		require.NoError(t, err)
		require.EqualValues(t, 3, requests.Load(), "one retry should recover, then the tool turn should make one follow-up request")
		require.EqualValues(t, 3, userContentCount.Load(), "each provider request must contain the submitted user message exactly once")
		require.EqualValues(t, 1, tool.calls.Load(), "the successful retry's tool call should execute once")
		require.Equal(t, []int{0}, corePolicy.calls(), "the Core operation owner should decide the one retry")
		require.Empty(t, middlewarePolicy.calls(), "active provider operations must stay single-pass in retry middleware")

		results := attempts.snapshot()
		require.Len(t, results, 3)
		require.Equal(t, llm.FailureTransientProvider, results[0].Failure)
		require.Equal(t, llm.AttemptSuccess, results[1].Outcome)
		require.Equal(t, results[0].OperationID, results[1].OperationID, "retry attempts must share the operation")
		require.NotEqual(t, results[0].AttemptID, results[1].AttemptID)
	})
}

func TestLoopDoesNotReplaySSEFailureAfterPublishingText(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{"role":"assistant"}}]}`)
		writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{"content":"published partial"}}]}`)
		writeSSERecoveryFrame(t, w, sseRecoveryRateLimitEnvelope)
		writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{"content":"must not be accepted after error"}}]}`)
		writeSSERecoveryFrame(t, w, "[DONE]")
	}))
	defer server.Close()

	model, cleanup := sseRecoveryModel(t, server)
	defer cleanup()
	policy := &operationControlledRetryPolicy{allowedRetries: 3}
	loop := NewLoopAgent(
		WithLLM(model),
		WithMaxIterations(1),
		WithProviderRetryPolicy(policy),
	)
	loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
	stream := &collectingEventStream{}
	_, err := loop.RunCanonical(context.Background(), Submission{Content: "request"}, stream)
	require.Error(t, err)
	require.EqualValues(t, 1, requests.Load(), "already published output must not be replayed")
	require.Empty(t, policy.calls(), "visible output must stop Core from asking for a retry")
	var published []AgentEvent
	for _, event := range stream.collected() {
		if event.Kind == "message" && event.Incremental {
			published = append(published, event)
		}
	}
	require.Len(t, published, 1)
	require.Equal(t, "published partial", published[0].Content)
	require.NotContains(t, err.Error(), "diagnostic-secret-canary")
}

func TestLoopFailsClosedOnNonTransientAndUnknownSSEErrors(t *testing.T) {
	cases := []struct {
		name        string
		envelope    string
		wantType    llm.ErrorType
		wantFailure llm.FailureClass
		malformed   bool
	}{
		{name: "authentication", envelope: `{"error":{"message":"invalid API key secret-canary","type":"authentication_error","code":"invalid_api_key"}}`, wantType: llm.ErrTypeAuth, wantFailure: llm.FailureAuth},
		{name: "quota", envelope: `{"error":{"message":"insufficient_quota secret-canary","type":"insufficient_quota","code":"insufficient_quota"}}`, wantType: llm.ErrorType("quota"), wantFailure: llm.FailureOther},
		{name: "invalid request", envelope: `{"error":{"message":"model not found secret-canary","type":"invalid_request_error","code":"model_not_found"}}`, wantType: llm.ErrTypeInvalidRequest, wantFailure: llm.FailureInvalidRequest},
		{name: "unknown quota message", envelope: `{"error":{"message":"quota exceeded secret-canary","type":"future_error","code":"future_code"}}`, wantType: llm.ErrTypeProtocol, wantFailure: llm.FailureProtocol},
		{name: "unknown overflow message", envelope: `{"error":{"message":"context window exceeded secret-canary","type":"future_error","code":"future_code"}}`, wantType: llm.ErrTypeProtocol, wantFailure: llm.FailureProtocol},
		{name: "malformed code shape", envelope: `{"error":{"message":"rate limit secret-canary","type":"rate_limit_error","code":{}}}`, wantType: llm.ErrTypeProtocol, wantFailure: llm.FailureProtocol},
		{name: "unrecognized API error", envelope: `{"error":{"message":"provider rejected the request secret-canary","type":"api_error","code":null}}`, wantType: llm.ErrorType("protocol"), wantFailure: llm.FailureProtocol},
		{name: "malformed event", malformed: true, wantFailure: llm.FailureProtocol},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{"role":"assistant"}}]}`)
				if test.malformed {
					writeSSERecoveryFrame(t, w, `{malformed event`)
				} else {
					writeSSERecoveryFrame(t, w, test.envelope)
				}
				writeSSERecoveryFrame(t, w, "[DONE]")
			}))
			defer server.Close()

			model, cleanup := sseRecoveryModel(t, server)
			defer cleanup()
			attempts := &attemptResultRecorder{}
			policy := &operationControlledRetryPolicy{allowedRetries: 3}
			loop := NewLoopAgent(
				WithLLM(attemptGuardedModel{BaseChatModel: model}),
				WithMaxIterations(1),
				WithProviderAttemptRecorder(attempts.record),
				WithProviderRetryPolicy(policy),
			)
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			_, err := loop.Run(context.Background(), Submission{Content: "request"})
			require.Error(t, err)
			require.EqualValues(t, 1, requests.Load())
			require.Empty(t, policy.calls(), "non-transient provider and protocol failures must be rejected before policy evaluation")
			require.NotContains(t, err.Error(), "secret-canary")
			results := attempts.snapshot()
			require.Len(t, results, 1)
			require.Equal(t, test.wantFailure, results[0].Failure)
			if test.wantType != "" {
				var providerErr *llm.ProviderError
				require.ErrorAs(t, err, &providerErr)
				require.Equal(t, test.wantType, providerErr.ErrorType)
				require.Equal(t, http.StatusOK, providerErr.StatusCode)
			}
		})
	}
}

func TestLoopSSEErrorPreservesPartialUsageCoverage(t *testing.T) {
	for _, test := range []struct {
		name     string
		activity string
		coverage llm.PrimaryUsageCoverage
	}{
		{name: "no output", coverage: llm.UsageCoverageUnknown},
		{name: "partial output", activity: `{"choices":[{"delta":{"content":"partial"}}]}`, coverage: llm.UsageCoveragePartial},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSERecoveryFrame(t, w, `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":0,"total_tokens":7}}`)
				if test.activity != "" {
					writeSSERecoveryFrame(t, w, test.activity)
				}
				writeSSERecoveryFrame(t, w, sseRecoveryRateLimitEnvelope)
				writeSSERecoveryFrame(t, w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
				writeSSERecoveryFrame(t, w, "[DONE]")
			}))
			defer server.Close()

			model, cleanup := sseRecoveryModel(t, server)
			defer cleanup()
			attempts := &attemptResultRecorder{}
			policy := &operationControlledRetryPolicy{allowedRetries: 0}
			loop := NewLoopAgent(
				WithLLM(attemptGuardedModel{BaseChatModel: model}),
				WithMaxIterations(1),
				WithProviderAttemptRecorder(attempts.record),
				WithProviderRetryPolicy(policy),
			)
			loop.SetMidTurnCompaction(nil, nil, compaction.NewHeuristicTokenEstimator(), 50000)
			_, err := loop.Run(context.Background(), Submission{Content: "request"})
			require.Error(t, err)
			require.EqualValues(t, 1, requests.Load())
			results := attempts.snapshot()
			require.Len(t, results, 1)
			require.Equal(t, llm.UsageKnown, results[0].Usage.State)
			require.NotNil(t, results[0].Usage.Usage)
			require.Equal(t, 7, results[0].Usage.Usage.InputTokens)
			require.Equal(t, 0, results[0].Usage.Usage.OutputTokens)
			require.Equal(t, test.coverage, results[0].Usage.Usage.PrimaryCoverage)
		})
	}
}

func sseRecoveryModel(t *testing.T, server *httptest.Server) (llm.BaseChatModel, func()) {
	t.Helper()
	registry := llm.NewProviderRegistry()
	require.NoError(t, registry.Register(llm.NewEinoProvider(
		llm.WithProviderName("sse-recovery-fixture"),
		llm.WithHTTPClient(server.Client()),
		llm.WithModels([]llm.ModelInfo{{Name: "sse-recovery-model"}}),
	)))
	resolved, err := registry.SnapshotResolver().ResolveModel(context.Background(), llm.ModelRequest{
		ProviderID: "sse-recovery-fixture",
		ModelID:    "sse-recovery-model",
		Endpoint:   server.URL,
	})
	require.NoError(t, err)
	model, cleanup, err := resolved.Build(context.Background())
	require.NoError(t, err)
	return model, cleanup
}

func writeSSERecoveryFrame(t *testing.T, w http.ResponseWriter, payload string) {
	t.Helper()
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("write SSE frame: %v", err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
