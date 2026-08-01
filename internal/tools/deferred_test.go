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

	require.Eventually(t, func() bool { return e.hasSpan("tools.deferred.load") }, 2*time.Second, 10*time.Millisecond)
}
