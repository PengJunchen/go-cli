package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestSSEWriter_WriteEvent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse, ok := NewSSEWriter(w)
		if !ok {
			t.Fatal("SSE not supported")
		}
		_ = sse.WriteEvent("message", map[string]string{"content": "hello"})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))

	sc := bufio.NewScanner(resp.Body)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	require.GreaterOrEqual(t, len(lines), 2)
	assert.Equal(t, "event: message", lines[0])
	assert.True(t, strings.HasPrefix(lines[1], "data: "))
}

func TestSSEWriter_WriteComment(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse, ok := NewSSEWriter(w)
		if !ok {
			t.Fatal("SSE not supported")
		}
		sse.WriteComment("keep-alive")
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	sc := bufio.NewScanner(resp.Body)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	require.GreaterOrEqual(t, len(lines), 1)
	assert.Equal(t, ": keep-alive", lines[0])
}

func TestSSEStream(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv, baseURL := startTestHTTPServer(t, nil)
	defer srv.Stop(context.Background())

	// Create a session by connecting.
	postACP(t, baseURL, "/connect", ACPMessage{
		Type:     TypeConnect,
		SenderID: "sse-client",
	}).Body.Close()

	sess := srv.getSession("sse-client")
	require.NotNil(t, sess)

	// Connect to /events; it blocks until events arrive or the session closes.
	eventsURL := baseURL + "/events?sender_id=sse-client"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, eventsURL, nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// Publish events to the session.
	expected := []core.AgentEvent{
		{Kind: "message", Content: "first", Timestamp: time.Now()},
		{Kind: "done", Content: "complete", Timestamp: time.Now()},
	}
	for _, ev := range expected {
		sess.PublishEvent(ev)
	}

	// Read SSE events from the response.
	sc := bufio.NewScanner(resp.Body)
	var received []core.AgentEvent
	var dataLine string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			dataLine = strings.TrimPrefix(line, "data: ")
		} else if line == "" && dataLine != "" {
			var ev core.AgentEvent
			if err := json.Unmarshal([]byte(dataLine), &ev); err == nil {
				received = append(received, ev)
			}
			dataLine = ""
			if len(received) >= len(expected) {
				break
			}
		}
	}

	require.Len(t, received, len(expected))
	assert.Equal(t, "message", received[0].Kind)
	assert.Equal(t, "first", received[0].Content)
	assert.Equal(t, "done", received[1].Kind)
	assert.Equal(t, "complete", received[1].Content)
}

func TestSSEStream_MethodNotAllowed(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv, baseURL := startTestHTTPServer(t, nil)
	defer srv.Stop(context.Background())

	resp, err := http.Post(baseURL+"/events?sender_id=x", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestSSEStream_MissingSenderID(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv, baseURL := startTestHTTPServer(t, nil)
	defer srv.Stop(context.Background())

	resp, err := http.Get(baseURL + "/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestSSEStream_SessionNotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	srv, baseURL := startTestHTTPServer(t, nil)
	defer srv.Stop(context.Background())

	resp, err := http.Get(baseURL + "/events?sender_id=nonexistent")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestSession_PublishEventAfterClose(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	sess := NewSession("test")
	sess.Close()

	// Publishing after close should not panic.
	assert.NotPanics(t, func() {
		sess.PublishEvent(core.AgentEvent{Kind: "message"})
	})
}
