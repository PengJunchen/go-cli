package tools

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

// stubTool is a minimal ToolDefinition used to exercise the registry.
type stubTool struct {
	name string
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub " + s.name }
func (s *stubTool) Execute(_ context.Context, call ToolCall) (*ToolResult, error) {
	return &ToolResult{
		Output:   "ran " + s.name,
		Metadata: map[string]any{"name": s.name, "id": call.ID},
	}, nil
}

type schemaStubTool struct {
	stubTool
	schema any
}

func (s *schemaStubTool) Parameters() any { return s.schema }

var errProjectTrustBindingRevoked = errors.New("project trust binding revoked")

type projectTrustBindingStub struct {
	root       string
	generation uint64
	valid      bool
}

func (b *projectTrustBindingStub) CanonicalRoot() string   { return b.root }
func (b *projectTrustBindingStub) TrustGeneration() uint64 { return b.generation }
func (b *projectTrustBindingStub) Valid() bool             { return b.valid }
func (b *projectTrustBindingStub) CommitIfCurrent(ctx context.Context, commit func() error) error {
	if !b.valid {
		return errProjectTrustBindingRevoked
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return commit()
}

func TestRegistryRegisterGetList(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	a := &stubTool{name: "alpha"}
	b := &stubTool{name: "beta"}

	require.NoError(t, reg.Register(ctx, a))
	require.NoError(t, reg.Register(ctx, b))

	got, err := reg.Get(ctx, "alpha")
	require.NoError(t, err)
	assert.Same(t, a, got)

	gotB, err := reg.Get(ctx, "beta")
	require.NoError(t, err)
	assert.Same(t, b, gotB)

	list, err := reg.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	// Registration order preserved.
	assert.Same(t, a, list[0])
	assert.Same(t, b, list[1])
}

func TestRegistryGetUnknown(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	_, err := reg.Get(ctx, "nope")
	assert.ErrorIs(t, err, ErrToolNotFound)
}

func TestRegistryRegisterNilAndEmptyName(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	assert.Error(t, reg.Register(ctx, nil))
	assert.Error(t, reg.Register(ctx, &stubTool{name: ""}))
}

func TestRegistryDuplicateOverwrite(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	first := &stubTool{name: "tool"}
	second := &stubTool{name: "tool"}

	require.NoError(t, reg.Register(ctx, first))
	require.NoError(t, reg.Register(ctx, second))

	got, err := reg.Get(ctx, "tool")
	require.NoError(t, err)
	// Last registration wins.
	assert.Same(t, second, got)

	// List still reports one entry (name preserved in position).
	list, err := reg.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Same(t, second, list[0])
}

func TestRegistryListReturnsCopy(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()
	require.NoError(t, reg.Register(ctx, &stubTool{name: "x"}))

	list1, err := reg.List(ctx)
	require.NoError(t, err)
	list1[0] = nil

	// The registry's internal slice is untouched.
	list2, err := reg.List(ctx)
	require.NoError(t, err)
	assert.NotNil(t, list2[0])
}

func TestRegistryConcurrentRegisterGet(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry()

	const workers = 20
	const toolsPerWorker = 10

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < toolsPerWorker; j++ {
				name := "t"
				require.NoError(t, reg.Register(ctx, &stubTool{name: name}))
				_, err := reg.Get(ctx, name)
				if err != nil {
					t.Errorf("concurrent Get failed: %v", err)
				}
				_, err = reg.List(ctx)
				if err != nil {
					t.Errorf("concurrent List failed: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()

	// Sanity: reads succeed after all registrations settle.
	_, err := reg.Get(ctx, "t")
	assert.NoError(t, err)
}

func TestRegistryExecute(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx := context.Background()
	reg := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, reg.Register(ctx, &stubTool{name: "alpha"}))

	res, err := reg.Execute(ctx, ToolCall{ID: "call-1", Name: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, "ran alpha", res.Output)
	assert.Equal(t, "call-1", res.Metadata["id"])

	// Unknown tool returns an error via Execute.
	_, err = reg.Execute(ctx, ToolCall{ID: "call-2", Name: "missing"})
	assert.ErrorIs(t, err, ErrToolNotFound)

	// Empty name is rejected.
	_, err = reg.Execute(ctx, ToolCall{ID: "call-3"})
	assert.Error(t, err)
}

func TestRegistryExecuteWithSpanContext(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	ctx := context.Background()
	require.NoError(t, reg.Register(ctx, &stubTool{name: "alpha"}))

	// A tracer-injected context should not change behavior; Execute must still
	// succeed. This exercises the SpanFromContext path.
	res, err := reg.Execute(ctx, ToolCall{ID: "c", Name: "alpha"})
	require.NoError(t, err)
	assert.Equal(t, "ran alpha", res.Output)
}

func TestRegistryRegistrationSnapshotBindsSchemaIdentityEffectsAndFence(t *testing.T) {
	ctx := context.Background()
	reg := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	schema := map[string]any{
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
		"required":   []string{"value"},
	}
	first := &schemaStubTool{stubTool: stubTool{name: "tool"}, schema: schema}
	require.NoError(t, reg.Register(ctx, first))

	firstSnapshot, err := reg.toolRegistrationSnapshot(ctx, "tool")
	require.NoError(t, err)
	assert.Same(t, first, firstSnapshot.definition)
	assert.Equal(t, RegistrationSourceToolRegistry, firstSnapshot.identity.RegistrationSource())
	assert.Equal(t, []TrustedToolEffect{EffectUnknown}, firstSnapshot.effects.Values())
	require.True(t, firstSnapshot.schema.Present())
	assert.Equal(t, firstSnapshot.identity.RegistrationGeneration(), uint64(1))
	assert.Equal(t, uint64(1), firstSnapshot.generations.Registry)

	schema["properties"].(map[string]any)["value"].(map[string]any)["type"] = "number"
	assert.Equal(t, "string", firstSnapshot.schema.Schema().(map[string]any)["properties"].(map[string]any)["value"].(map[string]any)["type"])

	second := &schemaStubTool{stubTool: stubTool{name: "tool"}, schema: map[string]any{"type": "object"}}
	require.NoError(t, reg.Register(ctx, second))
	secondSnapshot, err := reg.toolRegistrationSnapshot(ctx, "tool")
	require.NoError(t, err)
	assert.Same(t, second, secondSnapshot.definition)
	assert.Equal(t, uint64(2), secondSnapshot.identity.RegistrationGeneration())
	assert.Equal(t, uint64(2), secondSnapshot.generations.Registry)
	assert.NotEqual(t, firstSnapshot.identity.ExecutorFingerprint(), secondSnapshot.identity.ExecutorFingerprint())
	assert.Equal(t, [32]byte{}, firstSnapshot.identity.Scope().Fingerprint(), "unscoped ordinary registrations carry no scope authority")
}

func TestProjectSourcedRegistrationCarriesBindingThroughPreparedProxy(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	deferred := NewDeferredToolRegistryAdapter(base)
	registry := NewMiddlewareToolRegistry(deferred)
	binding := &projectTrustBindingStub{root: "/workspace", generation: 7, valid: true}
	var calls int
	definition := &countingProjectTool{name: "project_tool", calls: &calls}

	registrar, ok := any(registry).(ProjectSourcedToolRegistrar)
	require.True(t, ok)
	require.NoError(t, registrar.RegisterProjectSourced(ctx, definition, binding))

	snapshot, err := base.toolRegistrationSnapshot(ctx, definition.Name())
	require.NoError(t, err)
	assert.Equal(t, RegistrationSourceProjectSourced, snapshot.identity.RegistrationSource())
	assert.Same(t, binding, snapshot.projectTrustBinding)

	require.NoError(t, registry.SetPreparedToolStages(PreparedToolStages{}))
	require.NoError(t, registry.SetPreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	prepared, err := registry.PrepareToolCall(ctx, ToolCall{ID: "call", Name: definition.Name()})
	require.NoError(t, err)
	binding.valid = false
	_, err = prepared.Dispatch(ctx)
	assert.ErrorIs(t, err, errProjectTrustBindingRevoked)
	assert.False(t, prepared.DispatchCommitted())
	assert.Equal(t, 0, calls)
}

func TestProjectSourcedDeferredRegistrationRetainsBindingWhenLoaded(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	deferred := NewDeferredToolRegistryAdapter(base)
	registry := NewMiddlewareToolRegistry(deferred)
	binding := &projectTrustBindingStub{root: "/workspace", generation: 3, valid: true}
	definition := &stubTool{name: "project_skill"}

	registrar, ok := any(registry).(ProjectSourcedDeferredToolRegistrar)
	require.True(t, ok)
	require.NoError(t, registrar.RegisterProjectSourcedDeferred(ctx, definition.Name(), func() (ToolDefinition, error) {
		return definition, nil
	}, binding))

	loaded, err := deferred.Load(ctx, definition.Name())
	require.NoError(t, err)
	assert.Same(t, definition, loaded)
	snapshot, err := base.toolRegistrationSnapshot(ctx, definition.Name())
	require.NoError(t, err)
	assert.Same(t, binding, snapshot.projectTrustBinding)
	assert.Equal(t, RegistrationSourceProjectSourced, snapshot.identity.RegistrationSource())
}

func TestProjectTrustCommitGuardsStreamingAndQueuedDispatch(t *testing.T) {
	t.Run("streaming", func(t *testing.T) {
		ctx := context.Background()
		base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
		binding := &projectTrustBindingStub{root: "/workspace", generation: 4, valid: true}
		tool := &preparedStreamingCaptureTool{preparedCaptureTool: preparedCaptureTool{name: "project_stream"}}
		require.NoError(t, base.RegisterProjectSourced(ctx, tool, binding))
		registry := NewMiddlewareToolRegistry(base)
		configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{})
		preparer, ok := any(registry).(StreamingToolCallPreparer)
		require.True(t, ok)
		sink := &preparedCaptureSink{}
		attested, err := AttestSynchronousStreamSink(sink)
		require.NoError(t, err)
		invocation, err := preparer.PrepareStreamingToolCall(ctx, ToolCall{Name: tool.Name()}, attested)
		require.NoError(t, err)
		binding.valid = false

		_, err = invocation.DispatchStreaming(ctx, attested)
		assert.ErrorIs(t, err, errProjectTrustBindingRevoked)
		assert.False(t, invocation.DispatchCommitted())
		assert.Zero(t, tool.streamCalls)
		assert.Empty(t, sink.content, "stale streaming calls cannot emit a chunk before the final commit")
	})

	t.Run("queued mutation", func(t *testing.T) {
		ctx := &preparedQueueGateContext{
			Context:          context.Background(),
			workerCheckStart: make(chan struct{}),
			allowWorkerCheck: make(chan struct{}),
		}
		base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
		binding := &projectTrustBindingStub{root: "/workspace", generation: 9, valid: true}
		tool := &preparedCaptureTool{name: "write"}
		require.NoError(t, base.RegisterProjectSourced(ctx, tool, binding))
		queue := NewDefaultFileMutationQueue()
		t.Cleanup(func() {
			select {
			case <-ctx.allowWorkerCheck:
			default:
				close(ctx.allowWorkerCheck)
			}
			assert.NoError(t, queue.(interface{ Close() error }).Close())
		})
		registry := NewMiddlewareToolRegistry(base)
		configurePreparedRegistry(t, registry, allowPreparedCall, PreparedToolStages{MutationQueue: queue})
		path := filepath.Join(t.TempDir(), "revoked-before-dequeue.txt")
		invocation, err := registry.PrepareToolCall(ctx, ToolCall{Name: tool.Name(), Args: map[string]any{"path": path, "content": "must not run"}})
		require.NoError(t, err)

		type dispatchResult struct {
			err error
		}
		dispatchDone := make(chan dispatchResult, 1)
		go func() {
			_, dispatchErr := invocation.Dispatch(ctx)
			dispatchDone <- dispatchResult{err: dispatchErr}
		}()
		select {
		case <-ctx.workerCheckStart:
		case <-time.After(3 * time.Second):
			t.Fatal("mutation worker did not reach its dequeue-time commit gate")
		}
		binding.valid = false
		close(ctx.allowWorkerCheck)
		select {
		case completed := <-dispatchDone:
			assert.ErrorIs(t, completed.err, errProjectTrustBindingRevoked)
		case <-time.After(3 * time.Second):
			t.Fatal("queued mutation did not report the stale project binding")
		}
		assert.False(t, invocation.DispatchCommitted())
		assert.Zero(t, tool.snapshotCalls())
		assert.NoFileExists(t, path, "a stale queued mutation must not write after dequeue")
		_, err = invocation.Dispatch(ctx)
		assert.ErrorIs(t, err, ErrPreparedInvocationConsumed)
	})
}

type countingProjectTool struct {
	name  string
	calls *int
}

func (t *countingProjectTool) Name() string        { return t.name }
func (t *countingProjectTool) Description() string { return "project tool" }
func (t *countingProjectTool) Execute(context.Context, ToolCall) (*ToolResult, error) {
	*t.calls++
	return &ToolResult{Output: "ran"}, nil
}

func TestRegistryTrustedControlRequiresExplicitRegistrationAndSharesFence(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	deferred := NewDeferredToolRegistryAdapter(base)
	firstMiddleware := NewMiddlewareToolRegistry(deferred)
	outerMiddleware := NewMiddlewareToolRegistry(firstMiddleware)
	var registry ToolRegistry = outerMiddleware

	fenceProvider, ok := registry.(DispatchFenceProvider)
	require.True(t, ok)
	assert.Same(t, base.DispatchFence(), fenceProvider.DispatchFence())

	registrar, ok := registry.(TrustedControlRegistrar)
	require.True(t, ok)
	ordinary := &stubTool{name: "ask_user"}
	require.NoError(t, registry.Register(ctx, ordinary))
	ordinarySnapshot, err := base.toolRegistrationSnapshot(ctx, "ask_user")
	require.NoError(t, err)
	assert.Equal(t, RegistrationSourceToolRegistry, ordinarySnapshot.identity.RegistrationSource())
	assert.Equal(t, []TrustedToolEffect{EffectUnknown}, ordinarySnapshot.effects.Values())

	control := &stubTool{name: "ask_user"}
	require.NoError(t, registrar.RegisterTrustedControl(ctx, control))
	controlSnapshot, err := base.toolRegistrationSnapshot(ctx, "ask_user")
	require.NoError(t, err)
	assert.Same(t, control, controlSnapshot.definition)
	assert.Equal(t, RegistrationSourceTrustedControl, controlSnapshot.identity.RegistrationSource())
	assert.Equal(t, []TrustedToolEffect{EffectControl}, controlSnapshot.effects.Values())
	assert.Equal(t, uint64(2), controlSnapshot.identity.RegistrationGeneration())
}

func TestTrustedNetworkReadRegistrationAcceptsOnlyReviewedConcreteTools(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	deferred := NewDeferredToolRegistryAdapter(base)
	registry := NewMiddlewareToolRegistry(deferred)
	registrar, ok := any(registry).(TrustedNetworkReadRegistrar)
	require.True(t, ok)

	err := registrar.RegisterTrustedNetworkRead(ctx, &stubTool{name: "web_search"})
	require.ErrorIs(t, err, ErrUnreviewedNetworkReadDefinition)

	webSearch := NewWebSearchTool()
	require.NoError(t, registrar.RegisterTrustedNetworkRead(ctx, webSearch))
	snapshot, err := base.toolRegistrationSnapshot(ctx, "web_search")
	require.NoError(t, err)
	assert.Same(t, webSearch, snapshot.definition)
	assert.Equal(t, RegistrationSourceTrustedNetworkRead, snapshot.identity.RegistrationSource())
	assert.Equal(t, []TrustedToolEffect{EffectNetworkRead}, snapshot.effects.Values())
	assert.Equal(t, [32]byte{}, snapshot.identity.Scope().Fingerprint(), "network-read category does not assert endpoint scope")

	ordinary := NewWebFetchTool()
	require.NoError(t, registry.Register(ctx, ordinary))
	ordinarySnapshot, err := base.toolRegistrationSnapshot(ctx, ordinary.Name())
	require.NoError(t, err)
	assert.Equal(t, RegistrationSourceToolRegistry, ordinarySnapshot.identity.RegistrationSource())
	assert.Equal(t, []TrustedToolEffect{EffectUnknown}, ordinarySnapshot.effects.Values())
}

func TestPreparedRegistryPolicyAndStagesShareFenceAndInvalidatePermits(t *testing.T) {
	ctx := context.Background()
	base := NewDefaultToolRegistry().(*DefaultToolRegistry) //nolint:errcheck
	require.NoError(t, base.Register(ctx, &stubTool{name: "prepared"}))
	deferred := NewDeferredToolRegistryAdapter(base)
	inner := NewMiddlewareToolRegistry(deferred)
	outer := NewMiddlewareToolRegistry(inner)
	fence := base.DispatchFence()
	initial := fence.Snapshot()

	preparers := []ArgumentPreparer{NewPathNormalizer("/workspace")}
	originalPreparer := preparers[0]
	stages := PreparedToolStages{ArgumentPreparers: preparers}
	require.NoError(t, outer.SetPreparedToolStages(stages))
	afterStages := fence.Snapshot()
	assert.Equal(t, initial.Registry+1, afterStages.Registry)
	assert.Equal(t, initial.Approval, afterStages.Approval)
	assert.Equal(t, initial.Plan, afterStages.Plan)

	policy := PreparedToolPolicy(func(_ context.Context, request PreparedPolicyRequest) (PreparedPolicyDecision, error) {
		return PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	})
	require.NoError(t, outer.SetPreparedToolPolicy(policy))
	afterPolicy := fence.Snapshot()
	assert.Equal(t, afterStages.Registry, afterPolicy.Registry)
	assert.Equal(t, afterStages.Approval+1, afterPolicy.Approval)
	assert.Equal(t, afterStages.Plan, afterPolicy.Plan)

	snapshot, err := outer.toolRegistrationSnapshot(ctx, "prepared")
	require.NoError(t, err)
	assert.Same(t, fence, outer.DispatchFence())
	assert.NotNil(t, snapshot.pipeline.policy)
	assert.Len(t, snapshot.pipeline.stages.ArgumentPreparers, 1)
	assert.True(t, snapshot.pipeline.policyConfigured)
	assert.True(t, snapshot.pipeline.stagesConfigured)
	assert.Equal(t, afterPolicy, snapshot.generations)

	// Mutating the original slices after installation cannot replace a stage.
	preparers[0] = NewPathNormalizer("/different")
	snapshotAfterCallerMutation, err := base.toolRegistrationSnapshot(ctx, "prepared")
	require.NoError(t, err)
	assert.Same(t, originalPreparer, snapshotAfterCallerMutation.pipeline.stages.ArgumentPreparers[0])
}
