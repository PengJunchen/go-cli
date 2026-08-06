package extension

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registeringExt is a test Extension that registers a Hook and/or a Middleware
// into the registry during Init, so lifecycle retrieval can be exercised.
type registeringExt struct {
	name string
	hook Hook
	mw   Middleware
}

var _ Extension = (*registeringExt)(nil)

func (e *registeringExt) Name() string { return e.name }

func (e *registeringExt) Init(ctx context.Context, reg ExtensionRegistry) error {
	if e.hook != nil {
		if err := reg.RegisterHook(ctx, e.hook); err != nil {
			return err
		}
	}
	if e.mw != nil {
		if err := reg.RegisterMiddleware(ctx, e.mw); err != nil {
			return err
		}
	}
	return nil
}

func (e *registeringExt) Shutdown(context.Context) error { return nil }

// Hooks returns the hooks registered by extensions after Init.
func TestPluginManagerHooksReturnsRegistered(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	hook := newMgrTestHook("ext-hook")
	ext := &registeringExt{name: "reg-ext", hook: hook}
	loader.SetResult("p", []Extension{ext})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"p"}))

	// Before Init: no hooks are available.
	assert.Empty(t, pm.Hooks())

	require.NoError(t, pm.Init(context.Background()))

	hooks := pm.Hooks()
	require.Len(t, hooks, 1)
	assert.Equal(t, "ext-hook", hooks[0].Name())
}

// Middleware returns the middleware registered by extensions after Init.
func TestPluginManagerMiddlewareReturnsRegistered(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	mw := &defaultMW{name: "ext-mw"}
	ext := &registeringExt{name: "reg-ext", mw: mw}
	loader.SetResult("p", []Extension{ext})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"p"}))

	// Before Init: no middleware is available.
	assert.Empty(t, pm.Middleware())

	require.NoError(t, pm.Init(context.Background()))

	mws := pm.Middleware()
	require.Len(t, mws, 1)
	assert.Equal(t, "ext-mw", mws[0].Name())
}

// Hooks and Middleware are empty when no extension registers any.
func TestPluginManagerHooksMiddlewareEmptyWhenNone(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	ext := newMgrTestExt("plain")
	loader.SetResult("p", []Extension{ext})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"p"}))
	require.NoError(t, pm.Init(context.Background()))

	assert.Empty(t, pm.Hooks())
	assert.Empty(t, pm.Middleware())
}

// Hooks and Middleware collect registrations across multiple extensions.
func TestPluginManagerHooksMultipleExtensions(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	ext1 := &registeringExt{name: "ext-1", hook: newMgrTestHook("hook-1")}
	ext2 := &registeringExt{name: "ext-2", hook: newMgrTestHook("hook-2")}
	loader.SetResult("p", []Extension{ext1, ext2})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"p"}))
	require.NoError(t, pm.Init(context.Background()))

	hooks := pm.Hooks()
	require.Len(t, hooks, 2)

	names := make(map[string]bool, len(hooks))
	for _, h := range hooks {
		names[h.Name()] = true
	}
	assert.True(t, names["hook-1"])
	assert.True(t, names["hook-2"])
}

// DefaultExtensionRegistry.AllHooks returns every registered hook.
func TestRegistryAllHooks(t *testing.T) {
	reg := NewExtensionRegistry()
	der := reg.(*DefaultExtensionRegistry) //nolint:errcheck
	ctx := context.Background()

	require.NoError(t, der.RegisterHook(ctx, newMgrTestHook("h1")))
	require.NoError(t, der.RegisterHook(ctx, newMgrTestHook("h2")))

	hooks := der.AllHooks()
	assert.Len(t, hooks, 2)
}

// DefaultExtensionRegistry.AllMiddleware returns every registered middleware.
func TestRegistryAllMiddleware(t *testing.T) {
	reg := NewExtensionRegistry()
	der := reg.(*DefaultExtensionRegistry) //nolint:errcheck
	ctx := context.Background()

	require.NoError(t, der.RegisterMiddleware(ctx, &defaultMW{name: "m1"}))
	require.NoError(t, der.RegisterMiddleware(ctx, &defaultMW{name: "m2"}))

	mws := der.AllMiddleware()
	assert.Len(t, mws, 2)
}
