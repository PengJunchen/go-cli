package mock

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// ---------------------------------------------------------------------------
// MockExtension
// ---------------------------------------------------------------------------

func TestNewMockExtensionReturnsNonNil(t *testing.T) {
	e := NewMockExtension("test-ext")
	require.NotNil(t, e)
	assert.Equal(t, "test-ext", e.Name())
}

func TestMockExtensionInitRecordsCallAndRegistry(t *testing.T) {
	e := NewMockExtension("ext")
	reg := extension.NewExtensionRegistry()

	assert.False(t, e.InitCalled(), "Init should not have been called yet")

	err := e.Init(context.Background(), reg)
	require.NoError(t, err)

	assert.True(t, e.InitCalled(), "Init should have been called")
	assert.Equal(t, reg, e.Registry(), "Registry should match what was passed to Init")
}

func TestMockExtensionInitErrorInjection(t *testing.T) {
	e := NewMockExtension("ext")
	wantErr := errors.New("init failed")
	e.SetInitError(wantErr)

	err := e.Init(context.Background(), extension.NewExtensionRegistry())
	assert.ErrorIs(t, err, wantErr, "Init should return the injected error")
}

func TestMockExtensionShutdownRecordsCount(t *testing.T) {
	e := NewMockExtension("ext")

	assert.Equal(t, 0, e.ShutdownCount(), "initial shutdown count should be 0")

	require.NoError(t, e.Shutdown(context.Background()))
	assert.Equal(t, 1, e.ShutdownCount())

	require.NoError(t, e.Shutdown(context.Background()))
	assert.Equal(t, 2, e.ShutdownCount())
}

func TestMockExtensionShutdownErrorInjection(t *testing.T) {
	e := NewMockExtension("ext")
	wantErr := errors.New("shutdown failed")
	e.SetShutdownError(wantErr)

	err := e.Shutdown(context.Background())
	assert.ErrorIs(t, err, wantErr, "Shutdown should return the injected error")
}

func TestMockExtensionConcurrentAccess(t *testing.T) {
	e := NewMockExtension("ext-concurrent")
	reg := extension.NewExtensionRegistry()

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines * 3)

	// Concurrent Init calls
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			e.Init(context.Background(), reg) //nolint:errcheck,gosec // concurrent test, error not needed
		}()
	}
	// Concurrent Shutdown calls
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			e.Shutdown(context.Background()) //nolint:errcheck,gosec // concurrent test, error not needed
		}()
	}
	// Concurrent reads
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = e.InitCalled()
			_ = e.ShutdownCount()
			_ = e.Registry()
		}()
	}

	wg.Wait()

	assert.True(t, e.InitCalled())
	assert.Equal(t, goroutines, e.ShutdownCount())
}

// ---------------------------------------------------------------------------
// MockHook
// ---------------------------------------------------------------------------

func TestNewMockHookReturnsNonNil(t *testing.T) {
	h := NewMockHook("hook-test")
	require.NotNil(t, h)
	assert.Equal(t, "hook-test", h.Name())
}

func TestMockHookDefaultResultIsPass(t *testing.T) {
	h := NewMockHook("hook-pass")
	result := h.Handle(context.Background(), extension.HookEvent{Name: "test"})
	assert.Equal(t, extension.HookActionPass, result.Action, "default action should be HookActionPass")
}

func TestMockHookSetResult(t *testing.T) {
	h := NewMockHook("hook-custom")
	h.SetResult(extension.HookResult{Action: extension.HookAction("block"), Reason: "denied"})

	result := h.Handle(context.Background(), extension.HookEvent{Name: "test"})
	assert.Equal(t, extension.HookAction("block"), result.Action)
	assert.Equal(t, "denied", result.Reason)
}

func TestMockHookCallCount(t *testing.T) {
	h := NewMockHook("hook-count")
	assert.Equal(t, 0, h.CallCount())

	_ = h.Handle(context.Background(), extension.HookEvent{Name: "a"})
	assert.Equal(t, 1, h.CallCount())

	_ = h.Handle(context.Background(), extension.HookEvent{Name: "b"})
	_ = h.Handle(context.Background(), extension.HookEvent{Name: "c"})
	assert.Equal(t, 3, h.CallCount())
}

func TestMockHookConcurrentAccess(t *testing.T) {
	h := NewMockHook("hook-concurrent")
	h.SetResult(extension.HookResult{Action: extension.HookActionPass})

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = h.Handle(context.Background(), extension.HookEvent{Name: "evt"})
		}()
	}

	wg.Wait()
	assert.Equal(t, goroutines, h.CallCount())
}

// ---------------------------------------------------------------------------
// MockPluginLoader
// ---------------------------------------------------------------------------

func TestNewMockPluginLoaderReturnsNonNil(t *testing.T) {
	l := NewMockPluginLoader("loader-test")
	require.NotNil(t, l)
	assert.Equal(t, "loader-test", l.Name())
}

func TestMockPluginLoaderSetResultAndLoad(t *testing.T) {
	l := NewMockPluginLoader("loader")
	ext1 := NewMockExtension("ext1")
	ext2 := NewMockExtension("ext2")
	l.SetResult("/plugins/foo.so", []extension.Extension{ext1, ext2})

	exts, err := l.Load(context.Background(), "/plugins/foo.so")
	require.NoError(t, err)
	require.Len(t, exts, 2)
	assert.Equal(t, "ext1", exts[0].Name())
	assert.Equal(t, "ext2", exts[1].Name())
	assert.Equal(t, "/plugins/foo.so", l.LoadedPath())
}

func TestMockPluginLoaderLoadUnknownPathReturnsNil(t *testing.T) {
	l := NewMockPluginLoader("loader")
	exts, err := l.Load(context.Background(), "/nonexistent")
	require.NoError(t, err)
	assert.Nil(t, exts, "unknown path should return nil extensions")
	assert.Equal(t, "/nonexistent", l.LoadedPath())
}

func TestMockPluginLoaderSetError(t *testing.T) {
	l := NewMockPluginLoader("loader")
	wantErr := errors.New("load failed")
	l.SetError("/bad/path", wantErr)

	exts, err := l.Load(context.Background(), "/bad/path")
	assert.Nil(t, exts)
	assert.ErrorIs(t, err, wantErr)
}

func TestMockPluginLoaderErrorTakesPrecedenceOverResult(t *testing.T) {
	l := NewMockPluginLoader("loader")
	ext := NewMockExtension("ext")
	l.SetResult("/conflict", []extension.Extension{ext})
	l.SetError("/conflict", errors.New("err"))

	exts, err := l.Load(context.Background(), "/conflict")
	assert.Nil(t, exts, "error should take precedence over result")
	assert.Error(t, err)
}

func TestMockPluginLoaderLoadedPathTracksLastCall(t *testing.T) {
	l := NewMockPluginLoader("loader")

	l.Load(context.Background(), "/first") //nolint:errcheck,gosec // test
	assert.Equal(t, "/first", l.LoadedPath())

	l.Load(context.Background(), "/second") //nolint:errcheck,gosec // test
	assert.Equal(t, "/second", l.LoadedPath(), "LoadedPath should reflect the most recent call")
}

func TestMockPluginLoaderConcurrentAccess(t *testing.T) {
	l := NewMockPluginLoader("loader-concurrent")
	l.SetResult("/a", []extension.Extension{NewMockExtension("a")})
	l.SetError("/b", errors.New("fail"))

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				l.Load(context.Background(), "/a") //nolint:errcheck,gosec // concurrent test
			} else {
				l.Load(context.Background(), "/b") //nolint:errcheck,gosec // concurrent test
			}
		}(i)
	}

	wg.Wait()
	// No assertion on LoadedPath since it's non-deterministic, but the test
	// passes if no race is detected.
}

// ---------------------------------------------------------------------------
// MockMiddleware
// ---------------------------------------------------------------------------

func TestNewMockMiddlewareReturnsNonNil(t *testing.T) {
	m := NewMockMiddleware("mw")
	require.NotNil(t, m)
	assert.Equal(t, "mw", m.Name())
}

func TestMockMiddlewareWrapAgentDelegates(t *testing.T) {
	m := NewMockMiddleware("mw")
	called := false
	next := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		called = true
		return extension.AgentOutput{Text: "ok"}, nil
	}

	wrapped := m.WrapAgent(next)
	assert.Equal(t, 1, m.WrapCount())

	out, err := wrapped(context.Background(), extension.AgentInput{Message: "hi"})
	require.NoError(t, err)
	assert.True(t, called, "wrapped function should call next")
	assert.Equal(t, "ok", out.Text)
}

func TestMockMiddlewareWrapCountIncrements(t *testing.T) {
	m := NewMockMiddleware("mw")
	dummy := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{}, nil
	}

	assert.Equal(t, 0, m.WrapCount())
	m.WrapAgent(dummy)
	assert.Equal(t, 1, m.WrapCount())
	m.WrapAgent(dummy)
	m.WrapAgent(dummy)
	assert.Equal(t, 3, m.WrapCount())
}

func TestMockMiddlewareConcurrentWrap(t *testing.T) {
	m := NewMockMiddleware("mw-concurrent")
	dummy := func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{}, nil
	}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			m.WrapAgent(dummy)
		}()
	}

	wg.Wait()
	assert.Equal(t, goroutines, m.WrapCount())
}

// ---------------------------------------------------------------------------
// MockModelMiddleware
// ---------------------------------------------------------------------------

func TestNewMockModelMiddlewareReturnsNonNil(t *testing.T) {
	m := NewMockModelMiddleware("mmw")
	require.NotNil(t, m)
	assert.Equal(t, "mmw", m.Name())
}

func TestMockModelMiddlewareWrapModelDelegates(t *testing.T) {
	m := NewMockModelMiddleware("mmw")
	called := false
	next := func(ctx context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		called = true
		return extension.ModelResponse{Text: "model-ok"}, nil
	}

	wrapped := m.WrapModel(next)
	assert.Equal(t, 1, m.WrapCount())

	out, err := wrapped(context.Background(), extension.ModelRequest{Prompt: "hi"})
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, "model-ok", out.Text)
}

func TestMockModelMiddlewareConcurrentWrap(t *testing.T) {
	m := NewMockModelMiddleware("mmw-concurrent")
	dummy := func(ctx context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{}, nil
	}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			m.WrapModel(dummy)
		}()
	}

	wg.Wait()
	assert.Equal(t, goroutines, m.WrapCount())
}

// ---------------------------------------------------------------------------
// MockToolMiddleware
// ---------------------------------------------------------------------------

func TestNewMockToolMiddlewareReturnsNonNil(t *testing.T) {
	m := NewMockToolMiddleware("tmw")
	require.NotNil(t, m)
	assert.Equal(t, "tmw", m.Name())
}

func TestMockToolMiddlewareWrapToolDelegates(t *testing.T) {
	m := NewMockToolMiddleware("tmw")
	called := false
	next := func(ctx context.Context, name string, input any) (any, error) {
		called = true
		return "tool-ok", nil
	}

	wrapped := m.WrapTool(next)
	assert.Equal(t, 1, m.WrapCount())

	out, err := wrapped(context.Background(), "tool1", nil)
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, "tool-ok", out)
}

func TestMockToolMiddlewareConcurrentWrap(t *testing.T) {
	m := NewMockToolMiddleware("tmw-concurrent")
	dummy := func(ctx context.Context, name string, input any) (any, error) {
		return nil, nil
	}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			m.WrapTool(dummy)
		}()
	}

	wg.Wait()
	assert.Equal(t, goroutines, m.WrapCount())
}
