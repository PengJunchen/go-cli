package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

type toolFragmentObservedBody struct {
	io.ReadCloser
	read   func([]byte)
	closed chan struct{}
	once   sync.Once
}

func (body *toolFragmentObservedBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if n > 0 {
		body.read(p[:n])
	}
	return n, err
}

func (body *toolFragmentObservedBody) Close() error {
	err := body.ReadCloser.Close()
	body.once.Do(func() { close(body.closed) })
	return err
}

func TestHTTPToolFragmentsKeepStreamIdleAliveUntilComplete(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()
	const idle = time.Second
	const gap = 100 * time.Millisecond
	const callID = "call-continuously-streamed"
	const toolName = "inspect"
	args := map[string]any{"value": strings.Repeat("x", 32)}
	rawArgs, err := json.Marshal(args)
	require.NoError(t, err)
	var parts []string
	for start := 0; start < len(rawArgs); start += 2 {
		parts = append(parts, string(rawArgs[start:min(start+2, len(rawArgs))]))
	}
	require.Greater(t, time.Duration(len(parts))*gap, idle+idle/2, "the valid tool stream must outlive the idle budget")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	bodyClosed := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer close(handlerDone)
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(payload string) bool {
			if _, writeErr := fmt.Fprintf(w, "data: %s\n\n", payload); writeErr != nil {
				return false
			}
			w.(http.Flusher).Flush()
			return true
		}
		if !write(`{"id":"stream-tool-fragment-fixture","choices":[{"delta":{"role":"assistant"}}]}`) {
			return
		}
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		for index, part := range parts {
			timer := time.NewTimer(gap)
			select {
			case <-timer.C:
			case <-request.Context().Done():
				timer.Stop()
				return
			}
			call := map[string]any{"index": 0, "function": map[string]any{"arguments": part}}
			if index == 0 {
				call["id"] = callID
				call["type"] = "function"
				call["function"].(map[string]any)["name"] = toolName
			}
			payload, marshalErr := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{call}}}}})
			if marshalErr != nil || !write(string(payload)) {
				return
			}
		}
		if write(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`) {
			write("[DONE]")
		}
	}))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer func() { cancel(); server.Close(); transport.CloseIdleConnections() }()
	var mu sync.Mutex
	var pending []byte
	var receivedParts []string
	var receivedAt []time.Time
	var observeErr error
	observe := func(raw []byte) {
		mu.Lock()
		defer mu.Unlock()
		pending = append(pending, raw...)
		for {
			end := bytes.Index(pending, []byte("\n\n"))
			if end < 0 {
				break
			}
			frame := bytes.TrimSpace(bytes.TrimPrefix(pending[:end], []byte("data:")))
			pending = pending[end+2:]
			if bytes.Equal(frame, []byte("[DONE]")) {
				continue
			}
			var chunk openAIStreamChunk
			if decodeErr := json.Unmarshal(frame, &chunk); decodeErr != nil {
				observeErr = decodeErr
				continue
			}
			for _, choice := range chunk.Choices {
				for _, call := range choice.Delta.ToolCalls {
					if call.Function.Arguments != "" {
						receivedParts = append(receivedParts, call.Function.Arguments)
						receivedAt = append(receivedAt, time.Now())
					}
				}
			}
		}
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response, roundTripErr := transport.RoundTrip(request)
		if roundTripErr == nil {
			response.Body = &toolFragmentObservedBody{ReadCloser: response.Body, read: observe, closed: bodyClosed}
		}
		return response, roundTripErr
	})}
	provider := NewEinoProvider(WithBaseURL(server.URL), WithHTTPClient(client))
	model, cleanup, err := provider.Build(ctx, ModelConfig{Model: "tool-fragment-idle-fixture"})
	require.NoError(t, err)
	defer cleanup()
	stream, err := NewTimeoutModelMiddleware(WithTotalTimeout(10*time.Second), WithStreamChunkTimeout(idle)).WrapModel(model).Stream(ctx, []Message{{Role: RoleUser, Content: "inspect"}})
	require.NoError(t, err)
	select {
	case first := <-stream:
		require.Equal(t, RoleAssistant, first.Role)
		require.Empty(t, first.Content)
		require.Empty(t, first.ToolCalls)
		require.False(t, first.Final)
		require.NoError(t, first.Error)
	case <-ctx.Done():
		t.Fatal("fixture never observed the provider's role-ready chunk")
	}
	ready := time.Now()
	close(release)
	var streamErr error
	var final *MessageChunk
	var abortedAt time.Time
	for chunk := range stream {
		require.Empty(t, chunk.Content, "tool-argument activity must not fabricate assistant text")
		if chunk.Error != nil {
			streamErr = errors.Join(streamErr, chunk.Error)
			abortedAt = time.Now()
		}
		if chunk.Final {
			require.Nil(t, final, "only one final tool invocation may be assembled")
			copy := chunk
			final = &copy
		} else {
			require.Empty(t, chunk.ToolCalls, "partial arguments must never be dispatched as tools")
		}
	}
	select {
	case <-handlerDone:
	case <-ctx.Done():
		t.Fatal("SSE producer did not join")
	}
	select {
	case <-bodyClosed:
	case <-ctx.Done():
		t.Fatal("provider response reader did not close")
	}
	mu.Lock()
	consumed := append([]string(nil), receivedParts...)
	times := append([]time.Time(nil), receivedAt...)
	parseErr := observeErr
	mu.Unlock()
	require.NoError(t, parseErr)
	require.EqualValues(t, 1, requests.Load())
	require.GreaterOrEqual(t, len(consumed), 6, "actual provider reads must establish continuous legal tool-argument activity")
	require.Equal(t, parts[:len(consumed)], consumed)
	maxGap := times[0].Sub(ready)
	for index := 1; index < len(times); index++ {
		maxGap = max(maxGap, times[index].Sub(times[index-1]))
	}
	require.Less(t, maxGap, idle/2, "a real fixture-side quiet gap is not evidence of dropped activity")
	if streamErr != nil {
		require.ErrorIs(t, streamErr, context.DeadlineExceeded)
		require.Contains(t, streamErr.Error(), "stream idle exceeded")
		require.Less(t, abortedAt.Sub(times[len(times)-1]), idle/2, "the provider must have consumed a valid fragment shortly before the idle abort")
		require.Nil(t, final)
	}
	t.Logf("actual provider tool fragments consumed=%d planned=%d max_gap=%s elapsed=%s last_read_to_abort=%s error=%v", len(consumed), len(parts), maxGap, time.Since(ready), abortedAt.Sub(times[len(times)-1]), streamErr)
	require.NoError(t, streamErr, "continuous valid tool-argument SSE activity must keep the existing idle watchdog alive")
	require.NotNil(t, final)
	require.Equal(t, "tool_calls", final.FinishReason)
	require.Equal(t, []ToolCall{{ID: callID, Name: toolName, Args: args}}, final.ToolCalls)
}

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
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}",
			"",
			"data: [DONE]",
			"",
		}, "\n"))
	}))
	defer srv.Close()

	model := &HTTPChatModel{buildMode: UnboundLibraryBuild,
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

	model := &HTTPChatModel{buildMode: UnboundLibraryBuild,
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
	einoModel := &HTTPChatModel{buildMode: UnboundLibraryBuild,
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
	require.Len(t, einoChunks, 4, "initial role, two accepted argument pulses, and one complete final")
	for _, chunks := range [][]MessageChunk{einoChunks, nativeChunks} {
		for _, chunk := range chunks[:len(chunks)-1] {
			require.Equal(t, RoleAssistant, chunk.Role)
			require.Empty(t, chunk.Content)
			require.Empty(t, chunk.ToolCalls)
			require.False(t, chunk.Final)
			require.Empty(t, chunk.FinishReason)
			require.Nil(t, chunk.Usage)
		}
	}

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
	toolCalls, finishReason, chunks, err := runStreamAccumulatorResult(t, eventData...)
	require.NoError(t, err)
	return toolCalls, finishReason, chunks
}

func runStreamAccumulatorResult(t *testing.T, eventData ...string) ([]ToolCall, string, []MessageChunk, error) {
	t.Helper()
	events := make(chan SSEEvent, len(eventData)+1)
	for _, data := range eventData {
		events <- SSEEvent{Data: data}
	}
	events <- SSEEvent{Data: "[DONE]"}
	close(events)

	ch := make(chan MessageChunk, len(eventData)+1)
	toolCalls, finishReason, _, err := accumulateOpenAIStreamToolCalls(context.Background(), events, ch)
	close(ch)

	var chunks []MessageChunk
	for c := range ch {
		chunks = append(chunks, c)
	}
	return toolCalls, finishReason, chunks, err
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

// Missing wire IDs must fail closed instead of inventing a call ID that cannot
// be correlated with provider-side tool history.
func TestStreamAccumulator_RejectsMissingIDs(t *testing.T) {
	_, _, _, err := runStreamAccumulatorResult(t,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"SF\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	require.ErrorContains(t, err, "missing an ID or name")
}

func TestStreamAccumulator_RequiresTerminalFinishReason(t *testing.T) {
	_, _, _, err := runStreamAccumulatorResult(t,
		`{"choices":[{"delta":{"content":"partial"}}]}`,
	)
	require.ErrorContains(t, err, "without a terminal finish reason")
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

func TestStreamAccumulator_RejectsConflictingIDs(t *testing.T) {
	_, _, _, err := runStreamAccumulatorResult(t,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_second","function":{"arguments":"{\"city\":\"SF\"}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	require.ErrorContains(t, err, "conflicting IDs")
}

func TestStreamAccumulator_RejectsConflictingNames(t *testing.T) {
	_, _, _, err := runStreamAccumulatorResult(t,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"delete_file","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
	require.ErrorContains(t, err, "conflicting names")
}

func TestStreamAccumulator_RejectsInvalidToolCallIndexesBeforeAllocation(t *testing.T) {
	for _, index := range []string{"-1", "100000"} {
		t.Run(index, func(t *testing.T) {
			_, _, _, err := runStreamAccumulatorResult(t,
				fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%s,"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`, index),
			)
			require.ErrorContains(t, err, "outside the supported range")
		})
	}
}

func TestStreamAccumulator_RejectsNonObjectToolArguments(t *testing.T) {
	for _, rawArgs := range []string{"null", "[]", "true", "42", `"value"`} {
		t.Run(rawArgs, func(t *testing.T) {
			encodedArgs, err := json.Marshal(rawArgs)
			require.NoError(t, err)
			_, _, _, err = runStreamAccumulatorResult(t,
				fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":%s}}]}}]}`, encodedArgs),
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			)
			require.Error(t, err, "wire tool arguments must be a JSON object")
		})
	}
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

func TestStreamAccumulatorToolActivityOnlyForAcceptedNewPayload(t *testing.T) {
	role := `{"choices":[{"delta":{"role":"assistant"}}]}`
	complete := `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"inspect","arguments":"{}"}}]}}]}`
	finish := `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`
	for _, test := range []struct {
		name     string
		events   []string
		contents []string
		err      string
	}{
		{name: "initial role already signals activity", events: []string{complete, finish}, contents: []string{""}},
		{name: "new accepted bytes signal activity", events: []string{role, complete, finish}, contents: []string{"", ""}},
		{name: "text already signals activity", events: []string{role, strings.Replace(complete, `"delta":{`, `"delta":{"content":"text",`, 1), finish}, contents: []string{"", "text"}},
		{name: "empty and repeated metadata add no activity", events: []string{role, complete,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"inspect","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":""}}]}}]}`, finish}, contents: []string{"", ""}},
		{name: "conflicting metadata rejects new bytes without activity", events: []string{role, complete,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"conflict","function":{"arguments":"more"}}]}}]}`, finish}, contents: []string{"", ""}, err: "conflicting IDs"},
		{name: "invalid index adds no activity", events: []string{role,
			`{"choices":[{"delta":{"tool_calls":[{"index":-1,"id":"call","function":{"name":"inspect","arguments":"{}"}}]}}]}`, finish}, contents: []string{""}, err: "outside the supported range"},
		{name: "oversize payload adds no activity", events: []string{role, makeToolCallInitEvent(0, "call", "inspect"), makeToolCallArgEvent(0, strings.Repeat("x", GuardedStreamMaxBytes+1)), finish}, contents: []string{"", ""}, err: "payload exceeds"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls, _, chunks, streamErr := runStreamAccumulatorResult(t, test.events...)
			if test.err != "" {
				require.ErrorContains(t, streamErr, test.err)
				require.Empty(t, calls)
			} else {
				require.NoError(t, streamErr)
				require.Len(t, calls, 1)
				require.Equal(t, ToolCall{ID: "call-1", Name: "inspect", Args: map[string]any{}}, calls[0])
			}
			var contents []string
			for _, chunk := range chunks {
				contents = append(contents, chunk.Content)
				require.Equal(t, RoleAssistant, chunk.Role)
				require.Empty(t, chunk.ToolCalls)
				require.False(t, chunk.Final)
				require.Empty(t, chunk.FinishReason)
				require.Nil(t, chunk.Usage)
			}
			require.Equal(t, test.contents, contents)
		})
	}
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
			_, _, _, _ = accumulateOpenAIStreamToolCalls(context.Background(), ev, ch)
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

		ch := make(chan MessageChunk, n+4)
		_, _, _, _ = accumulateOpenAIStreamToolCalls(context.Background(), events, ch)
		close(ch)
		for range ch {
		}
	}
}
