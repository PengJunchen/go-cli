package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type schemaDecoderHITLEmitter struct {
	questions []tools.HITLQuestionEvent
}

func (e *schemaDecoderHITLEmitter) Emit(_ context.Context, event tools.HITLQuestionEvent) error {
	e.questions = append(e.questions, tools.HITLQuestionEvent{
		QuestionID: event.QuestionID,
		Question:   event.Question,
		Options:    append([]string(nil), event.Options...),
	})
	event.ResponseCh <- tools.HITLAnswer{QuestionID: event.QuestionID, Answer: "confirmed"}
	return nil
}

func TestExtraToolSchemaMixedProviderDecodersReachPreparedExecutor(t *testing.T) {
	ctx := context.Background()
	emitter := &schemaDecoderHITLEmitter{}
	base := tools.NewDefaultToolRegistry()
	tool := tools.NewAskUserQuestionTool(emitter, time.Second)
	require.NoError(t, base.Register(ctx, tool))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	var policyCalls atomic.Int32
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		policyCalls.Add(1)
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	proxy, err := registry.Get(ctx, "ask_user")
	require.NoError(t, err)
	parameterized, ok := proxy.(tools.Parameterized)
	require.True(t, ok, "prepared registration must retain the schema capability")
	schema := parameterized.Parameters()

	// The OpenAI-compatible request serializer receives the exact schema
	// captured by the protected registry, and its response decoder produces the
	// JSON-decoded argument shape consumed by the prepared tool path.
	openAIOptions := []Option{WithTools([]ToolDefinition{{Name: "ask_user", Parameters: schema}})}
	compatibleBody, err := (&HTTPChatModel{cfg: ModelConfig{Model: "fixture"}}).buildBody(
		[]Message{{Role: RoleUser, Content: "choose"}}, openAIOptions...,
	)
	require.NoError(t, err)
	var compatibleRequest openAIRequest
	require.NoError(t, json.Unmarshal(compatibleBody, &compatibleRequest))
	require.Len(t, compatibleRequest.Tools, 1)
	assert.Equal(t, normalizeSchemaJSON(t, schema), normalizeSchemaJSON(t, compatibleRequest.Tools[0].Function.Parameters))

	nativeBody, err := encodeOpenAIRequest(
		ModelConfig{Model: "fixture"}, "fixture",
		[]Message{{Role: RoleUser, Content: "choose"}}, openAIOptions,
	)
	require.NoError(t, err)
	var nativeRequest openAIRequest
	require.NoError(t, json.Unmarshal(nativeBody, &nativeRequest))
	require.Len(t, nativeRequest.Tools, 1)
	assert.Equal(t, normalizeSchemaJSON(t, schema), normalizeSchemaJSON(t, nativeRequest.Tools[0].Function.Parameters))

	dispatch := func(providerName string, call ToolCall) {
		t.Helper()
		args, ok := call.Args.(map[string]any)
		require.Truef(t, ok, "%s decoder must produce an argument object, got %T", providerName, call.Args)
		result, dispatchErr := proxy.Execute(ctx, tools.ToolCall{ID: call.ID, Name: call.Name, Args: args})
		require.NoErrorf(t, dispatchErr, "%s valid decoded call", providerName)
		require.NotNilf(t, result, "%s result", providerName)
		assert.Equal(t, "confirmed", result.Output)
	}

	openAIValid := decodeOpenAIAskUserCall(t, map[string]any{"question": "Choose", "options": []string{"red", "blue"}})
	dispatch("OpenAI-compatible", openAIValid)

	// Use a local server for Anthropic's actual streamed tool_use decoder.
	var claudeBody atomic.Value
	claudeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(claudeBody.Load().(string)))
	}))
	t.Cleanup(claudeServer.Close)
	claudeModel, _, err := NewClaudeProvider(WithNativeBaseURL(claudeServer.URL)).Build(ctx, ModelConfig{Model: "fixture"})
	require.NoError(t, err)
	claudeBody.Store(claudeToolUseSSE(t, map[string]any{"question": "Choose", "options": []string{"red", "blue"}}))
	claudeValid := decodeClaudeAskUserCall(t, claudeModel)
	dispatch("Anthropic native", claudeValid)

	require.Equal(t, int32(2), policyCalls.Load())
	require.Len(t, emitter.questions, 2)
	for _, question := range emitter.questions {
		assert.Equal(t, "Choose", question.Question)
		assert.Equal(t, []string{"red", "blue"}, question.Options)
	}

	// Both decoders preserve malformed-for-schema array members as JSON values;
	// validation rejects these before asking policy or emitting an HITL event.
	openAIBad := decodeOpenAIAskUserCall(t, map[string]any{"question": "Choose", "options": []any{"red", 7}})
	assertPreparedRejectsBeforePolicy(t, ctx, proxy, openAIBad, &policyCalls)
	claudeBody.Store(claudeToolUseSSE(t, map[string]any{"question": "Choose", "options": []any{"red", 7}}))
	claudeBad := decodeClaudeAskUserCall(t, claudeModel)
	assertPreparedRejectsBeforePolicy(t, ctx, proxy, claudeBad, &policyCalls)
	assert.Equal(t, int32(2), policyCalls.Load())
	assert.Len(t, emitter.questions, 2, "rejected calls must not reach the executor")
}

func decodeOpenAIAskUserCall(t *testing.T, args map[string]any) ToolCall {
	t.Helper()
	encodedArgs, err := json.Marshal(args)
	require.NoError(t, err)
	body, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id":   "call_openai_fixture",
					"type": "function",
					"function": map[string]any{
						"name":      "ask_user",
						"arguments": string(encodedArgs),
					},
				}},
			},
		}},
	})
	require.NoError(t, err)
	decoded, err := decodeOpenAIResponse(body)
	require.NoError(t, err)
	require.Len(t, decoded.ToolCalls, 1)
	return decoded.ToolCalls[0]
}

func decodeClaudeAskUserCall(t *testing.T, model BaseChatModel) ToolCall {
	t.Helper()
	stream, err := model.Stream(context.Background(), []Message{{Role: RoleUser, Content: "choose"}})
	require.NoError(t, err)
	var toolCalls []ToolCall
	for chunk := range stream {
		if chunk.Error != nil {
			require.NoError(t, chunk.Error)
		}
		if chunk.Final {
			toolCalls = chunk.ToolCalls
		}
	}
	require.Len(t, toolCalls, 1)
	return toolCalls[0]
}

func claudeToolUseSSE(t *testing.T, args map[string]any) string {
	t.Helper()
	encodedArgs, err := json.Marshal(args)
	require.NoError(t, err)
	delta, err := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": string(encodedArgs),
		},
	})
	require.NoError(t, err)
	return claudeSSEBody(t, []string{
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_claude_fixture","name":"ask_user"}}`,
		string(delta),
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}`,
		`{"type":"message_stop"}`,
	})
}

func assertPreparedRejectsBeforePolicy(t *testing.T, ctx context.Context, proxy tools.ToolDefinition, call ToolCall, policyCalls *atomic.Int32) {
	t.Helper()
	args, ok := call.Args.(map[string]any)
	require.True(t, ok)
	before := policyCalls.Load()
	_, err := proxy.Execute(ctx, tools.ToolCall{ID: call.ID, Name: call.Name, Args: args})
	require.Error(t, err)
	assert.Equal(t, before, policyCalls.Load(), "invalid arguments must fail before prepared policy")
}

func normalizeSchemaJSON(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var normalized any
	require.NoError(t, json.Unmarshal(encoded, &normalized))
	return normalized
}
