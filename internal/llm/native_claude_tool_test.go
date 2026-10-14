package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeToolUseGenerateAndStreamParity(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			System    string `json:"system"`
			MaxTokens int    `json:"max_tokens"`
			Stream    bool   `json:"stream"`
			Tools     []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"input_schema"`
			} `json:"tools"`
		}
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &request))
		requests.Add(1)
		assert.Equal(t, "system guidance", request.System)
		assert.Equal(t, 77, request.MaxTokens)
		require.Len(t, request.Tools, 1)
		assert.Equal(t, "lookup", request.Tools[0].Name)
		assert.Equal(t, "Find a record", request.Tools[0].Description)
		assert.Equal(t, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key": map[string]any{"type": "string"},
			},
			"required": []any{"key"},
		}, request.Tools[0].InputSchema)
		assert.NotContains(t, string(raw), `"function"`)

		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, claudeSSEBody(t, []string{
				`{"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":12}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking. "}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"lookup-1","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"key\":"}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"alpha\"}"}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"lookup-2","name":"lookup","input":{}}}`,
				`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"key\":\"beta\"}"}}`,
				`{"type":"content_block_stop","index":2}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
				`{"type":"message_stop"}`,
			}))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, claudeTwoToolUseResponse)
	}))
	defer server.Close()

	model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	tools := []ToolDefinition{{
		Name:        "lookup",
		Description: "Find a record",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"key": map[string]any{"type": "string"}},
			"required":   []string{"key"},
		},
	}}
	msgs := []Message{
		{Role: RoleSystem, Content: "system guidance"},
		{Role: RoleUser, Content: "look up alpha and beta"},
	}
	options := []Option{WithTools(tools), WithMaxTokens(77)}
	generated, err := model.Generate(context.Background(), msgs, options...)
	require.NoError(t, err)
	require.Equal(t, RoleAssistant, generated.Role)
	assert.Equal(t, "Checking. ", generated.Content)
	assert.Equal(t, "tool_use", generated.FinishReason)
	require.Equal(t, []ToolCall{
		{ID: "lookup-1", Name: "lookup", Args: map[string]any{"key": "alpha"}},
		{ID: "lookup-2", Name: "lookup", Args: map[string]any{"key": "beta"}},
	}, generated.ToolCalls)
	require.Equal(t, &Usage{InputTokens: 12, OutputTokens: 5, TotalTokens: 17}, generated.Usage)

	stream, err := model.Stream(context.Background(), msgs, options...)
	require.NoError(t, err)
	var streamedText strings.Builder
	var streamedFinal *MessageChunk
	for chunk := range stream {
		require.NoError(t, chunk.Error)
		streamedText.WriteString(chunk.Content)
		if chunk.Final {
			copy := chunk
			streamedFinal = &copy
		}
	}
	require.NotNil(t, streamedFinal)
	assert.Equal(t, generated.Content, streamedText.String())
	assert.Equal(t, generated.ToolCalls, streamedFinal.ToolCalls)
	assert.Equal(t, generated.FinishReason, streamedFinal.FinishReason)
	assert.Equal(t, generated.Usage, streamedFinal.Usage)
	assert.EqualValues(t, 2, requests.Load())
}

const claudeTwoToolUseResponse = `{"role":"assistant","content":[{"type":"text","text":"Checking. "},{"type":"tool_use","id":"lookup-1","name":"lookup","input":{"key":"alpha"}},{"type":"tool_use","id":"lookup-2","name":"lookup","input":{"key":"beta"}}],"stop_reason":"tool_use","usage":{"input_tokens":12,"output_tokens":5}}`

func TestClaudeGenerateClosesToolResultsByCallIDOnSecondRequest(t *testing.T) {
	var requests atomic.Int32
	secondRequestBody := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestIndex := requests.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		if requestIndex == 2 {
			var secondRequest map[string]any
			require.NoError(t, json.Unmarshal(body, &secondRequest))
			secondRequestBody <- secondRequest
		}
		w.Header().Set("Content-Type", "application/json")
		if requestIndex == 1 {
			writeResponse(t, w, claudeTwoToolUseResponse)
			return
		}
		writeResponse(t, w, `{"role":"assistant","content":[{"type":"text","text":"all done"}],"stop_reason":"end_turn"}`)
	}))
	defer server.Close()

	model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	definitions := []ToolDefinition{{Name: "lookup", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}}
	first, err := model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "lookup two keys"}}, WithTools(definitions))
	require.NoError(t, err)
	require.Len(t, first.ToolCalls, 2)
	_, err = model.Generate(context.Background(), []Message{
		{Role: RoleUser, Content: "lookup two keys"},
		{Role: RoleAssistant, Content: first.Content, ToolCalls: first.ToolCalls},
		{Role: RoleTool, ToolCallID: "lookup-2", Name: "lookup", Content: "result beta"},
		{Role: RoleTool, ToolCallID: "lookup-1", Name: "lookup", Content: "result alpha"},
	}, WithTools(definitions))
	require.NoError(t, err)
	assert.EqualValues(t, 2, requests.Load())
	secondRequest := <-secondRequestBody

	messages, ok := secondRequest["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 3)
	assistant, ok := messages[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "assistant", assistant["role"])
	assistantContent, ok := assistant["content"].([]any)
	require.True(t, ok)
	require.Len(t, assistantContent, 3)
	assert.Equal(t, "text", assistantContent[0].(map[string]any)["type"])
	assert.Equal(t, "lookup-1", assistantContent[1].(map[string]any)["id"])
	assert.Equal(t, "lookup-2", assistantContent[2].(map[string]any)["id"])

	user, ok := messages[2].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", user["role"])
	results, ok := user["content"].([]any)
	require.True(t, ok)
	require.Len(t, results, 2)
	assert.Equal(t, "tool_result", results[0].(map[string]any)["type"])
	assert.Equal(t, "lookup-1", results[0].(map[string]any)["tool_use_id"])
	assert.Equal(t, "result alpha", results[0].(map[string]any)["content"])
	assert.Equal(t, "lookup-2", results[1].(map[string]any)["tool_use_id"])
	assert.Equal(t, "result beta", results[1].(map[string]any)["content"])
	for _, message := range messages {
		assert.NotEqual(t, "tool", message.(map[string]any)["role"])
	}
}

func TestClaudeGenerateRejectsInvalidResponseBeforeReturningToolCalls(t *testing.T) {
	canary := "claude-response-canary-7f29"
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	tests := []struct {
		name string
		body string
	}{
		{name: "missing role", body: `{"content":[{"type":"tool_use","id":"id-1","name":"lookup","input":{"key":"` + canary + `"}}],"stop_reason":"tool_use"}`},
		{name: "wrong role", body: `{"role":"user","content":[{"type":"tool_use","id":"id-1","name":"lookup","input":{"key":"` + canary + `"}}],"stop_reason":"tool_use"}`},
		{name: "missing terminal reason", body: `{"role":"assistant","content":[{"type":"text","text":"` + canary + `"}]}`},
		{name: "empty tool id", body: `{"role":"assistant","content":[{"type":"tool_use","id":"","name":"lookup","input":{}}],"stop_reason":"tool_use"}`},
		{name: "duplicate tool id", body: `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"lookup","input":{}},{"type":"tool_use","id":"id-1","name":"lookup","input":{}}],"stop_reason":"tool_use"}`},
		{name: "empty tool name", body: `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"","input":{}}],"stop_reason":"tool_use"}`},
		{name: "input missing", body: `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"lookup"}],"stop_reason":"tool_use"}`},
		{name: "input null", body: `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"lookup","input":null}],"stop_reason":"tool_use"}`},
		{name: "input array", body: `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"lookup","input":["` + canary + `"]}],"stop_reason":"tool_use"}`},
		{name: "input scalar", body: `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"lookup","input":"` + canary + `"}],"stop_reason":"tool_use"}`},
		{name: "invalid JSON", body: `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"lookup","input":{"key":"` + canary + `"}],"stop_reason":"tool_use"`},
		{name: "tool use with non-tool stop reason", body: `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"lookup","input":{}}],"stop_reason":"end_turn"}`},
		{name: "tool stop reason without tool use", body: `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"tool_use"}`},
		{name: "text after tool use", body: `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"lookup","input":{}},{"type":"text","text":"` + canary + `"}],"stop_reason":"tool_use"}`},
		{name: "thinking block", body: `{"role":"assistant","content":[{"type":"thinking","thinking":"` + canary + `"}],"stop_reason":"end_turn"}`},
		{name: "redacted thinking block", body: `{"role":"assistant","content":[{"type":"redacted_thinking","data":"` + canary + `"}],"stop_reason":"end_turn"}`},
		{name: "server tool block", body: `{"role":"assistant","content":[{"type":"server_tool_use","id":"id-1","name":"web_search","input":{"query":"` + canary + `"}}],"stop_reason":"tool_use"}`},
		{name: "unknown block", body: `{"role":"assistant","content":[{"type":"opaque_custom","value":"` + canary + `"}],"stop_reason":"end_turn"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writeResponse(t, w, tt.body)
			}))
			defer server.Close()
			model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
			require.NoError(t, err)
			_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "run"}})
			require.Error(t, err)
			assert.EqualValues(t, 1, requests.Load())
			assert.NotContains(t, err.Error(), canary)
			assert.NotContains(t, logs.String(), canary)
		})
	}
}

func TestClaudeRejectsInvalidToolSchemasBeforeHTTP(t *testing.T) {
	tests := []struct {
		name   string
		schema any
		tool   string
	}{
		{name: "scalar", schema: "schema-canary-scalar"},
		{name: "array", schema: []any{"schema-canary-array"}},
		{name: "null", schema: json.RawMessage("null")},
		{name: "unmarshalable", schema: map[string]any{"secret": "schema-canary-secret", "bad": make(chan int)}},
		{name: "invalid name", schema: map[string]any{"type": "object"}, tool: "bad tool name"},
		{name: "too-long name", schema: map[string]any{"type": "object"}, tool: strings.Repeat("a", 129)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writeResponse(t, w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
			}))
			defer server.Close()
			model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
			require.NoError(t, err)
			name := tt.tool
			if name == "" {
				name = "lookup"
			}
			_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "run"}}, WithTools([]ToolDefinition{{Name: name, Parameters: tt.schema}}))
			require.Error(t, err)
			assert.Zero(t, requests.Load(), "invalid schemas and names must fail before HTTP")
			assert.NotContains(t, err.Error(), "schema-canary")
		})
	}

	var requests atomic.Int32
	requestBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		requestBody <- body
		writeResponse(t, w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	}))
	defer server.Close()
	model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	maxLengthName := strings.Repeat("a", 128)
	_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "run"}}, WithTools([]ToolDefinition{{Name: maxLengthName}}))
	require.NoError(t, err)
	assert.EqualValues(t, 1, requests.Load(), "nil schema retains the empty object default")
	var encoded struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(<-requestBody, &encoded))
	require.Len(t, encoded.Tools, 1)
	assert.Equal(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}, encoded.Tools[0].InputSchema)
	assert.Equal(t, maxLengthName, encoded.Tools[0].Name)
}

func TestClaudeRejectsUnclosedToolHistoryBeforeHTTP(t *testing.T) {
	tests := []struct {
		name     string
		messages []Message
	}{
		{name: "missing result", messages: []Message{{Role: RoleUser, Content: "run"}, {Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Args: map[string]any{}}}}}},
		{name: "missing one result", messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Args: map[string]any{}}, {ID: "call-2", Name: "lookup", Args: map[string]any{}}}}, {Role: RoleTool, ToolCallID: "call-1", Name: "lookup", Content: "ok"}}},
		{name: "unknown result id", messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Args: map[string]any{}}}}, {Role: RoleTool, ToolCallID: "foreign", Name: "lookup", Content: "ok"}}},
		{name: "missing result id", messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Args: map[string]any{}}}}, {Role: RoleTool, Name: "lookup", Content: "ok"}}},
		{name: "name mismatch", messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Args: map[string]any{}}}}, {Role: RoleTool, ToolCallID: "call-1", Name: "other", Content: "ok"}}},
		{name: "duplicate call id", messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Args: map[string]any{}}, {ID: "call-1", Name: "lookup", Args: map[string]any{}}}}}},
		{name: "non-object arguments", messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Args: []any{"args-canary"}}}}, {Role: RoleTool, ToolCallID: "call-1", Name: "lookup", Content: "ok"}}},
		{name: "unmarshalable arguments", messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Args: map[string]any{"secret": "args-canary", "bad": make(chan int)}}}}, {Role: RoleTool, ToolCallID: "call-1", Name: "lookup", Content: "ok"}}},
		{name: "floating tool result", messages: []Message{{Role: RoleUser, Content: "run"}, {Role: RoleTool, ToolCallID: "call-1", Content: "args-canary"}}},
		{name: "extra result", messages: []Message{{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup", Args: map[string]any{}}}}, {Role: RoleTool, ToolCallID: "call-1", Name: "lookup", Content: "ok"}, {Role: RoleTool, ToolCallID: "call-1", Name: "lookup", Content: "duplicate"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writeResponse(t, w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
			}))
			defer server.Close()
			model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
			require.NoError(t, err)
			_, err = model.Generate(context.Background(), tt.messages)
			require.Error(t, err)
			assert.Zero(t, requests.Load())
			assert.NotContains(t, err.Error(), "args-canary")
		})
	}
}

func TestClaudeEncodesNilToolArgumentsAsEmptyObject(t *testing.T) {
	requestBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody <- body
		writeResponse(t, w, `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	}))
	defer server.Close()
	model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	_, err = model.Generate(context.Background(), []Message{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call-1", Name: "lookup"}}},
		{Role: RoleTool, ToolCallID: "call-1", Name: "lookup", Content: "result"},
	})
	require.NoError(t, err)
	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type  string         `json:"type"`
				Input map[string]any `json:"input"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(<-requestBody, &request))
	require.Len(t, request.Messages, 2)
	require.Len(t, request.Messages[0].Content, 1)
	assert.Equal(t, "tool_use", request.Messages[0].Content[0].Type)
	assert.Empty(t, request.Messages[0].Content[0].Input)
}

func TestClaudeRejectsUnsupportedInputContentBeforeHTTP(t *testing.T) {
	tests := []struct {
		name  string
		block ContentBlock
	}{
		{name: "unknown content block", block: ContentBlock{Type: "thinking", Text: "block-canary"}},
		{name: "image without source", block: ContentBlock{Type: "image_url"}},
		{name: "remote image", block: ContentBlock{Type: "image_url", ImageURL: &ImageURL{URL: "https://example.invalid/image.png"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				writeResponse(t, w, `{"role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
			}))
			defer server.Close()
			model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
			require.NoError(t, err)
			_, err = model.Generate(context.Background(), []Message{{Role: RoleUser, ContentBlocks: []ContentBlock{tt.block}}})
			require.Error(t, err)
			assert.Zero(t, requests.Load())
			assert.NotContains(t, err.Error(), "block-canary")
		})
	}
}

func TestClaudePreservesLargeIntegerToolArguments(t *testing.T) {
	var requests atomic.Int32
	secondRequest := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			writeResponse(t, w, `{"role":"assistant","content":[{"type":"tool_use","id":"id-1","name":"lookup","input":{"count":9007199254740993}}],"stop_reason":"tool_use"}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		secondRequest <- body
		writeResponse(t, w, `{"role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	}))
	defer server.Close()
	model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	response, err := model.Generate(context.Background(), []Message{{Role: RoleUser, Content: "run"}})
	require.NoError(t, err)
	require.Len(t, response.ToolCalls, 1)
	args, ok := response.ToolCalls[0].Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, json.Number("9007199254740993"), args["count"])
	_, err = model.Generate(context.Background(), []Message{
		{Role: RoleUser, Content: "run"},
		{Role: RoleAssistant, ToolCalls: response.ToolCalls},
		{Role: RoleTool, ToolCallID: "id-1", Name: "lookup", Content: "counted"},
	})
	require.NoError(t, err)
	assert.EqualValues(t, 2, requests.Load())
	assert.Contains(t, string(<-secondRequest), `"count":9007199254740993`)
}

func TestClaudeStreamPreservesSchemaNumbersWithThinking(t *testing.T) {
	body := claudeSSEBody(t, []string{
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
		`{"type":"message_stop"}`,
	})
	requestBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		requestBody <- raw
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	model, _, err := NewClaudeProvider(WithNativeBaseURL(server.URL)).Build(context.Background(), ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": map[string]any{"type": "integer", "const": json.Number("9007199254740993")},
		},
	}
	stream, err := model.Stream(context.Background(), []Message{{Role: RoleUser, Content: "run"}},
		WithTools([]ToolDefinition{{Name: "lookup", Parameters: schema}}),
		WithThinking(ThinkingConfigForLevel(ThinkingHigh)),
	)
	require.NoError(t, err)
	var sawFinal bool
	for chunk := range stream {
		require.NoError(t, chunk.Error)
		sawFinal = sawFinal || chunk.Final
	}
	assert.True(t, sawFinal)
	raw := <-requestBody
	assert.Contains(t, string(raw), `"const":9007199254740993`)
	assert.Contains(t, string(raw), `"stream":true`)
	assert.Contains(t, string(raw), `"budget_tokens":16384`)
}

func TestClaudeStreamRejectsMalformedToolTerminalsWithoutFinal(t *testing.T) {
	base := []string{
		`{"type":"message_start","message":{"role":"assistant"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool-1","name":"lookup","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"value\":\"stream-canary"}}`,
		`{"type":"content_block_stop","index":1}`,
	}
	tests := []struct {
		name   string
		events []string
	}{
		{name: "tool calls with end turn", events: append(append([]string(nil), base...), `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, `{"type":"message_stop"}`)},
		{name: "tool stop reason without calls", events: []string{`{"type":"message_start","message":{"role":"assistant"}}`, `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, `{"type":"message_stop"}`}},
		{name: "incomplete JSON input", events: append(append([]string(nil), base...), `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, `{"type":"message_stop"}`)},
		{name: "trailing JSON input", events: append(append(append(append([]string(nil), base[:5]...), `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{} {}"}}`), base[6:]...), `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, `{"type":"message_stop"}`)},
		{name: "text after tool use", events: append(append([]string(nil), base...), `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`, `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`, `{"type":"message_stop"}`)},
		{name: "missing terminal event", events: append(append([]string(nil), base...), `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`)},
		{name: "unknown event", events: append(append([]string(nil), base...), `{"type":"opaque_event","secret":"stream-canary"}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chunks := collectClaudeStream(t, tt.events)
			var streamErr error
			var sawFinal bool
			for _, chunk := range chunks {
				if chunk.Error != nil {
					streamErr = chunk.Error
				}
				sawFinal = sawFinal || chunk.Final
			}
			require.Error(t, streamErr)
			assert.False(t, sawFinal)
			assert.NotContains(t, streamErr.Error(), "stream-canary")
		})
	}
}
