package tui

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

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// writeSSEEvent marshals ev as JSON and writes it in SSE format to w.
func writeSSEEvent(w http.ResponseWriter, ev core.AgentEvent) {
	jsonBytes, _ := json.Marshal(ev)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, jsonBytes) //nolint:errcheck
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestHTTPClient(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	expected := []core.AgentEvent{
		{Kind: "message", Content: "hello", Timestamp: time.Now()},
		{Kind: "done", Content: "complete", Timestamp: time.Now()},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		for _, ev := range expected {
			writeSSEEvent(w, ev)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := NewACPStreamAdapter(srv.URL)
	ch := adapter.Stream(ctx)

	var received []AgentEvent
	for ae := range ch {
		received = append(received, ae)
	}

	require.Len(t, received, len(expected))
	assert.Equal(t, "message", received[0].Type)
	assert.Equal(t, "hello", received[0].Content)
	assert.Equal(t, ContentTypeAssistant, received[0].ContentType)
	assert.Equal(t, "done", received[1].Type)
	assert.Equal(t, "complete", received[1].Content)
	assert.Equal(t, ContentTypeStatus, received[1].ContentType)
}

func TestHTTPClient_IgnoresComments(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		// Keep-alive comment should be ignored by the parser.
		fmt.Fprintf(w, ": keep-alive\n\n")
		flusher.Flush()
		writeSSEEvent(w, core.AgentEvent{Kind: "message", Content: "after-comment", Timestamp: time.Now()})
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := NewACPStreamAdapter(srv.URL)
	ch := adapter.Stream(ctx)

	var received []AgentEvent
	for ae := range ch {
		received = append(received, ae)
	}

	require.Len(t, received, 1)
	assert.Equal(t, "after-comment", received[0].Content)
}

func TestHTTPClient_ContextCancel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Server keeps the connection open without sending events.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	adapter := NewACPStreamAdapter(srv.URL)
	ch := adapter.Stream(ctx)

	// Cancel after a short delay; the channel should close without deadlock.
	time.AfterFunc(100*time.Millisecond, cancel)
	for range ch {
	}
}

func TestHTTPClient_BadStatus(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := NewACPStreamAdapter(srv.URL)
	ch := adapter.Stream(ctx)

	// Channel should close immediately with no events.
	for ae := range ch {
		t.Fatalf("expected no events on bad status, got: %+v", ae)
	}
}
