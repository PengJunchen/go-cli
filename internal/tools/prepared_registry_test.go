package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type preparedCaptureTool struct {
	name   string
	schema any
	mu     sync.Mutex
	calls  int
	last   ToolCall
}

func (t *preparedCaptureTool) Name() string        { return t.name }
func (t *preparedCaptureTool) Description() string { return "prepared capture " + t.name }
func (t *preparedCaptureTool) Parameters() any     { return t.schema }
func (t *preparedCaptureTool) PromptGuidelines() []string {
	return []string{"Use this prepared tool carefully."}
}
func (t *preparedCaptureTool) Execute(_ context.Context, call ToolCall) (*ToolResult, error) {
	frozen, err := freezeToolCall(call)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.calls++
	t.last = frozen.ToolCall()
	t.mu.Unlock()
	return &ToolResult{Output: call.Name}, nil
}
func (t *preparedCaptureTool) snapshot() (int, ToolCall) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, t.last
}

type preparedStreamingCaptureTool struct {
	preparedCaptureTool
	streamCalls int
}

func (t *preparedStreamingCaptureTool) ExecuteStreaming(ctx context.Context, call ToolCall, sink StreamSink) (*ToolResult, error) {
	if sink != nil {
		if err := sink.Send("streamed", call.ID, "stdout"); err != nil {
			return nil, err
		}
	}
	t.streamCalls++
	return t.Execute(ctx, call)
}

type preparedCaptureSink struct {
	content string
	callID  string
	stream  string
}

type preparedQueueGateContext struct {
	context.Context
	errCalls         atomic.Int32
	workerCheckStart chan struct{}
	allowWorkerCheck chan struct{}
}

func (c *preparedQueueGateContext) Err() error {
	if c.errCalls.Add(1) == 3 {
		close(c.workerCheckStart)
		<-c.allowWorkerCheck
	}
	return c.Context.Err()
}

func (s *preparedCaptureSink) Send(content, callID, stream string) error {
	s.content, s.callID, s.stream = content, callID, stream
	return nil
}

func configurePreparedRegistry(t *testing.T, registry *MiddlewareToolRegistry, policy PreparedToolPolicy, stages PreparedToolStages) {
	t.Helper()
	require.NoError(t, registry.SetPreparedToolStages(stages))
	require.NoError(t, registry.SetPreparedToolPolicy(policy))
}

func allowPreparedCall(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
	return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
}

func TestPrepareToolCallNormalizesValidatesAndFreezesCapturedArguments(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := &preparedCaptureTool{
		name: "capture",
		schema: map[string]any{
			"type":     "object",
			"required": []string{"path", "content"},
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
		},
	}
	require.NoError(t, base.Register(ctx, tool))
	registry := NewMiddlewareToolRegistry(NewDeferredToolRegistryAdapter(base))
	root := t.TempDir()
	var policyCalls int
	policy := PreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		policyCalls++
		call := request.Call.ToolCall()
		assert.Equal(t, "capture", call.Name)
		assert.Equal(t, root+"/relative.txt", call.Args["path"])
		assert.Equal(t, RegistrationSourceToolRegistry, request.Identity.RegistrationSource())
		assert.Equal(t, []TrustedToolEffect{EffectUnknown}, request.Effects.Values())
		assert.True(t, request.Schema.Present())
		assert.Same(t, base.DispatchFence(), request.Fence)
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	})
	configurePreparedRegistry(t, registry, policy, PreparedToolStages{
		ArgumentPreparers: []ArgumentPreparer{NewPathNormalizer(root)},
	})

	args := map[string]any{"path": "relative.txt", "content": "original"}
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{ID: "call-1", Name: "capture", Args: args})
	require.NoError(t, err)
	require.Equal(t, 1, policyCalls)
	args["content"] = "caller changed after approval"

	normalized := invocation.NormalizedCall()
	assert.Equal(t, root+"/relative.txt", normalized.Args["path"])
	assert.Equal(t, "original", normalized.Args["content"])
	_, err = invocation.Dispatch(ctx)
	require.NoError(t, err)
	calls, executed := tool.snapshot()
	require.Equal(t, 1, calls)
	assert.Equal(t, root+"/relative.txt", executed.Args["path"])
	assert.Equal(t, "original", executed.Args["content"])
	assert.True(t, invocation.DispatchCommitted())
	_, err = invocation.Dispatch(ctx)
	require.ErrorIs(t, err, ErrPreparedInvocationConsumed)
	assert.Equal(t, 1, tool.snapshotCalls())
}

func TestPrepareToolCallRejectsInvalidSchemaBeforePolicy(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := &preparedCaptureTool{
		name: "capture",
		schema: map[string]any{
			"type":     "object",
			"required": []string{"content"},
			"properties": map[string]any{
				"content": map[string]any{"type": "string"},
			},
		},
	}
	require.NoError(t, base.Register(ctx, tool))
	registry := NewMiddlewareToolRegistry(base)
	var policyCalls int
	configurePreparedRegistry(t, registry, func(context.Context, PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		policyCalls++
		return PreparedPolicyDecision{Allowed: true, Generations: registry.DispatchFence().Snapshot()}, nil
	}, PreparedToolStages{})

	_, err := registry.PrepareToolCall(ctx, ToolCall{Name: "capture", Args: map[string]any{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required parameter")
	assert.Zero(t, policyCalls, "invalid arguments must be rejected before approval")
	calls, _ := tool.snapshot()
	assert.Zero(t, calls)
}

func TestPreparedExecutionDecoratorCannotChangeApprovedArguments(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := &preparedCaptureTool{name: "capture"}
	require.NoError(t, base.Register(ctx, tool))
	registry := NewMiddlewareToolRegistry(base)
	mutatingDecorator := ToolExecutorWrapper(func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return func(ctx context.Context, call ToolCall) (*ToolResult, error) {
			call.Args["value"] = "changed"
			return next(ctx, call)
		}
	})
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{ExecutionWrappers: []ToolExecutorWrapper{mutatingDecorator}})

	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "capture", Args: map[string]any{"value": "approved"}})
	require.NoError(t, err)
	_, err = invocation.Dispatch(ctx)
	require.ErrorIs(t, err, ErrPreparedCallMutation)
	assert.True(t, invocation.DispatchCommitted(), "the fence commit precedes the execution decorator")
	calls, _ := tool.snapshot()
	assert.Zero(t, calls)
}

func TestPreparedPolicyReplacementStalesPriorInvocation(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := &preparedCaptureTool{name: "capture"}
	require.NoError(t, base.Register(ctx, tool))
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})

	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "capture"})
	require.NoError(t, err)
	require.NoError(t, registry.SetPreparedToolPolicy(allowPreparedCall))
	_, err = invocation.Dispatch(ctx)
	require.ErrorIs(t, err, ErrDispatchFenceChanged)
	assert.False(t, invocation.DispatchCommitted())
	calls, _ := tool.snapshot()
	assert.Zero(t, calls)
}

func TestPreparedExecutionFailsClosedWithoutPolicyAndForOpaqueWrappers(t *testing.T) {
	ctx := context.Background()
	tool := &preparedCaptureTool{name: "capture"}
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.Register(ctx, tool))

	registry := NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{}))
	def, err := registry.Get(ctx, "capture")
	require.NoError(t, err)
	_, err = def.Execute(ctx, ToolCall{Name: "capture"})
	require.ErrorIs(t, err, ErrPreparedToolPolicyUnavailable)
	assert.Zero(t, tool.snapshotCalls())

	legacy := NewMiddlewareToolRegistry(base, func(next func(context.Context, ToolCall) (*ToolResult, error)) func(context.Context, ToolCall) (*ToolResult, error) {
		return next
	})
	configurePreparedRegistry(t, legacy, allowPreparedCall, PreparedToolStages{})
	_, err = legacy.PrepareToolCall(ctx, ToolCall{Name: "capture"})
	require.ErrorIs(t, err, ErrPreparedOpaqueWrapperUnsupported)
	assert.Zero(t, tool.snapshotCalls())
}

func TestPreparedMutationFailsClosedUntilCapturedQueueIsConfigured(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	write := NewWriteTool()
	require.NoError(t, base.Register(ctx, write))
	registry := NewMiddlewareToolRegistry(base)
	var policyCalls int
	configurePreparedRegistry(t, registry, func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		policyCalls++
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}, PreparedToolStages{})

	path := filepath.Join(t.TempDir(), "must-not-exist.txt")
	_, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{"path": path, "content": "no"}})
	require.ErrorIs(t, err, ErrPreparedMutationQueueUnavailable)
	assert.Zero(t, policyCalls, "unsupported prepared mutations fail before approval")
	_, err = os.Stat(path)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPreparedMutationUsesCapturedExecutorAndConsumesOnce(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := &preparedCaptureTool{name: "write"}
	require.NoError(t, base.Register(ctx, tool))
	var handlerCalls int
	queue := NewDefaultFileMutationQueue(WithMutationHandler(func(context.Context, FileMutation) (*ToolResult, error) {
		handlerCalls++
		return &ToolResult{Output: "wrong queue handler"}, nil
	}))
	t.Cleanup(func() { assert.NoError(t, queue.(interface{ Close() error }).Close()) })
	registry := NewMiddlewareToolRegistry(base)
	root := t.TempDir()
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{
		ArgumentPreparers: []ArgumentPreparer{NewPathNormalizer(root)},
		MutationQueue:     queue,
	})

	path := filepath.Join(root, "queued.txt")
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{"path": "queued.txt", "content": "payload"}})
	require.NoError(t, err)
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	assert.Equal(t, "write", result.Output)
	assert.Equal(t, true, result.Metadata["queued"])
	assert.True(t, invocation.DispatchCommitted())
	_, err = invocation.Dispatch(ctx)
	require.ErrorIs(t, err, ErrPreparedInvocationConsumed)
	calls, executed := tool.snapshot()
	assert.Equal(t, 1, calls)
	assert.Equal(t, path, executed.Args["path"])
	assert.Equal(t, "payload", executed.Args["content"])
	assert.Zero(t, handlerCalls, "prepared jobs invoke the captured definition, not a reconstructed queue handler")
}

func TestPreparedMutationDequeueRejectsStaleRegistration(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	registered := &preparedCaptureTool{name: "write"}
	require.NoError(t, base.Register(ctx, registered))
	queue := NewDefaultFileMutationQueue()
	t.Cleanup(func() { assert.NoError(t, queue.(interface{ Close() error }).Close()) })
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})

	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{
		"path": filepath.Join(t.TempDir(), "stale.txt"), "content": "old executor",
	}})
	require.NoError(t, err)
	replacement := &preparedCaptureTool{name: "write"}
	require.NoError(t, base.Register(ctx, replacement))

	_, err = invocation.Dispatch(ctx)
	require.ErrorIs(t, err, ErrDispatchFenceChanged)
	assert.False(t, invocation.DispatchCommitted())
	assert.Zero(t, registered.snapshotCalls())
	assert.Zero(t, replacement.snapshotCalls())
	_, err = invocation.Dispatch(ctx)
	assert.ErrorIs(t, err, ErrPreparedInvocationConsumed)
}

func TestPreparedMutationInvalidPayloadRejectedBeforePolicy(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.Register(ctx, &preparedCaptureTool{name: "write"}))
	queue := NewDefaultFileMutationQueue()
	t.Cleanup(func() { assert.NoError(t, queue.(interface{ Close() error }).Close()) })
	registry := NewMiddlewareToolRegistry(base)
	var policyCalls int
	configurePreparedRegistry(t, registry, func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		policyCalls++
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}, PreparedToolStages{MutationQueue: queue})

	_, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{"path": filepath.Join(t.TempDir(), "bad.txt")}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write requires a string argument 'content'")
	assert.Zero(t, policyCalls, "malformed mutation payloads must be rejected before approval")
}

func TestPreparedMutationCanceledAtDequeueCannotCommitLater(t *testing.T) {
	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := &preparedQueueGateContext{
		Context:          baseCtx,
		workerCheckStart: make(chan struct{}),
		allowWorkerCheck: make(chan struct{}),
	}
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := &preparedCaptureTool{name: "write"}
	require.NoError(t, base.Register(context.Background(), tool))
	queue := NewDefaultFileMutationQueue()
	t.Cleanup(func() {
		select {
		case <-ctx.allowWorkerCheck:
		default:
			close(ctx.allowWorkerCheck)
		}
		cancel()
		assert.NoError(t, queue.(interface{ Close() error }).Close())
	})
	registry := NewMiddlewareToolRegistry(base)
	configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})
	path := filepath.Join(t.TempDir(), "canceled.txt")
	invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: "write", Args: map[string]any{"path": path, "content": "never"}})
	require.NoError(t, err)

	dispatchDone := make(chan error, 1)
	go func() {
		_, dispatchErr := invocation.Dispatch(ctx)
		dispatchDone <- dispatchErr
	}()
	select {
	case <-ctx.workerCheckStart:
	case <-time.After(3 * time.Second):
		t.Fatal("queue worker did not validate the original dispatch context")
	}
	cancel()
	select {
	case err := <-dispatchDone:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("canceled Dispatch did not return")
	}
	close(ctx.allowWorkerCheck)

	barrier, err := queue.Enqueue(context.Background(), FileMutation{FilePath: path, Operation: "barrier"})
	require.NoError(t, err)
	select {
	case <-barrier:
	case <-time.After(3 * time.Second):
		t.Fatal("queue worker did not drain the canceled job")
	}
	assert.False(t, invocation.DispatchCommitted())
	assert.Zero(t, tool.snapshotCalls(), "a canceled queued invocation cannot execute after Dispatch returned")
	_, err = invocation.Dispatch(context.Background())
	assert.ErrorIs(t, err, ErrPreparedInvocationConsumed)
}

func TestProtectedRegistryGetListAndTopToolsPreserveCapabilities(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	alpha := &preparedCaptureTool{name: "alpha"}
	require.NoError(t, base.Register(ctx, alpha))
	search := NewToolSearchTool(base)
	require.NoError(t, base.RegisterTrustedControl(ctx, search))
	registry := NewMiddlewareToolRegistry(NewDeferredToolRegistryAdapter(base))
	var calls int
	policy := PreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		calls++
		assert.Equal(t, request.Call.ToolCall().Name, request.Identity.ToolName())
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	})
	configurePreparedRegistry(t, registry, policy, PreparedToolStages{})

	defs, err := registry.List(ctx)
	require.NoError(t, err)
	var alphaProxy, searchProxy ToolDefinition
	for _, def := range defs {
		switch def.Name() {
		case "alpha":
			alphaProxy = def
		case "tool_search":
			searchProxy = def
		}
	}
	require.NotNil(t, alphaProxy)
	require.NotNil(t, searchProxy)
	assert.NotSame(t, alpha, alphaProxy)
	assert.NotSame(t, search, searchProxy)
	_, hasParameters := alphaProxy.(Parameterized)
	assert.True(t, hasParameters, "proxy type %T must preserve Parameterized", alphaProxy)
	guideliner, hasGuidelines := alphaProxy.(PromptGuideliner)
	require.True(t, hasGuidelines)
	assert.Equal(t, []string{"Use this prepared tool carefully."}, guideliner.PromptGuidelines())
	_, err = alphaProxy.Execute(ctx, ToolCall{Name: "tool_search"})
	require.ErrorIs(t, err, ErrPreparedProxyNameMismatch)
	assert.Zero(t, calls, "a protected proxy cannot be used to substitute another registered name")

	result, err := alphaProxy.Execute(ctx, ToolCall{Name: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, "alpha", result.Output)

	topTools, ok := searchProxy.(TopToolsProvider)
	require.True(t, ok)
	selected, err := topTools.TopTools(ctx, "alpha", 1)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	assert.Equal(t, "alpha", selected[0].Name())
	assert.NotSame(t, alpha, selected[0])
	_, err = selected[0].Execute(ctx, ToolCall{Name: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "Get/List/Search execution all pass through the prepared policy")
}

func TestProtectedStreamingToolDispatchesThroughPreparedPermit(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	tool := &preparedStreamingCaptureTool{preparedCaptureTool: preparedCaptureTool{name: "stream"}}
	require.NoError(t, base.Register(ctx, tool))
	registry := NewMiddlewareToolRegistry(base)
	var policyCalls int
	configurePreparedRegistry(t, registry, func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		policyCalls++
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}, PreparedToolStages{})

	def, err := registry.Get(ctx, "stream")
	require.NoError(t, err)
	streaming, ok := def.(StreamingBashTool)
	require.True(t, ok)
	sink := &preparedCaptureSink{}
	result, err := streaming.ExecuteStreaming(ctx, ToolCall{ID: "stream-1", Name: "stream"}, sink)
	require.NoError(t, err)
	assert.Equal(t, "stream", result.Output)
	assert.Equal(t, "streamed", sink.content)
	assert.Equal(t, "stream-1", sink.callID)
	assert.Equal(t, "stdout", sink.stream)
	assert.Equal(t, 1, policyCalls)
	assert.Equal(t, 1, tool.streamCalls)
	calls, _ := tool.snapshot()
	assert.Equal(t, 1, calls)
}

func (t *preparedCaptureTool) snapshotCalls() int {
	calls, _ := t.snapshot()
	return calls
}
