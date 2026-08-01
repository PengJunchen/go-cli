package core

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// drainEvents consumes all events from a stream until it closes.
func drainEvents(stream EventStream) []AgentEvent {
	var evs []AgentEvent
	for ev := range stream.Events() {
		evs = append(evs, ev)
	}
	return evs
}

func testHarness(t *testing.T) *HarnessImpl {
	t.Helper()
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"H-01", "harness",
		mock.ConversationTurn{AssistantContent: "assistant reply"},
	))
	loop := NewLoopAgent(WithLLM(model))
	agent := NewAgentImpl("h", loop)
	return NewHarnessImpl(agent)
}

func TestHarnessSubmitStreamsToDone(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	h := testHarness(t)
	stream, err := h.Submit(context.Background(), "hello harness")
	require.NoError(t, err)
	require.NotNil(t, stream)

	events := drainEvents(stream)
	require.NotEmpty(t, events)

	// Streamed the model message and a terminal "done" event.
	assert.Contains(t, findEvents(events, "message"), "assistant reply")
	done := findEvents(events, "done")
	require.Len(t, done, 1)
	assert.Equal(t, "assistant reply", done[0])

	// The recorded result carries the assistant final message.
	res, rerr := stream.Result()
	require.NoError(t, rerr)
	assert.Equal(t, "assistant", res.Role)
	assert.Equal(t, "assistant reply", res.Content)
	assert.NoError(t, stream.Err())
}

func TestHarnessSubmitBuffered(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	h := NewHarnessImpl(testAgent(t), WithEventBuffer(16))
	stream, err := h.Submit(context.Background(), "hi")
	require.NoError(t, err)
	require.NotNil(t, stream)

	events := drainEvents(stream)
	require.NotEmpty(t, events)
	require.Len(t, findEvents(events, "done"), 1)
}

func TestHarnessSubmitErrorPath(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"H-02", "error",
		mock.ConversationTurn{AssistantError: "boom"},
	))
	loop := NewLoopAgent(WithLLM(model))
	agent := NewAgentImpl("h", loop)
	h := NewHarnessImpl(agent)

	stream, err := h.Submit(context.Background(), "hi")
	require.NoError(t, err)
	require.NotNil(t, stream)

	events := drainEvents(stream)
	errs := findEvents(events, "error")
	require.NotEmpty(t, errs)

	_, rerr := stream.Result()
	require.Error(t, rerr)
}

func TestHarnessWaitsForCompletion(t *testing.T) {
	h := testHarness(t)
	stream, err := h.Submit(context.Background(), "hi")
	require.NoError(t, err)

	// The run must terminate within a bounded window (no goroutine hang).
	done := make(chan struct{})
	go func() {
		drainEvents(stream)
		close(done)
	}()

	select {
	case <-done:
		// success: the stream closed, so the harness goroutine exited.
	case <-time.After(3 * time.Second):
		t.Fatal("harness stream did not close within 3s")
	}
}

func TestHarnessNilAgentPanics(t *testing.T) {
	assert.Panics(t, func() {
		NewHarnessImpl(nil)
	})
}
