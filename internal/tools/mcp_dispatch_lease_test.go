package tools

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mcpDispatchLeaseProbe struct {
	acquireStarted chan struct{}
	allowAcquire   <-chan struct{}
	acquireErr     error
	startedOnce    sync.Once
	acquireCount   atomic.Int32
	releaseCount   atomic.Int32
}

func (l *mcpDispatchLeaseProbe) Acquire() (func(), error) {
	l.acquireCount.Add(1)
	if l.acquireStarted != nil {
		l.startedOnce.Do(func() { close(l.acquireStarted) })
	}
	if l.allowAcquire != nil {
		<-l.allowAcquire
	}
	return func() { l.releaseCount.Add(1) }, l.acquireErr
}

type mcpDispatchLeaseExecutor struct {
	calls   atomic.Int32
	entered chan struct{}
	resume  <-chan struct{}
	once    sync.Once
}

func (e *mcpDispatchLeaseExecutor) ExecuteMCPTool(context.Context, string, ToolCall) (*ToolResult, error) {
	e.calls.Add(1)
	if e.entered != nil {
		e.once.Do(func() { close(e.entered) })
	}
	if e.resume != nil {
		<-e.resume
	}
	return &ToolResult{Output: "done"}, nil
}

func newMCPDispatchLeaseCandidate(t *testing.T, name string, executor MCPGenerationClient, lease PreparedDispatchLease) MCPToolCandidate {
	t.Helper()
	handle, err := NewMCPGenerationClientHandle(executor)
	require.NoError(t, err)
	candidate, err := NewMCPToolCandidate(name, name, "lease test", map[string]any{
		"type":                 "object",
		"properties":           map[string]any{},
		"additionalProperties": false,
	}, handle, lease)
	require.NoError(t, err)
	return candidate
}

func TestPreparedMCPDispatchLeaseAcquiresOutsideReplacementLocks(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	deferred := NewDeferredToolRegistryAdapter(base)
	name := "mcp__srv__tool"
	oldExecutor := &mcpDispatchLeaseExecutor{}
	allowAcquire := make(chan struct{})
	var allowAcquireOnce sync.Once
	releaseAcquire := func() { allowAcquireOnce.Do(func() { close(allowAcquire) }) }
	defer releaseAcquire()
	lease := &mcpDispatchLeaseProbe{acquireStarted: make(chan struct{}), allowAcquire: allowAcquire}
	first, err := deferred.BeginMCPGenerationUpdate(context.Background(), "project-mcp", 1, []MCPToolCandidate{
		newMCPDispatchLeaseCandidate(t, name, oldExecutor, lease),
	})
	require.NoError(t, err)
	require.NoError(t, first.Publish())

	registry := NewMiddlewareToolRegistry(deferred)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(context.Background(), ToolCall{ID: "call-old", Name: name, Args: map[string]any{}})
	require.NoError(t, err)
	dispatchDone := make(chan error, 1)
	go func() {
		_, dispatchErr := invocation.Dispatch(context.Background())
		dispatchDone <- dispatchErr
	}()

	select {
	case <-lease.acquireStarted:
	case dispatchErr := <-dispatchDone:
		t.Fatalf("prepared MCP dispatch completed without acquiring its generation lease: %v", dispatchErr)
	case <-time.After(time.Second):
		t.Fatal("prepared MCP dispatch did not acquire its generation lease")
	}

	newExecutor := &mcpDispatchLeaseExecutor{}
	replacement, err := deferred.BeginMCPGenerationUpdate(context.Background(), "project-mcp", 2, []MCPToolCandidate{
		newMCPDispatchLeaseCandidate(t, name, newExecutor, &mcpDispatchLeaseProbe{}),
	})
	require.NoError(t, err)
	replaced := make(chan error, 1)
	go func() { replaced <- replacement.Publish() }()
	select {
	case err := <-replaced:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("generation replacement blocked behind speculative lease acquisition")
	}

	releaseAcquire()
	select {
	case err := <-dispatchDone:
		require.ErrorIs(t, err, ErrDispatchFenceChanged)
	case <-time.After(time.Second):
		t.Fatal("stale prepared dispatch did not finish after lease acquisition resumed")
	}
	assert.EqualValues(t, 1, lease.acquireCount.Load())
	assert.EqualValues(t, 1, lease.releaseCount.Load(), "a stale snapshot releases its speculative lease exactly once")
	assert.Zero(t, oldExecutor.calls.Load(), "a stale snapshot must not execute its old client generation")
}

func TestPreparedMCPDispatchLeaseJoinsCommittedExecutor(t *testing.T) {
	base := NewDefaultToolRegistry().(*DefaultToolRegistry)
	deferred := NewDeferredToolRegistryAdapter(base)
	name := "mcp__srv__tool"
	resume := make(chan struct{})
	var resumeOnce sync.Once
	finishExecutor := func() { resumeOnce.Do(func() { close(resume) }) }
	defer finishExecutor()
	oldExecutor := &mcpDispatchLeaseExecutor{entered: make(chan struct{}), resume: resume}
	lease := &mcpDispatchLeaseProbe{}
	first, err := deferred.BeginMCPGenerationUpdate(context.Background(), "project-mcp", 1, []MCPToolCandidate{
		newMCPDispatchLeaseCandidate(t, name, oldExecutor, lease),
	})
	require.NoError(t, err)
	require.NoError(t, first.Publish())

	registry := NewMiddlewareToolRegistry(deferred)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(context.Background(), ToolCall{ID: "call-old", Name: name, Args: map[string]any{}})
	require.NoError(t, err)
	dispatchDone := make(chan error, 1)
	go func() {
		_, dispatchErr := invocation.Dispatch(context.Background())
		dispatchDone <- dispatchErr
	}()
	select {
	case <-oldExecutor.entered:
	case <-time.After(time.Second):
		t.Fatal("committed MCP invocation did not enter its captured executor")
	}
	assert.EqualValues(t, 1, lease.acquireCount.Load())
	assert.Zero(t, lease.releaseCount.Load(), "the lease remains held while the committed executor is running")

	replacement, err := deferred.BeginMCPGenerationUpdate(context.Background(), "project-mcp", 2, []MCPToolCandidate{
		newMCPDispatchLeaseCandidate(t, name, &mcpDispatchLeaseExecutor{}, &mcpDispatchLeaseProbe{}),
	})
	require.NoError(t, err)
	require.NoError(t, replacement.Publish())
	assert.Zero(t, lease.releaseCount.Load(), "replacement publication must not release an already committed call's lease")

	finishExecutor()
	select {
	case err := <-dispatchDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("committed MCP invocation did not join its executor")
	}
	assert.EqualValues(t, 1, oldExecutor.calls.Load())
	assert.EqualValues(t, 1, lease.releaseCount.Load(), "the committed invocation releases its generation lease once after executor return")
}

func TestPreparedDispatchLeaseReleasesOnErrorsPanicAndCancellation(t *testing.T) {
	t.Run("executor error", func(t *testing.T) {
		lease := &mcpDispatchLeaseProbe{}
		invocation := newLeaseProbeInvocation(t, lease, func(context.Context, ToolCall) (*ToolResult, error) {
			return nil, errors.New("executor failed")
		})
		_, err := invocation.Dispatch(context.Background())
		require.EqualError(t, err, "executor failed")
		assert.EqualValues(t, 1, lease.acquireCount.Load())
		assert.EqualValues(t, 1, lease.releaseCount.Load())
		assert.True(t, invocation.DispatchCommitted())
	})

	t.Run("executor panic", func(t *testing.T) {
		lease := &mcpDispatchLeaseProbe{}
		invocation := newLeaseProbeInvocation(t, lease, func(context.Context, ToolCall) (*ToolResult, error) {
			panic("executor panic")
		})
		assert.Panics(t, func() { _, _ = invocation.Dispatch(context.Background()) })
		assert.EqualValues(t, 1, lease.acquireCount.Load())
		assert.EqualValues(t, 1, lease.releaseCount.Load(), "panic unwinding still releases the committed generation lease")
		assert.True(t, invocation.DispatchCommitted())
	})

	t.Run("acquire error", func(t *testing.T) {
		lease := &mcpDispatchLeaseProbe{acquireErr: errors.New("retired")}
		var calls atomic.Int32
		invocation := newLeaseProbeInvocation(t, lease, func(context.Context, ToolCall) (*ToolResult, error) {
			calls.Add(1)
			return &ToolResult{}, nil
		})
		_, err := invocation.Dispatch(context.Background())
		assert.ErrorIs(t, err, ErrPreparedDispatchLeaseUnavailable)
		assert.EqualValues(t, 1, lease.acquireCount.Load())
		assert.EqualValues(t, 1, lease.releaseCount.Load(), "an unsuccessful acquisition that returns a release function is unwound")
		assert.Zero(t, calls.Load())
		assert.False(t, invocation.DispatchCommitted())
	})

	t.Run("canceled after commit", func(t *testing.T) {
		lease := &mcpDispatchLeaseProbe{}
		var calls atomic.Int32
		invocation := newLeaseProbeInvocation(t, lease, func(context.Context, ToolCall) (*ToolResult, error) {
			calls.Add(1)
			return &ToolResult{}, nil
		})
		ctx := &cancelAfterPreparedCommitContext{Context: context.Background()}
		_, err := invocation.Dispatch(ctx)
		assert.ErrorIs(t, err, context.Canceled)
		assert.True(t, invocation.DispatchCommitted())
		assert.EqualValues(t, 1, lease.acquireCount.Load())
		assert.EqualValues(t, 1, lease.releaseCount.Load())
		assert.Zero(t, calls.Load(), "cancellation after the commit marker skips execution but releases the lease")
	})
}

func TestPreparedDispatchLeaseIsAcquiredByQueuedWorker(t *testing.T) {
	lease := &mcpDispatchLeaseProbe{}
	fence := NewDispatchFence()
	queued := make(chan *preparedInvocation, 1)
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence:         fence,
		generations:   fence.Snapshot(),
		call:          mustFreezeToolCall(t, ToolCall{Name: "queued-mcp"}),
		dispatchLease: lease,
		execute: func(context.Context, ToolCall) (*ToolResult, error) {
			return &ToolResult{Output: "done"}, nil
		},
		schedule: func(_ context.Context, prepared *preparedInvocation) (*ToolResult, error) {
			queued <- prepared
			return &ToolResult{Output: "queued"}, nil
		},
	})
	_, err := invocation.Dispatch(context.Background())
	require.NoError(t, err)
	assert.Zero(t, lease.acquireCount.Load(), "queue admission must not pin a generation before a worker starts it")

	worker := <-queued
	result, err := worker.commitAndInvoke(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "done", result.Output)
	assert.EqualValues(t, 1, lease.acquireCount.Load())
	assert.EqualValues(t, 1, lease.releaseCount.Load())
}

func TestPreparedStreamingDispatchLeaseCoversSynchronousJoin(t *testing.T) {
	lease := &mcpDispatchLeaseProbe{}
	entered := make(chan struct{})
	resume := make(chan struct{})
	var resumeOnce sync.Once
	finishStream := func() { resumeOnce.Do(func() { close(resume) }) }
	defer finishStream()
	attested, err := AttestSynchronousStreamSink(&preparedCaptureSink{})
	require.NoError(t, err)
	fence := NewDispatchFence()
	invocation := newPreparedStreamingInvocation(preparedInvocationConfig{
		fence:           fence,
		generations:     fence.Snapshot(),
		call:            mustFreezeToolCall(t, ToolCall{ID: "call-stream", Name: "stream-mcp"}),
		dispatchLease:   lease,
		completionProof: CompletionInlinePipelineJoined,
		streamingSink:   attested.(*synchronousStreamSink),
	}, func(_ context.Context, call ToolCall, sink StreamSink) (*ToolResult, error) {
		close(entered)
		<-resume
		return &ToolResult{Output: call.ID}, sink.Send("done", call.ID, "stdout")
	})
	dispatchDone := make(chan error, 1)
	go func() {
		_, dispatchErr := invocation.DispatchStreaming(context.Background(), attested)
		dispatchDone <- dispatchErr
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("streaming MCP executor did not start")
	}
	assert.Zero(t, lease.releaseCount.Load(), "stream lease remains held while its captured synchronous sink is active")
	finishStream()
	select {
	case err := <-dispatchDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("streaming dispatch did not join")
	}
	assert.EqualValues(t, 1, lease.acquireCount.Load())
	assert.EqualValues(t, 1, lease.releaseCount.Load())
	assert.Equal(t, CompletionInlinePipelineJoined, invocation.CompletionSignal().Proof())
}

type cancelAfterPreparedCommitContext struct {
	context.Context
	errCalls atomic.Int32
	canceled atomic.Bool
}

func (c *cancelAfterPreparedCommitContext) Err() error {
	if c.errCalls.Add(1) == 4 {
		c.canceled.Store(true)
	}
	if c.canceled.Load() {
		return context.Canceled
	}
	return nil
}

func newLeaseProbeInvocation(t *testing.T, lease PreparedDispatchLease, execute func(context.Context, ToolCall) (*ToolResult, error)) *preparedInvocation {
	t.Helper()
	fence := NewDispatchFence()
	return newPreparedInvocation(preparedInvocationConfig{
		fence:         fence,
		generations:   fence.Snapshot(),
		call:          mustFreezeToolCall(t, ToolCall{Name: "lease-probe"}),
		dispatchLease: lease,
		execute:       execute,
	})
}
