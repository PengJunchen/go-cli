package llm

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestReasoningActivityOnlyPublishesAcceptedProgress(t *testing.T) {
	events := make(chan SSEEvent, 16)
	for _, frame := range []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"reasoning":"private-alpha"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"private-beta"}}]}`,
		`{"choices":[{"delta":{"reasoning":[{"text":"private-gamma"}]}}]}`,
		`{"choices":[{"delta":{"reasoning_content":{"text":"private-delta"}}}]}`,
		`{"choices":[{"delta":{"reasoning":null,"reasoning_content":""}}]}`,
		`{"choices":[{"delta":{"reasoning":[],"reasoning_content":{}}}]}`,
		`{"choices":[{"delta":{"reasoning":false,"reasoning_content":0}}]}`,
		`{"choices":[{"delta":{"role":"assistant","unknown":"metadata"}}]}`,
		`{"choices":[{"delta":{"content":"visible","reasoning":"private-epsilon"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`,
		`[DONE]`,
	} {
		events <- SSEEvent{Data: frame}
	}
	close(events)
	out := make(chan MessageChunk, 16)
	tools, finish, usage, err := accumulateOpenAIStreamToolCalls(context.Background(), events, out)
	require.NoError(t, err)
	require.Empty(t, tools)
	require.Equal(t, "stop", finish)
	require.NotNil(t, usage)
	require.Equal(t, 11, usage.InputTokens)
	require.Equal(t, 4, usage.OutputTokens)
	close(out)
	var chunks []MessageChunk
	for chunk := range out {
		chunks = append(chunks, chunk)
	}
	require.Len(t, chunks, 6, "one legacy role, four reasoning activity chunks, one visible delta")
	for _, chunk := range chunks[:5] {
		require.Empty(t, chunk.Content)
		require.Empty(t, chunk.ToolCalls)
		require.False(t, chunk.Final)
		require.Nil(t, chunk.Usage)
	}
	require.Equal(t, "visible", chunks[5].Content)
}

func TestReasoningActivityCoalescesWithToolActivity(t *testing.T) {
	events := make(chan SSEEvent, 4)
	for _, frame := range []string{
		`{"choices":[{"delta":{"role":"assistant"}}]}`,
		`{"choices":[{"delta":{"reasoning":"private","tool_calls":[{"index":0,"id":"c1","function":{"name":"inspect","arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`[DONE]`,
	} {
		events <- SSEEvent{Data: frame}
	}
	close(events)
	out := make(chan MessageChunk, 8)
	tools, finish, _, err := accumulateOpenAIStreamToolCalls(context.Background(), events, out)
	require.NoError(t, err)
	require.Equal(t, "tool_calls", finish)
	require.Len(t, tools, 1)
	close(out)
	var chunks []MessageChunk
	for chunk := range out {
		chunks = append(chunks, chunk)
	}
	require.Len(t, chunks, 2, "reasoning and tool fragments in one frame emit only one activity chunk")
	require.Empty(t, chunks[1].Content)
	require.Empty(t, chunks[1].ToolCalls)
}

func TestReasoningActivityRejectsMalformedAndPostTerminalFrames(t *testing.T) {
	for _, frames := range [][]string{
		{`{"choices":[{"delta":{"role":"assistant"}}]}`, `{invalid`, `{"choices":[{"delta":{"reasoning":"private"}}]}`},
		{`{"choices":[{"delta":{"role":"assistant"}}]}`, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `{"choices":[{"delta":{"reasoning":"private"}}]}`},
		{`{"choices":[{"delta":{"role":"assistant"}}]}`, `{"choices":[{"delta":{"reasoning":"private","tool_calls":[{"index":0,"id":"c1","function":{"name":"inspect","arguments":"{"}}]},"finish_reason":"tool_calls"}]}`},
	} {
		events := make(chan SSEEvent, len(frames))
		for _, frame := range frames {
			events <- SSEEvent{Data: frame}
		}
		close(events)
		out := make(chan MessageChunk, 8)
		_, _, _, err := accumulateOpenAIStreamToolCalls(context.Background(), events, out)
		require.Error(t, err)
		close(out)
		var chunks []MessageChunk
		for chunk := range out {
			chunks = append(chunks, chunk)
		}
		require.Len(t, chunks, 1, "only the legacy initial role is permitted")
		require.Empty(t, chunks[0].Content)
	}
}

func TestReasoningHeartbeatKeepsRealSSEStreamAlive(t *testing.T) {
	for _, provider := range []string{"compatible", "native"} {
		t.Run(provider, func(t *testing.T) {
			const idle = 350 * time.Millisecond
			const gap = 80 * time.Millisecond
			const privateReasoning = "fixture-private-reasoning"
			var requests atomic.Int32
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSE(w, `{"choices":[{"delta":{"role":"assistant"}}]}`)
				for i := 0; i < 7; i++ {
					select {
					case <-time.After(gap):
					case <-r.Context().Done():
						return
					}
					if i%2 == 0 {
						writeSSE(w, `{"choices":[{"delta":{"reasoning":"`+privateReasoning+`"}}]}`)
					} else {
						writeSSE(w, `{"choices":[{"delta":{"reasoning_content":"`+privateReasoning+`"}}]}`)
					}
				}
				writeSSE(w, `{"choices":[{"delta":{"content":"visible"}}]}`)
				writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`)
				writeSSE(w, `[DONE]`)
			}))
			defer server.Close()
			model := detailTestModel(t, provider, server)
			model = NewTimeoutModelMiddleware(WithTotalTimeout(4*time.Second), WithStreamChunkTimeout(idle)).WrapModel(model)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			ctx = WithOperationDispatchAuthority(ctx, &attemptTestAuthority{id: "reasoning-heartbeat"})
			start := time.Now()
			stream, err := model.Stream(ctx, []Message{{Role: RoleUser, Content: "fixture"}})
			require.NoError(t, err)
			var visible string
			var final *MessageChunk
			var streamErr error
			for chunk := range stream {
				require.NotContains(t, chunk.Content, privateReasoning)
				visible += chunk.Content
				if chunk.Error != nil {
					streamErr = errors.Join(streamErr, chunk.Error)
				}
				if chunk.Final {
					copy := chunk
					final = &copy
				}
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("fake SSE handler did not close")
			}
			require.NoError(t, streamErr)
			require.Greater(t, time.Since(start), idle)
			require.Equal(t, "visible", visible)
			require.NotNil(t, final)
			require.Equal(t, "stop", final.FinishReason)
			require.NotNil(t, final.Usage)
			require.Equal(t, 11, final.Usage.InputTokens)
			require.Equal(t, 4, final.Usage.OutputTokens)
			require.EqualValues(t, 1, requests.Load())
		})
	}
}

func TestReasoningStreamDeadlineAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		total, idle, parent          time.Duration
		metadataOnly, consumerCancel bool
		want                         string
	}{
		{name: "idle", total: 2 * time.Second, idle: 250 * time.Millisecond, metadataOnly: true, want: "stream idle exceeded"},
		{name: "total", total: 500 * time.Millisecond, idle: 250 * time.Millisecond, want: "stream exceeded"},
		{name: "parent", total: 2 * time.Second, idle: time.Second, parent: 250 * time.Millisecond, want: "stream canceled"},
		{name: "consumer", total: 2 * time.Second, idle: time.Second, consumerCancel: true, want: "stream canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(done)
				requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSE(w, `{"choices":[{"delta":{"role":"assistant"}}]}`)
				ticker := time.NewTicker(50 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						if tc.metadataOnly {
							writeSSE(w, `{"choices":[{"delta":{"role":"assistant","reasoning":null,"reasoning_content":"","extra":"metadata"}}]}`)
						} else {
							writeSSE(w, `{"choices":[{"delta":{"reasoning_content":"private-fixture"}}]}`)
						}
					}
				}
			}))
			defer server.Close()
			model := detailTestModel(t, "compatible", server)
			model = NewTimeoutModelMiddleware(WithTotalTimeout(tc.total), WithStreamChunkTimeout(tc.idle)).WrapModel(model)
			base, stop := context.WithTimeout(context.Background(), 4*time.Second)
			defer stop()
			if tc.parent > 0 {
				var parentStop context.CancelFunc
				base, parentStop = context.WithTimeout(base, tc.parent)
				defer parentStop()
			}
			consumerErr := errors.New("consumer-stopped-fixture")
			if tc.consumerCancel {
				var cancel context.CancelCauseFunc
				base, cancel = context.WithCancelCause(base)
				defer cancel(nil)
				timer := time.AfterFunc(200*time.Millisecond, func() { cancel(consumerErr) })
				defer timer.Stop()
			}
			ctx := WithOperationDispatchAuthority(base, &attemptTestAuthority{id: "reasoning-timeout"})
			stream, err := model.Stream(ctx, []Message{{Role: RoleUser, Content: "fixture"}})
			require.NoError(t, err)
			var streamErr error
			for chunk := range stream {
				require.Empty(t, chunk.Content)
				require.False(t, chunk.Final)
				streamErr = errors.Join(streamErr, chunk.Error)
			}
			require.Error(t, streamErr)
			var abort *LocalStreamAbortCause
			if errors.As(streamErr, &abort) {
				streamErr = abort.Unwrap()
			}
			require.Contains(t, streamErr.Error(), tc.want)
			if tc.consumerCancel {
				require.ErrorIs(t, streamErr, consumerErr)
			} else {
				require.ErrorIs(t, streamErr, context.DeadlineExceeded)
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("fake SSE request did not close after cancellation")
			}
			require.EqualValues(t, 1, requests.Load())
		})
	}
}
