package acp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/core"
)

// stubDispatcher is a controllable SubagentDispatcher for testing the ACP
// bridge adapter. It records every dispatched task and returns a configurable
// result.
type stubDispatcher struct {
	mu     sync.Mutex
	tasks  []core.SubagentTask
	result core.SubagentResult
	err    error
}

func (d *stubDispatcher) Dispatch(_ context.Context, task core.SubagentTask) (core.SubagentResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tasks = append(d.tasks, task)
	return d.result, d.err
}

func (d *stubDispatcher) ParallelDispatch(_ context.Context, tasks []core.SubagentTask) ([]core.SubagentResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.tasks = append(d.tasks, tasks...)
	results := make([]core.SubagentResult, len(tasks))
	for i := range tasks {
		results[i] = d.result
	}
	return results, d.err
}

func (d *stubDispatcher) ListRunning() []core.SubagentTask { return nil }

func (d *stubDispatcher) getTasks() []core.SubagentTask {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]core.SubagentTask, len(d.tasks))
	copy(out, d.tasks)
	return out
}

// echoLoop is a minimal AgentLoop that records its invocation and echoes the
// submission content as a single agent event.
type echoLoop struct {
	called bool
}

func (e *echoLoop) Run(_ context.Context, submission core.Submission, _ ...core.EventStream) ([]core.AgentEvent, error) {
	e.called = true
	return []core.AgentEvent{{Kind: "message", Content: submission.Content, Timestamp: time.Now()}}, nil
}

// TestACPMiddlewareAdapterImplementsCoreMiddleware verifies that
// ACPMiddlewareAdapter satisfies the core.Middleware interface at compile time.
func TestACPMiddlewareAdapterImplementsCoreMiddleware(t *testing.T) {
	var _ core.Middleware = (*ACPMiddlewareAdapter)(nil)
}

// TestACPMiddlewareAdapterName verifies the adapter delegates Name to the
// wrapped ACPMiddleware.
func TestACPMiddlewareAdapterName(t *testing.T) {
	mw := NewACPMiddleware("acp-bridge", nil)
	adapter := NewACPMiddlewareAdapter(mw, nil, nil)
	assert.Equal(t, "acp-bridge", adapter.Name())
}

// TestACPMiddlewareAdapterWrapsLoop verifies that Wrap returns a loop that
// delegates Run to the inner loop unchanged.
func TestACPMiddlewareAdapterWrapsLoop(t *testing.T) {
	mw := NewACPMiddleware("acp-bridge", nil)
	adapter := NewACPMiddlewareAdapter(mw, nil, nil)
	inner := &echoLoop{}
	wrapped := adapter.Wrap(inner)

	events, err := wrapped.Run(context.Background(), core.Submission{Content: "hello"})
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "hello", events[0].Content)
	assert.True(t, inner.called, "inner loop must be called")
}

// TestACPMiddlewareAdapterRoutesMessageToDispatcher verifies that an inbound
// ACP message received from the client is converted to a SubagentTask,
// dispatched to the SubagentDispatcher, and the result is relayed back as an
// ACP response.
func TestACPMiddlewareAdapterRoutesMessageToDispatcher(t *testing.T) {
	client := newStubClient()
	dispatcher := &stubDispatcher{
		result: core.SubagentResult{TaskID: "acp-peer", Content: "dispatched-result"},
	}

	mw := NewACPMiddleware("acp-bridge", client)
	adapter := NewACPMiddlewareAdapter(mw, dispatcher, client)
	t.Cleanup(adapter.Close)

	inner := &echoLoop{}
	wrapped := adapter.Wrap(inner)

	// Start the router by invoking Run once.
	_, _ = wrapped.Run(context.Background(), core.Submission{Content: "start"})

	// Simulate an inbound ACP message from the peer.
	client.recv <- ACPMessage{
		Type:       TypeMessage,
		SenderID:   "peer",
		ReceiverID: "me",
		Content:    "do work",
	}

	// The dispatcher must receive a task whose Prompt matches the message.
	require.Eventually(t, func() bool {
		return len(dispatcher.getTasks()) > 0
	}, 2*time.Second, 10*time.Millisecond, "dispatcher should receive a task")

	tasks := dispatcher.getTasks()
	require.Len(t, tasks, 1)
	assert.Equal(t, "do work", tasks[0].Prompt)

	// The result must be relayed back as a TypeResponse through the client.
	require.Eventually(t, func() bool {
		return client.sentCount() > 0
	}, 2*time.Second, 10*time.Millisecond, "response should be sent back")

	reply := client.lastSent()
	assert.Equal(t, TypeResponse, reply.Type)
	assert.Equal(t, "me", reply.SenderID)
	assert.Equal(t, "peer", reply.ReceiverID)
	assert.Equal(t, "dispatched-result", reply.Content)
	assert.False(t, reply.Timestamp.IsZero())
}

// TestACPMiddlewareAdapterRoutesErrorOnDispatchFailure verifies that when
// Dispatch returns an error, an ACP error message is sent back instead of a
// response.
func TestACPMiddlewareAdapterRoutesErrorOnDispatchFailure(t *testing.T) {
	client := newStubClient()
	dispatcher := &stubDispatcher{
		err: assert.AnError,
	}

	mw := NewACPMiddleware("acp-bridge", client)
	adapter := NewACPMiddlewareAdapter(mw, dispatcher, client)
	t.Cleanup(adapter.Close)

	inner := &echoLoop{}
	wrapped := adapter.Wrap(inner)
	_, _ = wrapped.Run(context.Background(), core.Submission{Content: "start"})

	client.recv <- ACPMessage{
		Type:     TypeMessage,
		SenderID: "peer",
		Content:  "fail me",
	}

	require.Eventually(t, func() bool {
		return client.sentCount() > 0
	}, 2*time.Second, 10*time.Millisecond, "error response should be sent")

	reply := client.lastSent()
	assert.Equal(t, TypeError, reply.Type)
	assert.Equal(t, "peer", reply.ReceiverID)
}

// TestACPMiddlewareAdapterCloseStopsRouter verifies that Close stops the
// background message router and does not block.
func TestACPMiddlewareAdapterCloseStopsRouter(t *testing.T) {
	client := newStubClient()
	dispatcher := &stubDispatcher{}
	mw := NewACPMiddleware("acp-bridge", client)
	adapter := NewACPMiddlewareAdapter(mw, dispatcher, client)

	inner := &echoLoop{}
	wrapped := adapter.Wrap(inner)
	_, _ = wrapped.Run(context.Background(), core.Submission{Content: "start"})

	done := make(chan struct{})
	go func() {
		adapter.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked")
	}

	// A second Close must be a safe no-op.
	adapter.Close()
}

// TestACPMiddlewareAdapterNilClientNoPanic verifies the adapter does not panic
// when neither client nor dispatcher is configured (ACP not active).
func TestACPMiddlewareAdapterNilClientNoPanic(t *testing.T) {
	mw := NewACPMiddleware("acp-bridge", nil)
	adapter := NewACPMiddlewareAdapter(mw, nil, nil)
	inner := &echoLoop{}
	wrapped := adapter.Wrap(inner)

	assert.NotPanics(t, func() {
		_, _ = wrapped.Run(context.Background(), core.Submission{Content: "hello"})
	})
	adapter.Close()
}

// TestACPMiddlewareAdapterIgnoresNonMessageTypes verifies that inbound ACP
// messages whose Type is not TypeMessage are ignored (not dispatched).
func TestACPMiddlewareAdapterIgnoresNonMessageTypes(t *testing.T) {
	client := newStubClient()
	dispatcher := &stubDispatcher{}

	mw := NewACPMiddleware("acp-bridge", client)
	adapter := NewACPMiddlewareAdapter(mw, dispatcher, client)
	t.Cleanup(adapter.Close)

	inner := &echoLoop{}
	wrapped := adapter.Wrap(inner)
	_, _ = wrapped.Run(context.Background(), core.Submission{Content: "start"})

	client.recv <- ACPMessage{Type: TypeAck, SenderID: "peer", Content: "ack"}
	client.recv <- ACPMessage{Type: TypeConnect, SenderID: "peer", Content: "connect"}

	// Give the router a brief window to process.
	time.Sleep(100 * time.Millisecond)

	assert.Empty(t, dispatcher.getTasks(), "non-message types must not be dispatched")
}
