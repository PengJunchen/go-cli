package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

const assemblyCandidateLimitBytes = 400_000

type assemblyStreamFixture struct {
	server   *httptest.Server
	requests atomic.Int32
	modesMu  sync.Mutex
	modes    []bool
}

func newAssemblyStreamFixture(t *testing.T, handle func(http.ResponseWriter, *http.Request, int)) *assemblyStreamFixture {
	t.Helper()
	fixture := &assemblyStreamFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(fixture.requests.Add(1))
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &request)
		fixture.modesMu.Lock()
		fixture.modes = append(fixture.modes, request.Stream)
		fixture.modesMu.Unlock()
		handle(w, r, n)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *assemblyStreamFixture) requestModes() []bool {
	fixture.modesMu.Lock()
	defer fixture.modesMu.Unlock()
	return append([]bool(nil), fixture.modes...)
}

func (fixture *assemblyStreamFixture) assembleAndSubmit(t *testing.T, prompt string) (context.Context, core.EventStream) {
	t.Helper()
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	config := newE2ETestConfig(t, fixture.server.URL)
	// This uses the same real CLI assembly as interactive execution: provider,
	// the seven-layer model middleware chain (including overflow continuation),
	// telemetry/circuit-breaker wrappers, the outer output guard, and Harness.
	assembly, err := AssembleAgent(ctx, config, config.Provider.Name, config.Provider.Model, io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	stream, err := assembly.Harness.Submit(ctx, prompt)
	require.NoError(t, err)
	return ctx, stream
}

func assemblyRequestStreams(r *http.Request) bool {
	var request struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	return request.Stream
}

func writeAssemblyStreamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
}

func writeAssemblyChunk(w http.ResponseWriter, delta map[string]any, finishReason any) {
	payload, err := json.Marshal(map[string]any{
		"id":     "assembly-stream-test",
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	})
	if err != nil {
		panic(err)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeAssemblyTextResponse(w http.ResponseWriter, pieces []string, finishReason string) {
	writeAssemblyStreamHeaders(w)
	for _, piece := range pieces {
		writeAssemblyChunk(w, map[string]any{"content": piece}, nil)
	}
	writeAssemblyChunk(w, map[string]any{}, finishReason)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeAssemblyGenerateTextResponse(w http.ResponseWriter, content, finishReason string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": finishReason,
		}},
	})
}

func collectAssemblyEvents(stream core.EventStream) ([]core.AgentEvent, core.AgentMessage, error) {
	var events []core.AgentEvent
	for event := range stream.Events() {
		events = append(events, event)
	}
	message, err := stream.Result()
	return events, message, err
}

func drainAvailableAssemblyEvents(stream core.EventStream) []core.AgentEvent {
	var events []core.AgentEvent
	for {
		select {
		case event, ok := <-stream.Events():
			if !ok {
				return events
			}
			events = append(events, event)
		default:
			return events
		}
	}
}

func assemblyIncrementalText(events []core.AgentEvent) string {
	var text strings.Builder
	for _, event := range events {
		if event.Kind == "message" && event.Incremental {
			text.WriteString(event.Content)
		}
	}
	return text.String()
}

func TestOutputGuardRunsBeforeVisibleDelta(t *testing.T) {
	const secret = "john@example.com"
	fixture := newAssemblyStreamFixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeAssemblyTextResponse(w, []string{"Contact john@", "example.com for help."}, "stop")
	})
	_, stream := fixture.assembleAndSubmit(t, "Say hello to me.")
	events, message, err := collectAssemblyEvents(stream)
	assert.Error(t, err, "the PII guard must reject a candidate containing a full email")
	assert.Zero(t, len(assemblyIncrementalText(events)), "a guarded candidate must not emit incremental content")
	assert.NotContains(t, assemblyIncrementalText(events), secret)
	assert.NotContains(t, message.Content, secret)
	for _, event := range events {
		if event.Kind == "message" {
			assert.NotContains(t, event.Content, secret)
		}
	}
}

func TestOutputGuardBuffersAcrossLengthContinuationsBeforeVisibleDelta(t *testing.T) {
	const secret = "john@example.com"
	continuationStarted := make(chan struct{})
	continuationRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(continuationRelease) }) }
	t.Cleanup(release)
	fixture := newAssemblyStreamFixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			writeAssemblyTextResponse(w, []string{"Contact john@exa"}, "length")
			return
		}
		if n == 2 {
			close(continuationStarted)
			<-continuationRelease
			if assemblyRequestStreams(r) {
				writeAssemblyTextResponse(w, []string{"mple.com for help."}, "stop")
			} else {
				writeAssemblyGenerateTextResponse(w, "mple.com for help.", "stop")
			}
			return
		}
		http.Error(w, "unexpected extra model request", http.StatusBadRequest)
	})
	ctx, stream := fixture.assembleAndSubmit(t, "Say hello to me.")

	select {
	case <-continuationStarted:
	case <-ctx.Done():
		release()
		t.Fatalf("continuation request did not start: %v", ctx.Err())
	}
	beforeContinuation := drainAvailableAssemblyEvents(stream)
	release()
	afterContinuation, message, err := collectAssemblyEvents(stream)
	events := append(beforeContinuation, afterContinuation...)
	assert.Error(t, err, "the complete PII candidate must be rejected after the continuation is assembled")
	assert.Empty(t, assemblyIncrementalText(beforeContinuation), "no delta may be visible while the length continuation is incomplete")
	assert.NotContains(t, assemblyIncrementalText(events), secret)
	assert.NotContains(t, message.Content, secret)
	assert.Equal(t, 2, int(fixture.requests.Load()), "one initial request and one continuation only; modes=%v", fixture.requestModes())
}

func TestOutputGuardBudgetCoversMiddlewareContinuationBuffers(t *testing.T) {
	first := strings.Repeat("a", assemblyCandidateLimitBytes*5/8)
	second := strings.Repeat("b", assemblyCandidateLimitBytes-len(first)+1)
	continuationStarted := make(chan struct{})
	continuationRelease := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(continuationRelease) }) }
	t.Cleanup(release)
	fixture := newAssemblyStreamFixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			writeAssemblyTextResponse(w, []string{first}, "length")
			return
		}
		if n == 2 {
			close(continuationStarted)
			<-continuationRelease
			if assemblyRequestStreams(r) {
				writeAssemblyTextResponse(w, []string{second}, "stop")
			} else {
				writeAssemblyGenerateTextResponse(w, second, "stop")
			}
			return
		}
		http.Error(w, "unexpected extra model request", http.StatusBadRequest)
	})
	ctx, stream := fixture.assembleAndSubmit(t, "Return a response just over the output budget.")
	select {
	case <-continuationStarted:
	case <-ctx.Done():
		release()
		t.Fatalf("continuation request did not start: %v", ctx.Err())
	}
	beforeContinuation := drainAvailableAssemblyEvents(stream)
	release()
	afterContinuation, message, err := collectAssemblyEvents(stream)
	events := append(beforeContinuation, afterContinuation...)
	assert.Error(t, err, "combined continuation content past the budget must fail closed")
	assert.Zero(t, len(assemblyIncrementalText(events)), "neither response segment may become visible before the combined budget check")
	assert.Zero(t, len(message.Content))
	assert.Equal(t, 2, int(fixture.requests.Load()), "one initial request and one continuation only; modes=%v", fixture.requestModes())
}

func TestContinuationErrorReturnsFailure(t *testing.T) {
	fixture := newAssemblyStreamFixture(t, func(w http.ResponseWriter, r *http.Request, n int) {
		streaming := assemblyRequestStreams(r)
		if n == 1 {
			writeAssemblyTextResponse(w, []string{"partial safe"}, "length")
			return
		}
		if !streaming {
			http.Error(w, `{"error":"continuation rejected"}`, http.StatusBadRequest)
			return
		}
		if n == 2 {
			http.Error(w, `{"error":"continuation rejected"}`, http.StatusBadRequest)
			return
		}
		writeAssemblyTextResponse(w, []string{"recovered"}, "stop")
	})
	_, stream := fixture.assembleAndSubmit(t, "Say hello to me.")
	events, message, err := collectAssemblyEvents(stream)
	assert.Error(t, err, "a failed continuation request must fail the turn")
	assert.Empty(t, assemblyIncrementalText(events), "an incomplete candidate must not be visible")
	assert.NotContains(t, message.Content, "partial safe", "a truncated partial response must not become a successful result")
	assert.Equal(t, 2, int(fixture.requests.Load()), "the failed continuation must terminate the turn without retry/fallback; modes=%v", fixture.requestModes())
}

func TestIncompleteToolArgumentsAreNotDispatched(t *testing.T) {
	target := filepath.Join(t.TempDir(), "incomplete-arguments-must-not-write.txt")
	fixture := newAssemblyStreamFixture(t, func(w http.ResponseWriter, _ *http.Request, n int) {
		if n == 1 {
			writeAssemblyStreamHeaders(w)
			writeAssemblyChunk(w, map[string]any{"tool_calls": []any{map[string]any{
				"index": 0,
				"id":    "call-incomplete",
				"type":  "function",
				"function": map[string]any{
					"name":      "write",
					"arguments": `{"path":"` + target + `","content":"must not be written"`,
				},
			}}}, nil)
			writeAssemblyChunk(w, map[string]any{}, "tool_calls")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		writeAssemblyTextResponse(w, []string{"unexpected tool dispatch"}, "stop")
	})
	_, stream := fixture.assembleAndSubmit(t, "Write the marker file.")
	events, _, _ := collectAssemblyEvents(stream)
	for _, event := range events {
		assert.NotEqual(t, "tool_call", event.Kind, "malformed streamed arguments must not be dispatched")
	}
	assert.Equal(t, int32(1), fixture.requests.Load(), "an incomplete call must fail before a follow-up model request; modes=%v", fixture.requestModes())
	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err), "the incomplete write must not create the target")
}

func TestOutputGuardBufferLimitFailsClosedWithoutVisibleDelta(t *testing.T) {
	oversized := strings.Repeat("x", assemblyCandidateLimitBytes+1)
	fixture := newAssemblyStreamFixture(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		writeAssemblyTextResponse(w, []string{oversized}, "stop")
	})
	_, stream := fixture.assembleAndSubmit(t, "Return a long response.")
	events, message, err := collectAssemblyEvents(stream)
	assert.Error(t, err, "a candidate past the bounded output budget must fail closed")
	assert.Zero(t, len(assemblyIncrementalText(events)), "oversized content must not emit visible deltas")
	assert.Zero(t, len(message.Content))
	assert.Equal(t, 1, int(fixture.requests.Load()), "the single over-budget candidate must stop before any continuation; modes=%v", fixture.requestModes())
}
