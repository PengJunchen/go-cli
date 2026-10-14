package core

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

type claudeLoopbackTool struct {
	mu    sync.Mutex
	calls map[string]int
}

type claudeLoopbackLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (capture *claudeLoopbackLogCapture) Write(data []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.buf.Write(data)
}

func (capture *claudeLoopbackLogCapture) String() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.buf.String()
}

func (*claudeLoopbackTool) Name() string        { return "record" }
func (*claudeLoopbackTool) Description() string { return "Record a keyed value" }
func (*claudeLoopbackTool) Parameters() any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"required":   []string{"value"},
	}
}
func (tool *claudeLoopbackTool) Execute(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	value, ok := call.Args["value"].(string)
	if !ok {
		return nil, errInvalidClaudeLoopbackToolArgs
	}
	tool.mu.Lock()
	tool.calls[value]++
	tool.mu.Unlock()
	return &tools.ToolResult{Output: "result-" + value}, nil
}

var errInvalidClaudeLoopbackToolArgs = &claudeLoopbackToolError{}

type claudeLoopbackToolError struct{}

func (*claudeLoopbackToolError) Error() string { return "invalid loopback tool arguments" }

func (tool *claudeLoopbackTool) callCounts() map[string]int {
	tool.mu.Lock()
	defer tool.mu.Unlock()
	return map[string]int{"alpha": tool.calls["alpha"], "beta": tool.calls["beta"]}
}

func TestClaudeStreamToolUseLoopbackRunsEachToolOnce(t *testing.T) {
	var requests atomic.Int32
	var mu sync.Mutex
	var secondRequest map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if index == 2 {
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Errorf("decode second request: %v", err)
				return
			}
			mu.Lock()
			secondRequest = decoded
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if index == 1 {
			_, _ = io.WriteString(w, coreClaudeSSEBody(
				`{"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":14}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Processing."}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"record-alpha","name":"record","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"value\":\"alpha\"}"}}`,
				`{"type":"content_block_stop","index":1}`,
				`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"record-beta","name":"record","input":{}}}`,
				`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"value\":\"beta\"}"}}`,
				`{"type":"content_block_stop","index":2}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":4}}`,
				`{"type":"message_stop"}`,
			))
			return
		}
		_, _ = io.WriteString(w, coreClaudeSSEBody(
			`{"type":"message_start","message":{"role":"assistant","usage":{"input_tokens":30}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Finished."}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			`{"type":"message_stop"}`,
		))
	}))
	defer server.Close()

	model, _, err := llm.NewClaudeProvider(llm.WithNativeBaseURL(server.URL)).Build(context.Background(), llm.ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	tool := &claudeLoopbackTool{calls: make(map[string]int)}
	registry := mock.NewMockToolServer()
	require.NoError(t, registry.Register(context.Background(), tool))
	loop := NewLoopAgent(WithLLM(model), WithTools(registry), WithMaxIterations(5))
	result, err := loop.RunCanonical(context.Background(), Submission{Content: "record alpha and beta"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, requests.Load())
	assert.Equal(t, map[string]int{"alpha": 1, "beta": 1}, tool.callCounts())

	mu.Lock()
	request := secondRequest
	mu.Unlock()
	require.NotNil(t, request)
	messages, ok := request["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 3)
	assistant, ok := messages[1].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "assistant", assistant["role"])
	assistantBlocks := assistant["content"].([]any)
	require.Len(t, assistantBlocks, 3)
	assert.Equal(t, "record-alpha", assistantBlocks[1].(map[string]any)["id"])
	assert.Equal(t, "record-beta", assistantBlocks[2].(map[string]any)["id"])
	user := messages[2].(map[string]any)
	assert.Equal(t, "user", user["role"])
	results := user["content"].([]any)
	require.Len(t, results, 2)
	assert.Equal(t, "record-alpha", results[0].(map[string]any)["tool_use_id"])
	assert.Equal(t, "result-alpha", results[0].(map[string]any)["content"])
	assert.Equal(t, "record-beta", results[1].(map[string]any)["tool_use_id"])
	assert.Equal(t, "result-beta", results[1].(map[string]any)["content"])

	var assistantTexts []string
	toolReceipts := make(map[string]string)
	for _, entry := range result.Transcript {
		switch entry.Message.Role {
		case string(llm.RoleAssistant):
			assistantTexts = append(assistantTexts, entry.Message.Content)
		case string(llm.RoleTool):
			toolReceipts[entry.Message.ToolCallID] = entry.Message.Content
		}
	}
	assert.Equal(t, []string{"Processing.", "Finished."}, assistantTexts)
	assert.Equal(t, map[string]string{"record-alpha": "result-alpha", "record-beta": "result-beta"}, toolReceipts)
}

func TestClaudeMalformedStreamTerminalDoesNotDispatchTools(t *testing.T) {
	var logs claudeLoopbackLogCapture
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, coreClaudeSSEBody(
			`{"type":"message_start","message":{"role":"assistant"}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"record-canary-call","name":"record","input":{}}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"value\":\"alpha\"}"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"late text"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
			`{"type":"message_stop"}`,
		))
	}))
	defer server.Close()

	model, _, err := llm.NewClaudeProvider(llm.WithNativeBaseURL(server.URL)).Build(context.Background(), llm.ModelConfig{Model: "claude-test"})
	require.NoError(t, err)
	tool := &claudeLoopbackTool{calls: make(map[string]int)}
	registry := mock.NewMockToolServer()
	require.NoError(t, registry.Register(context.Background(), tool))
	loop := NewLoopAgent(WithLLM(model), WithTools(registry), WithMaxIterations(5))
	_, err = loop.RunCanonical(context.Background(), Submission{Content: "record alpha"})
	require.Error(t, err)
	assert.EqualValues(t, 1, requests.Load())
	assert.Equal(t, map[string]int{"alpha": 0, "beta": 0}, tool.callCounts())
	assert.NotContains(t, err.Error(), "record-canary-call")
	assert.NotContains(t, logs.String(), "record-canary-call")
}

func coreClaudeSSEBody(events ...string) string {
	var body strings.Builder
	for _, event := range events {
		var decoded struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(event), &decoded)
		body.WriteString("event: ")
		body.WriteString(decoded.Type)
		body.WriteString("\ndata: ")
		body.WriteString(event)
		body.WriteString("\n\n")
	}
	return body.String()
}
