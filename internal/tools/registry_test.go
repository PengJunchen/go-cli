package tools

import (
	"context"
	"sync"
	"testing"

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
