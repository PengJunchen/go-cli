package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestEventStreamBoundedBuffering(t *testing.T) {
	stream := NewEventStream(3)
	// A bounded buffer accepts events up to capacity without a consumer.
	for i := 0; i < 3; i++ {
		require.NoError(t, stream.Send(AgentEvent{Kind: "message", Content: "m"}))
	}
	stream.Close()

	got := drainEvents(stream)
	assert.Len(t, got, 3)
}

func TestEventStreamZeroCapacityWithConsumer(t *testing.T) {
	// A zero-capacity (synchronous) stream requires a consumer for Send to
	// complete. Drive a reader concurrently and verify the event lands and the
	// channel afterwards closes.
	stream := NewEventStream(0)

	got := make(chan AgentEvent, 1)
	go func() {
		ev, ok := <-stream.Events()
		if ok {
			got <- ev
		}
	}()

	require.NoError(t, stream.Send(AgentEvent{Kind: "status", Content: "sync"}))
	stream.Close()

	select {
	case ev := <-got:
		assert.Equal(t, "status", ev.Kind)
		assert.Equal(t, "sync", ev.Content)
	case <-time.After(2 * time.Second):
		t.Fatal("synchronous send did not reach the consumer")
	}
	// After close the channel is closed.
	_, open := <-stream.Events()
	assert.False(t, open)
}

func TestEventStreamResultAndError(t *testing.T) {
	stream := NewEventStream(1)
	stream.SetResult(AgentMessage{Role: "assistant", Content: "final"}, nil)
	stream.Close()

	res, err := stream.Result()
	require.NoError(t, err)
	assert.Equal(t, "assistant", res.Role)
	assert.Equal(t, "final", res.Content)
	assert.NoError(t, stream.Err())
}

func TestEventStreamResultWithError(t *testing.T) {
	stream := NewEventStream(1)
	boom := errors.New("run failed")
	stream.SetResult(AgentMessage{Content: "partial"}, boom)

	res, err := stream.Result()
	require.ErrorIs(t, err, boom)
	assert.Equal(t, "partial", res.Content)
	assert.ErrorIs(t, stream.Err(), boom)
}

func TestEventStreamSetResultAfterCloseIgnored(t *testing.T) {
	stream := NewEventStream(1)
	stream.SetResult(AgentMessage{Content: "first"}, nil)
	stream.Close()

	// SetResult after close must not overwrite the recorded result.
	stream.SetResult(AgentMessage{Content: "second"}, errors.New("late"))
	res, err := stream.Result()
	require.NoError(t, err)
	assert.Equal(t, "first", res.Content)
	assert.NoError(t, stream.Err())
}

func TestEventStreamResultBeforeSetIsNoResult(t *testing.T) {
	stream := NewEventStream(1)
	_, err := stream.Result()
	require.ErrorIs(t, err, errNoResult)
	assert.NoError(t, stream.Err())
}

func TestEventStreamErrDefaultNil(t *testing.T) {
	stream := NewEventStream(1)
	stream.Close()
	assert.NoError(t, stream.Err())
}

func TestEventStreamConcurrentSendAndSendIsSafe(t *testing.T) {
	stream := NewEventStream(64)

	var wg sync.WaitGroup
	const n = 40
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			//nolint:errcheck,gosec // Send returns nil; best-effort under test.
			stream.Send(AgentEvent{Kind: "message", Content: "x"})
		}()
	}
	wg.Wait()

	stream.Close()
	// Every send must have been buffered and drained.
	got := drainEvents(stream)
	assert.Len(t, got, n)
}

func TestEventStreamEventsChannelCloses(t *testing.T) {
	stream := NewEventStream(2)
	ch := stream.Events()
	stream.Close()
	_, open := <-ch
	assert.False(t, open, "events channel must be closed after Close")
}

func TestEventStreamReplayResultIsStable(t *testing.T) {
	stream := NewEventStream(1)
	stream.SetResult(AgentMessage{Role: "assistant", Content: "stable"}, nil)
	stream.Close()

	first, err := stream.Result()
	require.NoError(t, err)
	second, err := stream.Result()
	require.NoError(t, err)
	assert.Equal(t, first, second)
}

// fakeEventStreamAgent is an Agent that produces a configurable result and set
// of events and records how many times Run was invoked.
type fakeEventStreamAgent struct {
	mu     sync.Mutex
	calls  int
	events []AgentEvent
	res    Result
	err    error
}

func (f *fakeEventStreamAgent) Name() string { return "fake-agent" }

func (f *fakeEventStreamAgent) Run(_ context.Context, _ Submission, _ ...EventStream) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.res, f.err
}

func (f *fakeEventStreamAgent) Events() []AgentEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AgentEvent{}, f.events...)
}

func (f *fakeEventStreamAgent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var _ eventSource = (*fakeEventStreamAgent)(nil)

func TestHarnessFansOutAgentEventsToStream(t *testing.T) {
	agent := &fakeEventStreamAgent{
		events: []AgentEvent{
			{Kind: "message", Content: "hello"},
			{Kind: "status", Content: "thinking"},
		},
		res: Result{Message: "hello", Success: true},
	}
	h := NewHarnessImpl(agent, WithEventBuffer(16))
	stream, err := h.Submit(context.Background(), "hi")
	require.NoError(t, err)

	events := drainEvents(stream)
	// fanned-out events plus a terminal "done" event.
	assert.Contains(t, findEvents(events, "message"), "hello")
	done := findEvents(events, "done")
	require.Len(t, done, 1)
	assert.Equal(t, "hello", done[0])
	assert.Equal(t, 1, agent.callCount())
}

// TestEventStreamConcurrentSendAndCloseNoPanicNoRecover exercises 32 senders
// and 2 closers racing against each other with a drain goroutine consuming
// events. The stream must not panic and no goroutine may leak.
func TestEventStreamConcurrentSendAndCloseNoPanicNoRecover(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	stream := NewEventStream(8)

	// Drain goroutine consuming Events() until the channel closes.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range stream.Events() {
		}
	}()

	const numSenders = 32
	var sendWg sync.WaitGroup
	sendWg.Add(numSenders)
	for range numSenders {
		go func() {
			defer sendWg.Done()
			//nolint:errcheck,gosec // Send returns nil; best-effort under test.
			stream.Send(AgentEvent{Kind: "message", Content: "x"})
		}()
	}

	// Two closers racing — sync.Once must make this safe.
	var closeWg sync.WaitGroup
	closeWg.Add(2)
	for range 2 {
		go func() {
			defer closeWg.Done()
			stream.Close()
		}()
	}

	sendWg.Wait()
	closeWg.Wait()
	<-drainDone
}

// TestEventStreamResultNeverBlockedBySlowConsumer verifies that Result and
// SetResult work immediately even when a Send is blocked on a zero-capacity
// channel with no consumer. The mutex is not held during the channel send,
// so result access is never blocked by back-pressure.
func TestEventStreamResultNeverBlockedBySlowConsumer(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	stream := NewEventStream(0)

	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		//nolint:errcheck,gosec // Send returns nil; best-effort under test.
		stream.Send(AgentEvent{Kind: "message", Content: "blocked"})
	}()

	// Allow the goroutine to enter the blocking select.
	time.Sleep(10 * time.Millisecond)

	// SetResult and Result must succeed while Send is blocked.
	stream.SetResult(AgentMessage{Role: "assistant", Content: "final"}, nil)
	res, err := stream.Result()
	require.NoError(t, err)
	assert.Equal(t, "final", res.Content)

	// Unblock the send.
	stream.Close()
	<-sendDone
}

// TestEventStreamSendBlockedThenCloseNoDeadlock verifies that Close does not
// deadlock when a Send is blocked on a zero-capacity channel with no
// consumer. The done channel in the select allows Send to exit promptly.
func TestEventStreamSendBlockedThenCloseNoDeadlock(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	stream := NewEventStream(0)

	sendDone := make(chan struct{})
	go func() {
		defer close(sendDone)
		//nolint:errcheck,gosec // Send returns nil; best-effort under test.
		stream.Send(AgentEvent{Kind: "message", Content: "blocked"})
	}()

	// Allow the goroutine to enter the blocking select.
	time.Sleep(10 * time.Millisecond)

	// Close must complete promptly — the done channel unblocks the select.
	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		stream.Close()
	}()

	select {
	case <-closeDone:
		// Close completed without deadlock.
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked while Send was blocked")
	}

	<-sendDone
}
