package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

// writeSSEEventWithID writes an SSE event carrying an id: field so the client
// can resume via Last-Event-ID on reconnect.
func writeSSEEventWithID(w http.ResponseWriter, id string, ev core.AgentEvent) {
	jsonBytes, _ := json.Marshal(ev)
	fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, ev.Kind, jsonBytes) //nolint:errcheck
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
		if len(received) == len(expected) {
			cancel()
		}
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
		if len(received) == 1 {
			cancel()
		}
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

	// With reconnect enabled, a persistent 503 retries until maxReconnects is
	// exhausted, then closes the channel with no events.
	adapter := NewACPStreamAdapter(srv.URL,
		WithMaxReconnects(2),
		WithBackoffBase(5*time.Millisecond),
	)
	ch := adapter.Stream(ctx)

	// Channel should close with no events once reconnects are exhausted.
	for ae := range ch {
		t.Fatalf("expected no events on bad status, got: %+v", ae)
	}
}

// TestACPReconnect verifies that when the SSE connection drops, the adapter
// reconnects with exponential backoff, resumes the stream, and sends the
// Last-Event-ID header so the server can replay missed events.
func TestACPReconnect(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var connCount int32
	var mu sync.Mutex
	var gotLastEventID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()

		if n == 1 {
			// First connection: send an event with an id, then drop the
			// connection by returning from the handler.
			writeSSEEventWithID(w, "evt-1", core.AgentEvent{
				Kind: "message", Content: "first", Timestamp: time.Now(),
			})
			return
		}
		// Subsequent connections: record the resume header and deliver the
		// event that was "missed" during the disconnect.
		mu.Lock()
		gotLastEventID = r.Header.Get("Last-Event-ID")
		mu.Unlock()
		writeSSEEvent(w, core.AgentEvent{
			Kind: "message", Content: "second", Timestamp: time.Now(),
		})
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := NewACPStreamAdapter(srv.URL,
		WithBackoffBase(20*time.Millisecond),
		WithMaxReconnects(5),
	)
	ch := adapter.Stream(ctx)

	var received []AgentEvent
	for ae := range ch {
		received = append(received, ae)
		if len(received) == 2 {
			cancel()
		}
	}

	require.Len(t, received, 2)
	assert.Equal(t, "first", received[0].Content)
	assert.Equal(t, "second", received[1].Content)
	mu.Lock()
	assert.Equal(t, "evt-1", gotLastEventID, "reconnect must send Last-Event-ID header")
	mu.Unlock()
}

// TestACPHeartbeatTimeout verifies that a stalled connection (no data at all,
// not even keep-alive comments) is detected as dead after the heartbeat
// timeout and torn down to trigger a reconnect.
func TestACPHeartbeatTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var connCount int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&connCount, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()

		if n == 1 {
			// First connection: accept but never send any data (not even a
			// keep-alive comment). The client must detect the dead link.
			<-r.Context().Done()
			return
		}
		// After the heartbeat-driven reconnect, deliver an event.
		writeSSEEvent(w, core.AgentEvent{
			Kind: "message", Content: "after-heartbeat", Timestamp: time.Now(),
		})
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := NewACPStreamAdapter(srv.URL,
		WithHeartbeatTimeout(100*time.Millisecond),
		WithBackoffBase(20*time.Millisecond),
		WithMaxReconnects(3),
	)
	ch := adapter.Stream(ctx)

	var received []AgentEvent
	for ae := range ch {
		received = append(received, ae)
		if len(received) == 1 {
			cancel()
		}
	}

	require.Len(t, received, 1)
	assert.Equal(t, "after-heartbeat", received[0].Content)
	assert.GreaterOrEqual(t, atomic.LoadInt32(&connCount), int32(2),
		"heartbeat timeout must trigger a reconnect")
}

// TestACPHeartbeatKeepAlive verifies that keep-alive comments (":") reset the
// heartbeat timer, keeping an otherwise quiet connection alive.
func TestACPHeartbeatKeepAlive(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()
		// Send keep-alive comments well within the heartbeat window so the
		// link is NOT declared dead. After a few comments, send a real event.
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, ": keep-alive\n\n") //nolint:errcheck
			flusher.Flush()
			time.Sleep(30 * time.Millisecond)
		}
		writeSSEEvent(w, core.AgentEvent{
			Kind: "message", Content: "survived", Timestamp: time.Now(),
		})
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := NewACPStreamAdapter(srv.URL,
		WithHeartbeatTimeout(80*time.Millisecond),
		WithMaxReconnects(0),
	)
	ch := adapter.Stream(ctx)

	var received []AgentEvent
	for ae := range ch {
		received = append(received, ae)
		if len(received) == 1 {
			cancel()
		}
	}

	require.Len(t, received, 1)
	assert.Equal(t, "survived", received[0].Content)
}

// TestACPMaxReconnectsExhausted verifies that once the configured reconnect
// limit is reached, the adapter gives up and closes the channel.
func TestACPMaxReconnectsExhausted(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	var connCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&connCount, 1)
		// Always drop the connection immediately to force reconnects.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter := NewACPStreamAdapter(srv.URL,
		WithMaxReconnects(3),
		WithBackoffBase(5*time.Millisecond),
	)
	ch := adapter.Stream(ctx)

	// Channel must close once reconnects are exhausted; no events expected.
	for ae := range ch {
		t.Fatalf("expected no events, got: %+v", ae)
	}

	// Initial connection + 3 reconnect attempts = 4 total.
	assert.Equal(t, int32(4), atomic.LoadInt32(&connCount),
		"expected initial connection plus maxReconnects attempts")
}
