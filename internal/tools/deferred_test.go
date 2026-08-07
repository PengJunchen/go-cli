package tools

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tracing"
	"github.com/pengjunchen/go-cli/internal/verify"
)

// recorderLoader is a loader that records how many times it was invoked and
// returns a fixed tool.
type recorderLoader struct {
	name    string
	calls   atomic.Int32
	payload ToolDefinition
	err     error
}

func (l *recorderLoader) fn() func() (ToolDefinition, error) {
	return func() (ToolDefinition, error) {
		l.calls.Add(1)
		if l.err != nil {
			return nil, l.err
		}
		return l.payload, nil
	}
}

func (l *recorderLoader) count() int32 { return l.calls.Load() }

// fakeTool is a minimal in-memory ToolDefinition for deferred tests. It cannot
// name collide with built-ins because each test registers fresh.
type fakeTool struct {
	name        string
	description string
}

func (f *fakeTool) Name() string { return f.name }
func (f *fakeTool) Description() string {
	return "fake tool " + f.name + ": " + f.description
}
func (f *fakeTool) Execute(_ context.Context, _ ToolCall) (*ToolResult, error) {
	return &ToolResult{Output: "ran " + f.name}, nil
}

func TestDeferredUnloadedReturnsStub(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := NewDefaultToolRegistry()
	dr := NewDefaultDeferredToolRegistry(reg)
	loader := &recorderLoader{name: "deferred_tool", payload: &fakeTool{name: "deferred_tool", description: "real"}}
	require.NoError(t, dr.RegisterDeferred(context.Background(), "deferred_tool", loader.fn()))

	// Before any Load the tool is neither loaded nor present in the backing
	// registry, and the loader has not been invoked.
	assert.False(t, dr.IsLoaded("deferred_tool"))
	_, err := reg.Get(context.Background(), "deferred_tool")
	assert.ErrorIs(t, err, ErrToolNotFound)
	assert.Equal(t, int32(0), loader.count())
}

func TestDeferredLoadTriggersLoaderOnce(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := NewDefaultToolRegistry()
	dr := NewDefaultDeferredToolRegistry(reg)
	loader := &recorderLoader{name: "deferred_tool", payload: &fakeTool{name: "deferred_tool", description: "real"}}
	require.NoError(t, dr.RegisterDeferred(context.Background(), "deferred_tool", loader.fn()))

	def, err := dr.Load(context.Background(), "deferred_tool")
	require.NoError(t, err)
	assert.Equal(t, "deferred_tool", def.Name())
	assert.Equal(t, int32(1), loader.count())
	assert.True(t, dr.IsLoaded("deferred_tool"))

	// The real tool is now registered in the backing registry.
	got, err := reg.Get(context.Background(), "deferred_tool")
	require.NoError(t, err)
	assert.Equal(t, "deferred_tool", got.Name())

	// A second Load must NOT re-invoke the loader.
	def2, err := dr.Load(context.Background(), "deferred_tool")
	require.NoError(t, err)
	assert.Same(t, def, def2)
	assert.Equal(t, int32(1), loader.count())
}

func TestDeferredLoadErrUnknownTool(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dr := NewDefaultDeferredToolRegistry(nil)
	_, err := dr.Load(context.Background(), "nope")
	assert.ErrorIs(t, err, ErrDeferredToolNotFound)
}

func TestDeferredLoaderErrorReturnsStubAndRetainsLoader(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dr := NewDefaultDeferredToolRegistry(nil)
	loader := &recorderLoader{name: "bad", err: errors.New("boom")}
	require.NoError(t, dr.RegisterDeferred(context.Background(), "bad", loader.fn()))

	def, err := dr.Load(context.Background(), "bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	// A stub is returned so execution neither blocks nor panics.
	require.NotNil(t, def)
	assert.Equal(t, "bad", def.Name())
	assert.True(t, dr.IsLoaded("bad"))

	// Executing the stub yields a not-loaded error, not a panic.
	_, execErr := def.Execute(context.Background(), ToolCall{Name: "bad"})
	assert.Error(t, execErr)

	// The loader ran exactly once.
	assert.Equal(t, int32(1), loader.count())
}

func TestDeferredParallelLoadCallsLoaderOnce(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	reg := NewDefaultToolRegistry()
	dr := NewDefaultDeferredToolRegistry(reg)
	loader := &recorderLoader{name: "deferred_tool", payload: &fakeTool{name: "deferred_tool", description: "real"}}
	require.NoError(t, dr.RegisterDeferred(context.Background(), "deferred_tool", loader.fn()))

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := dr.Load(context.Background(), "deferred_tool")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	// Double-checked locking: the loader ran exactly once despite concurrent Load.
	assert.Equal(t, int32(1), loader.count())
	assert.True(t, dr.IsLoaded("deferred_tool"))
}

func TestDeferredEmitsLoadSpan(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	e := &captureExporter{}
	tracer := tracing.NewTracer("trace-deferred", e)
	root, ctx := tracer.Start(context.Background(), "cli.invocation", tracing.SpanKindInternal)

	dr := NewDefaultDeferredToolRegistry(nil)
	loader := &recorderLoader{name: "deferred_tool", payload: &fakeTool{name: "deferred_tool", description: "real"}}
	require.NoError(t, dr.RegisterDeferred(ctx, "deferred_tool", loader.fn()))

	_, err := dr.Load(ctx, "deferred_tool")
	require.NoError(t, err)

	root.SetStatus(tracing.SpanStatusOK, "")
	root.End()

	require.Eventually(t, func() bool { return e.hasSpan("tool.call") }, 2*time.Second, 10*time.Millisecond)
}

// --- deferredStub tests ---

func TestDeferredStub_Description(t *testing.T) {
	s := &deferredStub{name: "my_tool"}
	assert.Equal(t, "my_tool: deferred tool (not yet loaded)", s.Description())
}

func TestDeferredStub_Execute_ReturnsError(t *testing.T) {
	s := &deferredStub{name: "my_tool"}
	_, err := s.Execute(context.Background(), ToolCall{Name: "my_tool"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not loaded")
}

func TestDeferredStub_Name(t *testing.T) {
	s := &deferredStub{name: "my_tool"}
	assert.Equal(t, "my_tool", s.Name())
}

// --- RegisterDeferred validation tests ---

func TestDeferredRegisterEmptyName(t *testing.T) {
	dr := NewDefaultDeferredToolRegistry(nil)
	err := dr.RegisterDeferred(context.Background(), "", func() (ToolDefinition, error) {
		return &fakeTool{name: "x"}, nil
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty name")
}

func TestDeferredRegisterNilLoader(t *testing.T) {
	dr := NewDefaultDeferredToolRegistry(nil)
	err := dr.RegisterDeferred(context.Background(), "tool", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// --- Deferred loader returns nil definition ---

func TestDeferredLoaderReturnsNil(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dr := NewDefaultDeferredToolRegistry(nil)
	loader := &recorderLoader{name: "nil_tool"}
	loader.payload = nil // nil payload
	require.NoError(t, dr.RegisterDeferred(context.Background(), "nil_tool", loader.fn()))

	def, err := dr.Load(context.Background(), "nil_tool")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil definition")
	// A stub is returned so execution neither blocks nor panics.
	require.NotNil(t, def)
	assert.Equal(t, "nil_tool", def.Name())
	assert.True(t, dr.IsLoaded("nil_tool"))
}

// --- RegisterDeferred overwrites previous loader ---

func TestDeferredRegisterOverwrites(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dr := NewDefaultDeferredToolRegistry(nil)
	first := &recorderLoader{name: "tool", payload: &fakeTool{name: "tool", description: "first"}}
	require.NoError(t, dr.RegisterDeferred(context.Background(), "tool", first.fn()))

	def, err := dr.Load(context.Background(), "tool")
	require.NoError(t, err)
	assert.Equal(t, "tool", def.Name())
	assert.True(t, dr.IsLoaded("tool"))

	// Re-registering resets the loaded state.
	second := &recorderLoader{name: "tool", payload: &fakeTool{name: "tool", description: "second"}}
	require.NoError(t, dr.RegisterDeferred(context.Background(), "tool", second.fn()))
	assert.False(t, dr.IsLoaded("tool"))

	// Load again uses the new loader.
	def2, err := dr.Load(context.Background(), "tool")
	require.NoError(t, err)
	assert.Equal(t, "tool", def2.Name())
	assert.Equal(t, int32(1), second.count())
}

// --- Load with underlying registry that fails to register ---

func TestDeferredLoadUnderlyingRegisterFails(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// A registry that always rejects registration.
	reg := &failingRegistry{}
	dr := NewDefaultDeferredToolRegistry(reg)
	loader := &recorderLoader{name: "tool", payload: &fakeTool{name: "tool", description: "real"}}
	require.NoError(t, dr.RegisterDeferred(context.Background(), "tool", loader.fn()))

	def, err := dr.Load(context.Background(), "tool")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "register")
	// The definition is still returned even when registration fails.
	require.NotNil(t, def)
	assert.Equal(t, "tool", def.Name())
}

// failingRegistry is a ToolRegistry that always returns an error on Register.
type failingRegistry struct{}

func (f *failingRegistry) Register(_ context.Context, _ ToolDefinition) error {
	return errors.New("register rejected")
}
func (f *failingRegistry) Get(_ context.Context, _ string) (ToolDefinition, error) {
	return nil, ErrToolNotFound
}
func (f *failingRegistry) List(_ context.Context) ([]ToolDefinition, error) { return nil, nil }

// --- Global deferred registry tests ---

func TestGetDeferredToolRegistry_Default(t *testing.T) {
	// Reset the global registry to nil first so we test the lazy default.
	deferredMu.Lock()
	defaultDeferred = nil
	deferredMu.Unlock()

	reg := GetDeferredToolRegistry()
	require.NotNil(t, reg)
	// Should be a DefaultDeferredToolRegistry.
	_, ok := reg.(*DefaultDeferredToolRegistry)
	assert.True(t, ok)
}

func TestRegisterDeferredToolRegistry_SetsGlobal(t *testing.T) {
	deferredMu.Lock()
	defaultDeferred = nil
	deferredMu.Unlock()

	inner := NewDefaultToolRegistry()
	custom := NewDefaultDeferredToolRegistry(inner)
	RegisterDeferredToolRegistry(custom)

	got := GetDeferredToolRegistry()
	assert.Equal(t, custom, got)
}

func TestRegisterDeferredToolRegistry_NilResetsToDefault(t *testing.T) {
	deferredMu.Lock()
	defaultDeferred = nil
	deferredMu.Unlock()

	inner := NewDefaultToolRegistry()
	custom := NewDefaultDeferredToolRegistry(inner)
	RegisterDeferredToolRegistry(custom)

	// Verify custom is set.
	got1 := GetDeferredToolRegistry()
	customConcrete := custom.(*DefaultDeferredToolRegistry) //nolint:errcheck
	got1Concrete := got1.(*DefaultDeferredToolRegistry)     //nolint:errcheck
	assert.Same(t, customConcrete, got1Concrete, "custom should be the global registry")

	// Passing nil resets to a fresh default.
	RegisterDeferredToolRegistry(nil)

	got2 := GetDeferredToolRegistry()
	got2Concrete := got2.(*DefaultDeferredToolRegistry) //nolint:errcheck
	assert.NotSame(t, customConcrete, got2Concrete, "after nil reset, the global should be a different instance")
}

func TestGetDeferredToolRegistry_CalledTwiceReturnsSame(t *testing.T) {
	deferredMu.Lock()
	defaultDeferred = nil
	deferredMu.Unlock()

	reg1 := GetDeferredToolRegistry()
	reg2 := GetDeferredToolRegistry()
	// Without a Register call, each Get creates a new instance.
	// But the important thing is both are non-nil and functional.
	require.NotNil(t, reg1)
	require.NotNil(t, reg2)
}

// --- DeferredToolRegistryAdapter tests ---

func TestAdapter_Register_PassThrough(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	adapter := NewDeferredToolRegistryAdapter(NewDefaultToolRegistry())

	tool := &fakeTool{name: "eager_tool", description: "registered eagerly"}
	require.NoError(t, adapter.Register(context.Background(), tool))

	// Register went straight to the underlying registry.
	def, err := adapter.Get(context.Background(), "eager_tool")
	require.NoError(t, err)
	assert.Equal(t, "eager_tool", def.Name())

	// List returns the eagerly registered tool.
	list, err := adapter.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "eager_tool", list[0].Name())
}

func TestAdapter_Get_LoadsOnDemand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	adapter := NewDeferredToolRegistryAdapter(NewDefaultToolRegistry())
	loader := &recorderLoader{name: "lazy_tool", payload: &fakeTool{name: "lazy_tool", description: "real"}}
	require.NoError(t, adapter.RegisterDeferred(context.Background(), "lazy_tool", loader.fn()))

	// Before Get, the loader has not run.
	assert.Equal(t, int32(0), loader.count())

	// Get triggers the loader.
	def, err := adapter.Get(context.Background(), "lazy_tool")
	require.NoError(t, err)
	assert.Equal(t, "lazy_tool", def.Name())
	assert.Equal(t, int32(1), loader.count())

	// A second Get must NOT re-invoke the loader.
	_, err = adapter.Get(context.Background(), "lazy_tool")
	require.NoError(t, err)
	assert.Equal(t, int32(1), loader.count())
}

func TestAdapter_Get_UnknownReturnsErrToolNotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	adapter := NewDeferredToolRegistryAdapter(NewDefaultToolRegistry())

	_, err := adapter.Get(context.Background(), "does_not_exist")
	assert.ErrorIs(t, err, ErrToolNotFound)
}

func TestAdapter_List_IncludesStubs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	adapter := NewDeferredToolRegistryAdapter(NewDefaultToolRegistry())

	// Register one eager tool.
	eager := &fakeTool{name: "eager", description: "eager"}
	require.NoError(t, adapter.Register(context.Background(), eager))

	// Register two deferred tools (not yet loaded).
	loader1 := &recorderLoader{name: "deferred_one", payload: &fakeTool{name: "deferred_one", description: "real"}}
	require.NoError(t, adapter.RegisterDeferred(context.Background(), "deferred_one", loader1.fn()))
	loader2 := &recorderLoader{name: "deferred_two", payload: &fakeTool{name: "deferred_two", description: "real"}}
	require.NoError(t, adapter.RegisterDeferred(context.Background(), "deferred_two", loader2.fn()))

	list, err := adapter.List(context.Background())
	require.NoError(t, err)
	// 1 eager + 2 stubs.
	require.Len(t, list, 3)

	names := make(map[string]bool, len(list))
	for _, d := range list {
		names[d.Name()] = true
	}
	assert.True(t, names["eager"])
	assert.True(t, names["deferred_one"])
	assert.True(t, names["deferred_two"])

	// Loaders have not been invoked by List.
	assert.Equal(t, int32(0), loader1.count())
	assert.Equal(t, int32(0), loader2.count())

	// After loading one deferred tool, List still has 3 entries but one
	// is now the real tool instead of a stub.
	_, err = adapter.Get(context.Background(), "deferred_one")
	require.NoError(t, err)

	list2, err := adapter.List(context.Background())
	require.NoError(t, err)
	require.Len(t, list2, 3)

	var foundReal bool
	for _, d := range list2 {
		if d.Name() == "deferred_one" {
			// The real tool's description is "fake tool deferred_one: real",
			// not the stub description.
			assert.Contains(t, d.Description(), "fake tool")
			foundReal = true
		}
	}
	assert.True(t, foundReal)
}

func TestAdapter_Concurrent(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	adapter := NewDeferredToolRegistryAdapter(NewDefaultToolRegistry())

	// Pre-register one eager tool.
	require.NoError(t, adapter.Register(context.Background(), &fakeTool{name: "eager", description: "eager"}))

	// Register several deferred tools.
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("lazy_%d", i) //nolint:govet // test helper
		loader := &recorderLoader{name: name, payload: &fakeTool{name: name, description: "real"}}
		require.NoError(t, adapter.RegisterDeferred(context.Background(), name, loader.fn()))
	}

	const n = 64
	var wg sync.WaitGroup
	wg.Add(n * 3)

	// Concurrent Get on deferred tools.
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			_, _ = adapter.Get(context.Background(), fmt.Sprintf("lazy_%d", idx%5))
		}(i)
	}

	// Concurrent Register (eager).
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			_ = adapter.Register(context.Background(), &fakeTool{name: fmt.Sprintf("concurrent_eager_%d", idx), description: "concurrent"})
		}(i)
	}

	// Concurrent List.
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = adapter.List(context.Background())
		}()
	}

	wg.Wait()

	// Verify the registry is still functional after concurrent access.
	list, err := adapter.List(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, list)
}
