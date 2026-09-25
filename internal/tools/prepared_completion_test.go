package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreparedCompletionProofValuesAreStable(t *testing.T) {
	assert.Equal(t, "inline_pipeline_joined", string(CompletionInlinePipelineJoined))
	assert.Equal(t, "queued_job_joined", string(CompletionQueuedJobJoined))
	assert.Equal(t, "owned_process_group_joined", string(CompletionOwnedProcessGroupJoined))
}

func TestPreparedCompletionInlineSignalWaitsForExecutorReturn_HelperOnly(t *testing.T) {
	// Prepared Write/Edit registration deliberately requires the mutation queue.
	// This directly exercises the sealed invocation's internal inline signal
	// protocol without making that path reachable for those tools.
	ctx := context.Background()
	fence := NewDispatchFence()
	frozenCall, err := freezeToolCall(ToolCall{Name: "internal-test", Args: map[string]any{"value": "ok"}})
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	invocation := newPreparedInvocation(preparedInvocationConfig{
		fence:           fence,
		generations:     fence.Snapshot(),
		call:            frozenCall,
		completionProof: CompletionInlinePipelineJoined,
		execute: func(_ context.Context, call ToolCall) (*ToolResult, error) {
			close(entered)
			<-release
			if call.Args["value"] != "ok" {
				return nil, errors.New("frozen argument changed")
			}
			return &ToolResult{Output: "done"}, nil
		},
	})

	dispatchResult := make(chan error, 1)
	go func() {
		_, dispatchErr := invocation.Dispatch(ctx)
		dispatchResult <- dispatchErr
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("prepared executor did not enter")
	}

	signal := invocation.CompletionSignal()
	require.NotNil(t, signal.Done(), "the helper invocation exposes its action-scoped join channel")
	assert.Empty(t, signal.Proof(), "the completion enum is unavailable before the executor returns")
	select {
	case <-signal.Done():
		t.Fatal("completion channel closed before the executor returned")
	default:
	}

	close(release)
	require.NoError(t, <-dispatchResult)
	select {
	case <-signal.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("completion channel did not close after the executor returned")
	}
	assert.Equal(t, CompletionInlinePipelineJoined, signal.Proof())
}

func TestPreparedCompletionRequiresSynchronousWrapperAttestation(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"write"})))
	registry := NewMiddlewareToolRegistry(base)
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	defer queue.Close()
	wrapper := ToolExecutorWrapper(func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return func(ctx context.Context, call ToolCall) (*ToolResult, error) { return next(ctx, call) }
	})
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{
		ExecutionWrappers: []ToolExecutorWrapper{wrapper},
		MutationQueue:     queue,
	})

	path := filepath.Join(t.TempDir(), "write.txt")
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{"path": path, "content": "done"}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	signal := invocation.CompletionSignal()
	assert.Nil(t, signal.Done(), "an un-attested wrapper may detach work, so its return cannot establish quiescence")
	assert.Empty(t, signal.Proof())
}

func TestPreparedCompletionOrdinaryRegistrationAndReplacementHaveNoProof(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"write"})))
	custom := &preparedCaptureTool{name: "write", schema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"},
		},
		"required": []any{"path", "content"},
	}}
	require.NoError(t, base.Register(ctx, custom), "ordinary same-name replacement must replace, not inherit, completion provenance")
	registry := NewMiddlewareToolRegistry(base)
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	defer queue.Close()
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})

	path := filepath.Join(t.TempDir(), "custom.txt")
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{"path": path, "content": "custom"}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	signal := invocation.CompletionSignal()
	assert.Nil(t, signal.Done(), "a normal return from a custom same-name executor is not a proof")
	assert.Empty(t, signal.Proof())
	assert.Equal(t, 1, custom.snapshotCalls())
}

type preparedDetachedDiffGenerator struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *preparedDetachedDiffGenerator) Generate(context.Context, string, string, string) (string, error) {
	go func() {
		g.once.Do(func() { close(g.started) })
		<-g.release
	}()
	return "", nil
}

func TestPreparedCompletionUnknownDiffGeneratorHasNoInlineProof(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	diff := &preparedDetachedDiffGenerator{started: make(chan struct{}), release: make(chan struct{})}
	defer close(diff.release)
	require.NoError(t, RegisterDefaults(ctx, base,
		WithRegisteredBuiltinWhitelist([]string{"write"}),
		WithRegisteredDiffGenerator(diff),
	))
	registry := NewMiddlewareToolRegistry(base)
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	defer queue.Close()
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})

	path := filepath.Join(t.TempDir(), "existing.txt")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{
		"path": path, "content": "new", "append": true,
	}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	select {
	case <-diff.started:
	case <-time.After(5 * time.Second):
		t.Fatal("custom diff generator did not run for the existing file")
	}
	signal := invocation.CompletionSignal()
	assert.Nil(t, signal.Done(), "an injected helper-capable diff generator cannot inherit the built-in inline proof")
	assert.Empty(t, signal.Proof())
}

func TestPreparedCompletionCanceledQueueWaitsForExactAcceptedJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(context.Background(), base, WithRegisteredBuiltinWhitelist([]string{"write"})))
	registry := NewMiddlewareToolRegistry(base)
	started := make(chan struct{})
	release := make(chan struct{})
	var customHandlerCalls atomic.Int32
	queue := NewDefaultFileMutationQueue(WithMutationHandler(func(context.Context, FileMutation) (*ToolResult, error) {
		customHandlerCalls.Add(1)
		return &ToolResult{Output: "custom handler"}, nil
	})).(*DefaultFileMutationQueue) //nolint:errcheck
	wrapper := ToolExecutorWrapper(func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			close(started)
			<-release
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return next(ctx, call)
		}
	})
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{
		ExecutionWrappers:            []ToolExecutorWrapper{wrapper},
		SynchronousExecutionWrappers: true,
		MutationQueue:                queue,
	})
	defer queue.Close()

	path := filepath.Join(t.TempDir(), "queued.txt")
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{"path": path, "content": "queued"}})
	require.NoError(t, err)
	dispatchResult := make(chan error, 1)
	go func() {
		_, dispatchErr := invocation.Dispatch(ctx)
		dispatchResult <- dispatchErr
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("accepted prepared queue job did not enter its captured executor")
	}

	cancel()
	require.ErrorIs(t, <-dispatchResult, context.Canceled)
	signal := invocation.CompletionSignal()
	require.NotNil(t, signal.Done(), "an accepted queue job must remain owned after its caller stops waiting")
	assert.Empty(t, signal.Proof(), "queued job proof is unavailable until the exact job has returned")
	select {
	case <-signal.Done():
		t.Fatal("caller cancellation must not close a still-running queue job signal")
	default:
	}

	close(release)
	select {
	case <-signal.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("queue completion signal did not close after the accepted job returned")
	}
	assert.Equal(t, CompletionQueuedJobJoined, signal.Proof())
	assert.Zero(t, customHandlerCalls.Load(), "the proof is from the captured prepared executor, never a custom queue handler result")
	_, err = os.Stat(path)
	assert.True(t, errors.Is(err, os.ErrNotExist), "the canceled wrapper did not call the mutation executor")
}

func TestPreparedCompletionRejectedQueueSubmissionHasNoSignal(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, base, WithRegisteredBuiltinWhitelist([]string{"write"})))
	registry := NewMiddlewareToolRegistry(base)
	queue := NewDefaultFileMutationQueue().(*DefaultFileMutationQueue) //nolint:errcheck
	require.NoError(t, queue.Close())
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})

	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{
		"path": filepath.Join(t.TempDir(), "rejected.txt"), "content": "never",
	}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.Error(t, err, "the closed queue must reject the submission before accepting a job")
	assert.Nil(t, invocation.CompletionSignal().Done())
	assert.Empty(t, invocation.CompletionSignal().Proof())
}

func TestPreparedCompletionLocalBashWaitsForProcessAndWrapperJoin(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.registerTrustedLocalProcess(ctx, NewBashTool(WithNoSandbox())))
	registry := NewMiddlewareToolRegistry(base)
	innerReturned := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	wrapper := ToolExecutorWrapper(func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			result, err := next(ctx, call)
			close(innerReturned)
			<-release
			return result, err
		}
	})
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{
		ExecutionWrappers:            []ToolExecutorWrapper{wrapper},
		SynchronousExecutionWrappers: true,
	})
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": "sleep 0.1 & wait; printf joined"}})
	require.NoError(t, err)
	dispatchResult := make(chan error, 1)
	go func() {
		_, dispatchErr := invocation.Dispatch(ctx)
		dispatchResult <- dispatchErr
	}()
	select {
	case <-innerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("local executor did not return after joining the owned process group")
	}
	signal := invocation.CompletionSignal()
	require.NotNil(t, signal.Done())
	assert.Empty(t, signal.Proof(), "Bash's process-group report cannot close the public signal while a synchronous wrapper is still running")
	select {
	case <-signal.Done():
		t.Fatal("public completion closed before the trusted wrapper returned")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-dispatchResult)
	select {
	case <-signal.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("local process completion channel did not close after the owned group joined")
	}
	assert.Equal(t, CompletionOwnedProcessGroupJoined, signal.Proof())
}

func TestPreparedCompletionStreamingBashHasNoProofWithoutTrustedSink(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, RegisterDefaults(ctx, base,
		WithRegisteredBuiltinWhitelist([]string{"bash"}),
		WithRegisteredBashSandbox(AllowAllSandbox{}),
	))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": "printf joined"}})
	require.NoError(t, err)
	assert.Nil(t, invocation.CompletionSignal().Done(), "the dispatch-time StreamSink has no prepare-time synchronous attestation")
	streaming, ok := invocation.(PreparedStreamingInvocation)
	require.True(t, ok)
	_, err = streaming.DispatchStreaming(ctx, &preparedCaptureSink{})
	require.NoError(t, err)
	assert.Nil(t, invocation.CompletionSignal().Done(), "the channel stays unavailable when the supplied sink is untrusted")
	assert.Empty(t, invocation.CompletionSignal().Proof())
}

func TestPreparedCompletionOrdinaryBashRegistrationDoesNotInferProof(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.Register(ctx, NewBashTool(WithNoSandbox())))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": "printf ordinary"}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	signal := invocation.CompletionSignal()
	assert.Nil(t, signal.Done(), "ordinary registration cannot mint a process-group proof")
	assert.Empty(t, signal.Proof())
}

type preparedDetachedSandbox struct {
	started chan struct{}
	release chan struct{}
}

func (s *preparedDetachedSandbox) Validate(context.Context, string, string) error {
	go func() {
		close(s.started)
		<-s.release
	}()
	return nil
}

func TestPreparedCompletionCustomSandboxDoesNotInheritProcessProof(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	sandbox := &preparedDetachedSandbox{started: make(chan struct{}), release: make(chan struct{})}
	defer close(sandbox.release)
	require.NoError(t, base.registerTrustedLocalProcess(ctx, NewBashTool(WithBashSandbox(sandbox))))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": "printf sandbox"}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	select {
	case <-sandbox.started:
	case <-time.After(5 * time.Second):
		t.Fatal("custom sandbox did not start its asynchronous work")
	}
	assert.Nil(t, invocation.CompletionSignal().Done(), "a custom sandbox can outlive the local process group")
	assert.Empty(t, invocation.CompletionSignal().Proof())
}

type preparedDetachedStreamingTool struct {
	started chan struct{}
	release chan struct{}
}

func (*preparedDetachedStreamingTool) Name() string        { return "bash" }
func (*preparedDetachedStreamingTool) Description() string { return "custom streaming executor" }
func (*preparedDetachedStreamingTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
		},
		"required": []any{"command"},
	}
}
func (t *preparedDetachedStreamingTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	return t.ExecuteStreaming(ctx, call, nil)
}
func (t *preparedDetachedStreamingTool) ExecuteStreaming(context.Context, ToolCall, StreamSink) (*ToolResult, error) {
	go func() {
		close(t.started)
		<-t.release
	}()
	return &ToolResult{Output: "streamed"}, nil
}

func TestPreparedCompletionCustomStreamingExecutorDoesNotInheritProof(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := &preparedDetachedStreamingTool{started: make(chan struct{}), release: make(chan struct{})}
	defer close(tool.release)
	require.NoError(t, base.Register(ctx, tool))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "bash", Args: map[string]any{"command": "custom"}})
	require.NoError(t, err)
	streaming, ok := invocation.(PreparedStreamingInvocation)
	require.True(t, ok)
	_, err = streaming.DispatchStreaming(ctx, &preparedCaptureSink{})
	require.NoError(t, err)
	select {
	case <-tool.started:
	case <-time.After(5 * time.Second):
		t.Fatal("custom stream executor did not start its asynchronous work")
	}
	assert.Nil(t, invocation.CompletionSignal().Done(), "ordinary custom StreamingBashTool registration has no local process-group authority")
	assert.Empty(t, invocation.CompletionSignal().Proof())
}
