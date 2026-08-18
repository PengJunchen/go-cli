package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// TestEinoStream_UsesDefaultSSEParser verifies that EinoProvider.Stream uses
// DefaultSSEParser to correctly handle standard SSE features: comment lines
// (starting with ':'), event: fields, multi-line data payloads, and the
// [DONE] sentinel.
func TestEinoStream_UsesDefaultSSEParser(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// The stream includes comments, event: fields, and multi-line data.
		// DefaultSSEParser must skip comments, accept event: fields, and join
		// multi-line data with "\n". The multi-line data splits JSON at a
		// valid whitespace boundary (after '[' so the join produces valid JSON).
		writeResponse(t, w, strings.Join([]string{
			": this is a comment",
			"",
			"event: delta",
			"data: {\"choices\":[",
			"data: {\"delta\":{\"content\":\"hello\"}}",
			"data: ]}",
			"",
			": another comment",
			"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}",
			"",
			"data: [DONE]",
			"",
		}, "\n"))
	}))
	defer srv.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	// Expected: role-only placeholder, "hello", "world", final.
	require.Len(t, chunks, 4)
	assert.Equal(t, RoleAssistant, chunks[0].Role)
	assert.Equal(t, "", chunks[0].Content) // role-only placeholder
	assert.Equal(t, "hello", chunks[1].Content)
	assert.Equal(t, "world", chunks[2].Content)
	assert.True(t, chunks[3].Final)
}

// TestEinoStream_JSONFallback_DetectNonSSE verifies that when the server
// returns a plain JSON response instead of an SSE stream (e.g. an error
// response), detectJSONResponse detects it and the response is parsed
// without panic.
func TestEinoStream_JSONFallback_DetectNonSSE(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeResponse(t, w, `{"choices":[{"message":{"role":"assistant","content":"not streamed"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	model := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	ch, err := model.Stream(context.Background(), nil)
	require.NoError(t, err)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}

	// JSON path: content chunk + final chunk (no role-only placeholder).
	require.Len(t, chunks, 2)
	assert.Equal(t, "not streamed", chunks[0].Content)
	assert.True(t, chunks[1].Final)
	// FinishReason must be carried on the final chunk (bug fix).
	assert.Equal(t, "stop", chunks[1].FinishReason)
}

// TestEinoStream_ToolCallAccumulation_Shared verifies that the same SSE input
// produces identical tool call results when processed by EinoProvider.Stream
// and native streamOpenAI, proving the shared accumulator is consistent.
func TestEinoStream_ToolCallAccumulation_Shared(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// SSE stream with tool-call fragments split across chunks.
	sseBody := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
		"",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"SF\"}"}}]}}]}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeResponse(t, w, sseBody)
	}))
	defer srv.Close()

	// Run through EinoProvider.
	einoModel := &HTTPChatModel{
		provider: NewEinoProvider(WithBaseURL(srv.URL)),
		cfg:      ModelConfig{Model: "m"},
		client:   http.DefaultClient,
	}
	einoCh, err := einoModel.Stream(context.Background(), nil)
	require.NoError(t, err)
	var einoChunks []MessageChunk
	for c := range einoCh {
		einoChunks = append(einoChunks, c)
	}

	// Run through native OpenAI provider.
	openAIProvider := NewOpenAIProvider(WithNativeBaseURL(srv.URL))
	nativeModel, _, err := openAIProvider.Build(context.Background(), ModelConfig{Model: "m"})
	require.NoError(t, err)
	nativeCh, err := nativeModel.Stream(context.Background(), nil)
	require.NoError(t, err)
	var nativeChunks []MessageChunk
	for c := range nativeCh {
		nativeChunks = append(nativeChunks, c)
	}

	// Both should have the same number of chunks.
	require.Len(t, einoChunks, len(nativeChunks))

	// Find the final chunk from each.
	var einoFinal, nativeFinal *MessageChunk
	for i := range einoChunks {
		if einoChunks[i].Final {
			einoFinal = &einoChunks[i]
		}
		if nativeChunks[i].Final {
			nativeFinal = &nativeChunks[i]
		}
	}
	require.NotNil(t, einoFinal)
	require.NotNil(t, nativeFinal)

	// Tool calls must be identical.
	require.Len(t, einoFinal.ToolCalls, 1)
	require.Len(t, nativeFinal.ToolCalls, 1)
	assert.Equal(t, einoFinal.ToolCalls[0].ID, nativeFinal.ToolCalls[0].ID)
	assert.Equal(t, einoFinal.ToolCalls[0].Name, nativeFinal.ToolCalls[0].Name)
	assert.Equal(t, einoFinal.ToolCalls[0].Args, nativeFinal.ToolCalls[0].Args)

	// Verify the accumulated tool call content.
	assert.Equal(t, "call_0", einoFinal.ToolCalls[0].ID)
	assert.Equal(t, "get_weather", einoFinal.ToolCalls[0].Name)
	args, ok := einoFinal.ToolCalls[0].Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "SF", args["city"])

	// FinishReason must match.
	assert.Equal(t, "tool_calls", einoFinal.FinishReason)
	assert.Equal(t, "tool_calls", nativeFinal.FinishReason)
}

// TestDetectJSONResponse_LeadingWhitespace verifies that detectJSONResponse
// correctly identifies a JSON response even when it has leading whitespace
// (spaces, tabs, newlines).
func TestDetectJSONResponse_LeadingWhitespace(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantJSON bool
	}{
		{
			name:     "leading spaces",
			input:    "   {\"key\":\"value\"}",
			wantJSON: true,
		},
		{
			name:     "leading newlines and tabs",
			input:    "\n\n\t  {\"key\":\"value\"}",
			wantJSON: true,
		},
		{
			name:     "leading carriage returns",
			input:    "\r\n\r\n  [1,2,3]",
			wantJSON: true,
		},
		{
			name:     "SSE stream (starts with data:)",
			input:    "data: {\"choices\":[]}\n\n",
			wantJSON: false,
		},
		{
			name:     "SSE comment (starts with :)",
			input:    ": comment\ndata: {\"choices\":[]}\n\n",
			wantJSON: false,
		},
		{
			name:     "empty input",
			input:    "",
			wantJSON: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := bufio.NewReader(bytes.NewReader([]byte(tt.input)))
			isJSON, body, err := detectJSONResponse(reader)
			require.NoError(t, err)
			assert.Equal(t, tt.wantJSON, isJSON)
			if tt.wantJSON {
				assert.NotEmpty(t, body)
				// Verify the body can be parsed as JSON.
				var v any
				require.NoError(t, json.Unmarshal(body, &v))
			}
		})
	}
}

// runStreamAccumulator feeds the given SSE event data payloads through
// accumulateOpenAIStreamToolCalls and returns the accumulated tool calls,
// finish reason, and emitted content chunks. A [DONE] sentinel is appended
// automatically.
func runStreamAccumulator(t *testing.T, eventData ...string) ([]ToolCall, string, []MessageChunk) {
	t.Helper()
	events := make(chan SSEEvent, len(eventData)+1)
	for _, data := range eventData {
		events <- SSEEvent{Data: data}
	}
	events <- SSEEvent{Data: "[DONE]"}
	close(events)

	ch := make(chan MessageChunk, 64)
	toolCalls, finishReason, _ := accumulateOpenAIStreamToolCalls(context.Background(), events, ch)
	close(ch)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	return toolCalls, finishReason, chunks
}

// TestStreamAccumulator_WithAPIIDsUsesRealID verifies that when the API
// returns a tool call ID in the stream delta, the accumulator preserves it
// instead of synthesizing a "call_%d" ID.
func TestStreamAccumulator_WithAPIIDsUsesRealID(t *testing.T) {
	toolCalls, _, _ := runStreamAccumulator(t,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "call_abc123", toolCalls[0].ID)
	assert.Equal(t, "get_weather", toolCalls[0].Name)
	args, ok := toolCalls[0].Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "SF", args["city"])
}

// TestStreamAccumulator_WithoutIDsFallsBackSynthetic verifies that when the
// stream never carries a tool call ID, the accumulator falls back to the
// synthetic "call_%d" scheme so downstream code still has a non-empty ID.
func TestStreamAccumulator_WithoutIDsFallsBackSynthetic(t *testing.T) {
	toolCalls, _, _ := runStreamAccumulator(t,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "call_0", toolCalls[0].ID)
	assert.Equal(t, "get_weather", toolCalls[0].Name)
}

// TestStreamAccumulator_MultipleToolCallsTracked verifies that tool call IDs
// are tracked independently per index when several tool calls are interleaved
// in the stream.
func TestStreamAccumulator_MultipleToolCallsTracked(t *testing.T) {
	toolCalls, _, _ := runStreamAccumulator(t,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""}},{"index":1,"id":"call_b","type":"function","function":{"name":"get_time","arguments":""}},{"index":2,"id":"call_c","type":"function","function":{"name":"search","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}},{"index":1,"function":{"arguments":"{\"zone\":\"PST\"}"}},{"index":2,"function":{"arguments":"{\"q\":\"go\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	require.Len(t, toolCalls, 3)
	assert.Equal(t, "call_a", toolCalls[0].ID)
	assert.Equal(t, "call_b", toolCalls[1].ID)
	assert.Equal(t, "call_c", toolCalls[2].ID)
	assert.Equal(t, "get_weather", toolCalls[0].Name)
	assert.Equal(t, "get_time", toolCalls[1].Name)
	assert.Equal(t, "search", toolCalls[2].Name)
}

// TestStreamAccumulator_FirstWriteWins verifies that once an ID is stored for
// an index, subsequent deltas for the same index do not overwrite it
// (first-write-wins semantics). This matches the behaviour already used for
// tool names.
func TestStreamAccumulator_FirstWriteWins(t *testing.T) {
	toolCalls, _, _ := runStreamAccumulator(t,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_second","function":{"arguments":"{\"city\":\"SF\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "call_first", toolCalls[0].ID)
}

// TestToolCallID_MatchesNonStreaming verifies that the streaming accumulator
// and the non-streaming conversion path (convertAssistantToolCalls) produce
// identical tool call IDs when the API returns the same IDs. This is the core
// invariant that lets tool results match tool calls regardless of whether the
// response was streamed.
func TestToolCallID_MatchesNonStreaming(t *testing.T) {
	// Streaming path: fragments arrive across SSE chunks with API IDs.
	streamCalls, _, _ := runStreamAccumulator(t,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	// Non-streaming path: the same tool call arrives in a single response.
	nonStreamCalls := convertAssistantToolCalls([]openAIToolCall{
		{ID: "call_abc123", Type: "function", Function: openAIFunction{Name: "get_weather", Arguments: `{"city":"SF"}`}},
	})

	require.Len(t, streamCalls, 1)
	require.Len(t, nonStreamCalls, 1)
	assert.Equal(t, nonStreamCalls[0].ID, streamCalls[0].ID)
	assert.Equal(t, "call_abc123", streamCalls[0].ID)
	assert.Equal(t, nonStreamCalls[0].Name, streamCalls[0].Name)
	assert.Equal(t, nonStreamCalls[0].Args, streamCalls[0].Args)
}

// makeToolCallInitEvent builds an SSE event data string carrying the tool call
// ID and function name for the given index. This is the "header" event that
// precedes argument fragments.
func makeToolCallInitEvent(index int, id, name string) string {
	idJSON, _ := json.Marshal(id)
	nameJSON, _ := json.Marshal(name)
	return fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":%s,"type":"function","function":{"name":%s,"arguments":""}}]}}]}`, index, idJSON, nameJSON)
}

// makeToolCallArgEvent builds an SSE event data string carrying an argument
// fragment for the tool call at the given index.
func makeToolCallArgEvent(index int, arguments string) string {
	argsJSON, _ := json.Marshal(arguments)
	return fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}}]}`, index, argsJSON)
}

// TestAccumulateOpenAIStreamToolCalls_BuilderCorrectness verifies that 100
// argument fragments producing a ~10 KB JSON payload are correctly accumulated
// by the strings.Builder-based accumulator.
func TestAccumulateOpenAIStreamToolCalls_BuilderCorrectness(t *testing.T) {
	largeValue := strings.Repeat("a", 10*1024)
	fullJSON := `{"key":"` + largeValue + `"}`

	events := []string{makeToolCallInitEvent(0, "call_0", "big_tool")}

	n := 100
	chunkSize := len(fullJSON) / n
	for i := 0; i < n; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if i == n-1 {
			end = len(fullJSON)
		}
		events = append(events, makeToolCallArgEvent(0, fullJSON[start:end]))
	}
	events = append(events, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)

	toolCalls, _, _ := runStreamAccumulator(t, events...)
	require.Len(t, toolCalls, 1)
	assert.Equal(t, "call_0", toolCalls[0].ID)
	assert.Equal(t, "big_tool", toolCalls[0].Name)

	args, ok := toolCalls[0].Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, largeValue, args["key"])
}

// TestAccumulateOpenAIStreamToolCalls_MultipleTools verifies that interleaved
// argument fragments for multiple tools are accumulated independently by each
// tool's strings.Builder.
func TestAccumulateOpenAIStreamToolCalls_MultipleTools(t *testing.T) {
	events := []string{
		makeToolCallInitEvent(0, "call_a", "tool_a"),
		makeToolCallInitEvent(1, "call_b", "tool_b"),
		// Interleave fragments so each Builder must stay independent.
		makeToolCallArgEvent(0, `{"msg":"`),
		makeToolCallArgEvent(1, `{"msg":"`),
		makeToolCallArgEvent(0, `hello`),
		makeToolCallArgEvent(1, `world`),
		makeToolCallArgEvent(0, `"}`),
		makeToolCallArgEvent(1, `"}`),
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}

	toolCalls, _, _ := runStreamAccumulator(t, events...)
	require.Len(t, toolCalls, 2)

	assert.Equal(t, "call_a", toolCalls[0].ID)
	assert.Equal(t, "tool_a", toolCalls[0].Name)
	args0, ok := toolCalls[0].Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "hello", args0["msg"])

	assert.Equal(t, "call_b", toolCalls[1].ID)
	assert.Equal(t, "tool_b", toolCalls[1].Name)
	args1, ok := toolCalls[1].Args.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "world", args1["msg"])
}

// TestAccumulateOpenAIStreamToolCalls_Race runs the accumulator concurrently
// from multiple goroutines to verify there is no shared mutable state (the
// race detector will fail if any data race exists).
func TestAccumulateOpenAIStreamToolCalls_Race(t *testing.T) {
	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			events := []string{
				makeToolCallInitEvent(0, "call_0", "tool"),
				makeToolCallArgEvent(0, `{"x":1}`),
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			}
			ev := make(chan SSEEvent, len(events)+1)
			for _, e := range events {
				ev <- SSEEvent{Data: e}
			}
			ev <- SSEEvent{Data: "[DONE]"}
			close(ev)

			ch := make(chan MessageChunk, 64)
			accumulateOpenAIStreamToolCalls(context.Background(), ev, ch)
			close(ch)
			for range ch {
			}
		}()
	}
	wg.Wait()
}

// BenchmarkAccumulateToolCalls_Builder measures the allocation profile of the
// strings.Builder-based accumulator with 100 argument fragments producing a
// ~10 KB JSON payload. With Builder, allocs/op should be O(log n) and B/op
// O(n), in contrast to the O(n²) bytes of the old string-concat approach.
func BenchmarkAccumulateToolCalls_Builder(b *testing.B) {
	largeValue := strings.Repeat("a", 10*1024)
	fullJSON := `{"key":"` + largeValue + `"}`

	initEvent := makeToolCallInitEvent(0, "call_0", "big_tool")
	finishEvent := `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`

	n := 100
	chunkSize := len(fullJSON) / n
	argEvents := make([]string, n)
	for i := 0; i < n; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if i == n-1 {
			end = len(fullJSON)
		}
		argEvents[i] = makeToolCallArgEvent(0, fullJSON[start:end])
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		events := make(chan SSEEvent, n+4)
		events <- SSEEvent{Data: initEvent}
		for _, e := range argEvents {
			events <- SSEEvent{Data: e}
		}
		events <- SSEEvent{Data: finishEvent}
		events <- SSEEvent{Data: "[DONE]"}
		close(events)

		ch := make(chan MessageChunk, 64)
		accumulateOpenAIStreamToolCalls(context.Background(), events, ch)
		close(ch)
		for range ch {
		}
	}
}
