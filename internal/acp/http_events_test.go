package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

type sseFlushGate struct {
	header http.Header
	mu     sync.Mutex
	body   bytes.Buffer
	status int

	flushCount  int
	initialSent chan struct{}
	liveBlocked chan struct{}
	releaseLive chan struct{}
	releaseOnce sync.Once
}

func newSSEFlushGate() *sseFlushGate {
	return &sseFlushGate{
		header:      make(http.Header),
		initialSent: make(chan struct{}),
		liveBlocked: make(chan struct{}),
		releaseLive: make(chan struct{}),
	}
}

func (w *sseFlushGate) Header() http.Header { return w.header }

func (w *sseFlushGate) WriteHeader(status int) { w.status = status }

func (w *sseFlushGate) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func (w *sseFlushGate) Flush() {
	w.mu.Lock()
	w.flushCount++
	count := w.flushCount
	w.mu.Unlock()
	switch count {
	case 1:
		close(w.initialSent)
	case 2:
		close(w.liveBlocked)
		<-w.releaseLive
	}
}

func (w *sseFlushGate) bodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func (w *sseFlushGate) release() { w.releaseOnce.Do(func() { close(w.releaseLive) }) }

type sseTestFrame struct {
	id    uint64
	event string
	data  string
}

func parseSSETestFrames(t *testing.T, raw string) []sseTestFrame {
	t.Helper()
	scanner := bufio.NewScanner(strings.NewReader(raw))
	var frames []sseTestFrame
	var frame sseTestFrame
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			frame.id, _ = strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
		case strings.HasPrefix(line, "event: "):
			frame.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			frame.data = strings.TrimPrefix(line, "data: ")
		case line == "" && frame.event != "":
			frames = append(frames, frame)
			frame = sseTestFrame{}
		}
	}
	require.NoError(t, scanner.Err())
	return frames
}

func TestSSEStream_OverflowReportsGapAfterContiguousPrefix(t *testing.T) {
	session := NewSession("slow-reader")
	server := NewHTTPServer("test", "", nil)
	server.sessions[session.ID()] = httpSessionBinding{principalID: localLoopbackPrincipal, senderAlias: session.ID(), session: session}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/events?session_id=slow-reader", nil).WithContext(ctx)
	writer := newSSEFlushGate()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		server.handleEvents(writer, request)
	}()
	defer func() {
		writer.release()
		cancel()
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Error("SSE handler did not stop during test cleanup")
		}
	}()
	select {
	case <-writer.initialSent:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not establish its stream")
	}

	session.PublishEvent(core.AgentEvent{Kind: "message", Content: "first"})
	select {
	case <-writer.liveBlocked:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not block at the controlled slow-consumer boundary")
	}

	const lastID = uint64(1 + eventSubscriberBufferSize + 5)
	publishDone := make(chan struct{})
	go func() {
		defer close(publishDone)
		for id := uint64(2); id <= lastID; id++ {
			session.PublishEvent(core.AgentEvent{Kind: "message", Content: fmt.Sprint(id)})
		}
	}()
	select {
	case <-publishDone:
	case <-time.After(time.Second):
		t.Fatal("slow SSE consumer blocked event publication")
	}
	writer.release()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not finish after reporting overflow")
	}

	frames := parseSSETestFrames(t, writer.bodyString())
	require.Len(t, frames, eventSubscriberBufferSize+2)
	for index := 0; index <= eventSubscriberBufferSize; index++ {
		require.Equal(t, "message", frames[index].event)
		require.Equal(t, uint64(index+1), frames[index].id)
	}
	gapFrame := frames[len(frames)-1]
	require.Equal(t, "event_gap", gapFrame.event)
	require.Equal(t, uint64(eventSubscriberBufferSize+1), gapFrame.id)
	var gap EventGap
	require.NoError(t, json.Unmarshal([]byte(gapFrame.data), &gap))
	require.Equal(t, "subscriber_overflow", gap.Reason)
	require.Equal(t, uint64(eventSubscriberBufferSize+2), gap.MissingFrom)
	require.Equal(t, lastID, gap.MissingThrough)
	require.Equal(t, uint64(5), gap.MissingCount)
	require.Equal(t, lastID, gap.LastEventID)
}

func TestSSEStream_OverflowThenSessionCloseStillReportsGap(t *testing.T) {
	session := NewSession("overflow-close")
	server := NewHTTPServer("test", "", nil)
	server.sessions[session.ID()] = httpSessionBinding{principalID: localLoopbackPrincipal, senderAlias: session.ID(), session: session}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/events?session_id=overflow-close", nil).WithContext(ctx)
	writer := newSSEFlushGate()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		server.handleEvents(writer, request)
	}()
	defer func() {
		writer.release()
		cancel()
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Error("SSE handler did not stop during test cleanup")
		}
	}()
	select {
	case <-writer.initialSent:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not establish its stream")
	}

	session.PublishEvent(core.AgentEvent{Kind: "message", Content: "first"})
	select {
	case <-writer.liveBlocked:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not block at the controlled slow-consumer boundary")
	}

	const lastID = uint64(1 + eventSubscriberBufferSize + 5)
	publishDone := make(chan struct{})
	go func() {
		defer close(publishDone)
		for id := uint64(2); id <= lastID; id++ {
			session.PublishEvent(core.AgentEvent{Kind: "message", Content: fmt.Sprint(id)})
		}
	}()
	select {
	case <-publishDone:
	case <-time.After(time.Second):
		t.Fatal("slow SSE consumer blocked event publication")
	}
	session.Close()
	writer.release()
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not finish after session close")
	}

	frames := parseSSETestFrames(t, writer.bodyString())
	require.Len(t, frames, eventSubscriberBufferSize+2)
	for index := 0; index <= eventSubscriberBufferSize; index++ {
		require.Equal(t, "message", frames[index].event)
		require.Equal(t, uint64(index+1), frames[index].id)
	}
	gapFrame := frames[len(frames)-1]
	require.Equal(t, "event_gap", gapFrame.event)
	require.Equal(t, uint64(eventSubscriberBufferSize+1), gapFrame.id)
	var gap EventGap
	require.NoError(t, json.Unmarshal([]byte(gapFrame.data), &gap))
	require.Equal(t, "subscriber_overflow", gap.Reason)
	require.Equal(t, uint64(eventSubscriberBufferSize+2), gap.MissingFrom)
	require.Equal(t, lastID, gap.MissingThrough)
	require.Equal(t, uint64(5), gap.MissingCount)
	require.Equal(t, lastID, gap.LastEventID)
}

func TestSSEStream_FutureLastEventIDReturnsBadRequest(t *testing.T) {
	session := NewSession("future-cursor")
	session.PublishEvent(core.AgentEvent{Kind: "message", Content: "existing"})
	server := NewHTTPServer("test", "", nil)
	server.sessions[session.ID()] = httpSessionBinding{principalID: localLoopbackPrincipal, senderAlias: session.ID(), session: session}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/events?session_id=future-cursor", nil).WithContext(ctx)
	request.Header.Set("Last-Event-ID", "2")
	writer := newSSEFlushGate()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		server.handleEvents(writer, request)
	}()
	select {
	case <-handlerDone:
	case <-writer.initialSent:
		cancel()
		select {
		case <-handlerDone:
		case <-time.After(time.Second):
			t.Fatal("SSE handler did not stop after the test canceled the response")
		}
	case <-time.After(time.Second):
		t.Fatal("SSE handler neither rejected the cursor nor started the stream")
	}

	require.Equal(t, http.StatusBadRequest, writer.status)
	session.mu.Lock()
	subscriberCount := len(session.subscribers)
	session.mu.Unlock()
	require.Zero(t, subscriberCount, "a rejected future cursor must not register a live subscriber")
}
