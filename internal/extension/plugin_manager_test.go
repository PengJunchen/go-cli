package extension

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orderTrackingExt records its shutdown sequence into a shared slice.
type orderTrackingExt struct {
	name   string
	order  *[]string
	mu     *sync.Mutex
	initOK bool
}

var _ Extension = (*orderTrackingExt)(nil)

func (e *orderTrackingExt) Name() string { return e.name }

func (e *orderTrackingExt) Init(_ context.Context, _ ExtensionRegistry) error {
	e.initOK = true
	return nil
}

func (e *orderTrackingExt) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	*e.order = append(*e.order, e.name)
	return nil
}

// Load with empty paths is a no-op: no error and no extensions.
func TestPluginManagerLoadEmptyPaths(t *testing.T) {
	pm := NewPluginManager(newMgrTestPluginLoader("test"))
	require.NoError(t, pm.Load(context.Background(), nil))
	assert.Empty(t, pm.Extensions())

	require.NoError(t, pm.Load(context.Background(), []string{}))
	assert.Empty(t, pm.Extensions())
}

// Load collects extensions returned by the loader for each path.
func TestPluginManagerLoadCollectsExtensions(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	ext1 := newMgrTestExt("ext-1")
	ext2 := newMgrTestExt("ext-2")
	loader.SetResult("path-a", []Extension{ext1})
	loader.SetResult("path-b", []Extension{ext2})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"path-a", "path-b"}))

	exts := pm.Extensions()
	require.Len(t, exts, 2)
	assert.Equal(t, "ext-1", exts[0].Name())
	assert.Equal(t, "ext-2", exts[1].Name())
}

// Load with a non-existent path handles the error gracefully: the error is
// not propagated and other paths are still loaded.
func TestPluginManagerLoadHandlesErrorsGracefully(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	loader.SetError("bad-path", errors.New("file not found"))
	goodExt := newMgrTestExt("good")
	loader.SetResult("good-path", []Extension{goodExt})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"bad-path", "good-path"}))

	exts := pm.Extensions()
	require.Len(t, exts, 1, "good-path extensions should still be loaded")
	assert.Equal(t, "good", exts[0].Name())
}

// Init calls Init on every loaded extension.
func TestPluginManagerInit(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	ext1 := newMgrTestExt("ext-1")
	ext2 := newMgrTestExt("ext-2")
	loader.SetResult("p", []Extension{ext1, ext2})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"p"}))
	require.NoError(t, pm.Init(context.Background()))

	assert.True(t, ext1.InitCalled())
	assert.True(t, ext2.InitCalled())
}

// Init passes the internal registry to each extension.
func TestPluginManagerInitPassesRegistry(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	ext := newMgrTestExt("reg-ext")
	loader.SetResult("p", []Extension{ext})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"p"}))
	require.NoError(t, pm.Init(context.Background()))

	require.NotNil(t, ext.Registry())
	assert.Same(t, pm.Registry(), ext.Registry())
}

// Init with no extensions is a no-op.
func TestPluginManagerInitNoExtensions(t *testing.T) {
	pm := NewPluginManager(newMgrTestPluginLoader("test"))
	require.NoError(t, pm.Init(context.Background()))
}

// Shutdown calls Shutdown on every extension in reverse load order.
func TestPluginManagerShutdownReverseOrder(t *testing.T) {
	loader := newMgrTestPluginLoader("test")

	var mu sync.Mutex
	var order []string

	ext1 := &orderTrackingExt{name: "ext-1", order: &order, mu: &mu}
	ext2 := &orderTrackingExt{name: "ext-2", order: &order, mu: &mu}
	ext3 := &orderTrackingExt{name: "ext-3", order: &order, mu: &mu}
	loader.SetResult("p", []Extension{ext1, ext2, ext3})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"p"}))
	require.NoError(t, pm.Init(context.Background()))
	require.NoError(t, pm.Shutdown(context.Background()))

	// All extensions should have been shut down in reverse order.
	require.Len(t, order, 3)
	assert.Equal(t, []string{"ext-3", "ext-2", "ext-1"}, order)
}

// Shutdown with no extensions is a no-op.
func TestPluginManagerShutdownNoExtensions(t *testing.T) {
	pm := NewPluginManager(newMgrTestPluginLoader("test"))
	require.NoError(t, pm.Shutdown(context.Background()))
}

// Shutdown after Load+Init calls Shutdown on every extension.
func TestPluginManagerFullLifecycle(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	ext := newMgrTestExt("lifecycle")
	loader.SetResult("p", []Extension{ext})

	pm := NewPluginManager(loader)

	// Before Load: no extensions.
	assert.Empty(t, pm.Extensions())

	// After Load: one extension.
	require.NoError(t, pm.Load(context.Background(), []string{"p"}))
	require.Len(t, pm.Extensions(), 1)

	// After Init: extension is initialized.
	require.NoError(t, pm.Init(context.Background()))
	assert.True(t, ext.InitCalled())
	assert.Equal(t, 0, ext.ShutdownCount())

	// After Shutdown: extension is shut down.
	require.NoError(t, pm.Shutdown(context.Background()))
	assert.Equal(t, 1, ext.ShutdownCount())
}

// Registry returns a non-nil registry.
func TestPluginManagerRegistry(t *testing.T) {
	pm := NewPluginManager(newMgrTestPluginLoader("test"))
	assert.NotNil(t, pm.Registry())
}

// NewPluginManager with nil loader defaults to DefaultPluginLoader.
func TestPluginManagerNilLoader(t *testing.T) {
	pm := NewPluginManager(nil)
	assert.NotNil(t, pm)
	// Loading an empty path list should be safe.
	require.NoError(t, pm.Load(context.Background(), nil))
}

// Load with a path that returns an error does not add partial extensions.
func TestPluginManagerLoadErrorNoPartialExtensions(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	loader.SetError("err-path", errors.New("load failed"))

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"err-path"}))
	assert.Empty(t, pm.Extensions())
}

// A failing Init for one extension does not prevent others from initializing.
func TestPluginManagerInitPartialFailure(t *testing.T) {
	loader := newMgrTestPluginLoader("test")
	ext1 := newMgrTestExt("fail")
	ext1.SetInitError(errors.New("init failed"))
	ext2 := newMgrTestExt("ok")
	loader.SetResult("p", []Extension{ext1, ext2})

	pm := NewPluginManager(loader)
	require.NoError(t, pm.Load(context.Background(), []string{"p"}))
	require.NoError(t, pm.Init(context.Background()))

	// ext2 should still have been initialized even though ext1 failed.
	assert.True(t, ext2.InitCalled())
}
