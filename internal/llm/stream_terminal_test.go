package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func buildHTTPStreamTestModel(t *testing.T, server *httptest.Server) BaseChatModel {
	t.Helper()
	provider := NewEinoProvider(WithBaseURL(server.URL), WithHTTPClient(server.Client()))
	model, _, err := provider.Build(context.Background(), ModelConfig{Model: "stream-test"})
	require.NoError(t, err)
	return model
}

type idleStreamModel struct{}

func (idleStreamModel) Generate(context.Context, []Message, ...Option) (*Message, error) {
	return nil, errors.New("unused Generate")
}

func (idleStreamModel) Stream(ctx context.Context, _ []Message, _ ...Option) (<-chan MessageChunk, error) {
	ch := make(chan MessageChunk, 1)
	ch <- MessageChunk{Role: RoleAssistant, Content: "partial"}
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func writeSSE(w http.ResponseWriter, data string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func collectStreamChunks(t *testing.T, ch <-chan MessageChunk) []MessageChunk {
	t.Helper()
	var chunks []MessageChunk
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return chunks
			}
			chunks = append(chunks, chunk)
		case <-timer.C:
			t.Fatal("stream did not close within the test deadline")
			return nil
		}
	}
}

func assertStreamFailedWithoutSuccess(t *testing.T, chunks []MessageChunk) {
	t.Helper()
	var streamErr error
	for _, chunk := range chunks {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
		assert.False(t, chunk.Final && chunk.Error == nil, "an incomplete provider stream must not emit a successful final chunk")
	}
	require.Error(t, streamErr, "stream failure must be observable through MessageChunk.Error")
}

func TestStreamReadErrorIsNotComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		payload := `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n"
		_, _ = rw.WriteString(fmt.Sprintf("%x\r\n%s\r\n", len(payload), payload))
		_, _ = rw.WriteString("20\r\ntruncated")
		_ = rw.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)

	model := buildHTTPStreamTestModel(t, server)
	ch, err := model.Stream(context.Background(), []Message{{Role: RoleUser, Content: "go"}})
	require.NoError(t, err)
	assertStreamFailedWithoutSuccess(t, collectStreamChunks(t, ch))
}

func TestStreamMalformedEventIsNotComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"choices":[{"delta":{"content":"partial"}}]}`)
		writeSSE(w, `{ malformed json`)
		writeSSE(w, "[DONE]")
	}))
	t.Cleanup(server.Close)

	model := buildHTTPStreamTestModel(t, server)
	ch, err := model.Stream(context.Background(), []Message{{Role: RoleUser, Content: "go"}})
	require.NoError(t, err)
	assertStreamFailedWithoutSuccess(t, collectStreamChunks(t, ch))
}

func TestStreamTimeoutIsNotComplete(t *testing.T) {
	timed := NewTimeoutModelMiddleware(
		WithTotalTimeout(time.Second),
		WithStreamChunkTimeout(30*time.Millisecond),
	).WrapModel(idleStreamModel{})
	ch, err := timed.Stream(context.Background(), []Message{{Role: RoleUser, Content: "go"}})
	require.NoError(t, err)
	chunks := collectStreamChunks(t, ch)
	assertStreamFailedWithoutSuccess(t, chunks)

	var content strings.Builder
	for _, chunk := range chunks {
		content.WriteString(chunk.Content)
	}
	assert.Equal(t, "partial", content.String(), "unguarded streams preserve already emitted partial text before a terminal timeout")
}

func TestOpenAIStreamRejectsChoicesAfterTerminalFinishReason(t *testing.T) {
	for name, lateDelta := range map[string]string{
		"text":      `"content":"injected"`,
		"tool call": `"tool_calls":[{"index":0,"id":"late-call","type":"function","function":{"name":"inspect","arguments":"{}"}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`)
				writeSSE(w, `{"choices":[{"delta":{`+lateDelta+`},"finish_reason":null}]}`)
				writeSSE(w, "[DONE]")
			}))
			t.Cleanup(server.Close)

			model := buildHTTPStreamTestModel(t, server)
			ch, err := model.Stream(context.Background(), []Message{{Role: RoleUser, Content: "go"}})
			require.NoError(t, err)
			chunks := collectStreamChunks(t, ch)
			assertStreamFailedWithoutSuccess(t, chunks)
			for _, chunk := range chunks {
				assert.NotContains(t, chunk.Content, "injected")
				assert.Empty(t, chunk.ToolCalls)
			}
		})
	}
}

func TestOpenAIStreamRequiresTerminalFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"choices":[{"delta":{"content":"partial"}}]}`)
		writeSSE(w, "[DONE]")
	}))
	t.Cleanup(server.Close)

	model := buildHTTPStreamTestModel(t, server)
	ch, err := model.Stream(context.Background(), []Message{{Role: RoleUser, Content: "go"}})
	require.NoError(t, err)
	assertStreamFailedWithoutSuccess(t, collectStreamChunks(t, ch))
}

func TestHTTPProviderCancellationClosesStreamingRequest(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, `{"choices":[{"delta":{"content":"`+strings.Repeat("x", 100)+`"}}]}`)
		close(requestStarted)
		<-r.Context().Done()
		close(requestCanceled)
	}))
	t.Cleanup(server.Close)

	model := buildHTTPStreamTestModel(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := model.Stream(ctx, []Message{{Role: RoleUser, Content: "go"}})
	require.NoError(t, err)
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("provider request did not start")
	}
	cancel()
	chunks := collectStreamChunks(t, ch)
	assertStreamFailedWithoutSuccess(t, chunks)
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request context did not observe stream cancellation")
	}
}
