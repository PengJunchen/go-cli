package extension

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- local stubs (avoid importing internal/mock which creates a cycle) ---

// stubExt records Init/Shutdown calls for lifecycle assertions.
type stubExt struct {
	mu          sync.Mutex
	name        string
	initCalled  bool
	shutdownCnt int
	initErr     error
	shutdownErr error
	registry    ExtensionRegistry
}

var _ Extension = (*stubExt)(nil)

func newStubExt(name string) *stubExt { return &stubExt{name: name} }

func (e *stubExt) Name() string { return e.name }

func (e *stubExt) SetInitError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initErr = err
}

func (e *stubExt) SetShutdownError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdownErr = err
}

func (e *stubExt) Init(_ context.Context, reg ExtensionRegistry) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.initCalled = true
	e.registry = reg
	return e.initErr
}

func (e *stubExt) Shutdown(_ context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.shutdownCnt++
	return e.shutdownErr
}

func (e *stubExt) InitCalled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.initCalled
}

func (e *stubExt) ShutdownCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.shutdownCnt
}

func (e *stubExt) Registry() ExtensionRegistry {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.registry
}

// stubHook records Handle calls.
type stubHook struct {
	mu     sync.Mutex
	name   string
	calls  int
	result HookResult
}

var _ Hook = (*stubHook)(nil)

func newStubHook(name string) *stubHook {
	return &stubHook{name: name, result: HookResult{Action: HookActionPass}}
}

func (h *stubHook) Name() string { return h.name }

func (h *stubHook) SetResult(r HookResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.result = r
}

func (h *stubHook) Handle(_ context.Context, _ HookEvent) HookResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.result
}

func (h *stubHook) CallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// --- tests ---

// TestCoordinatorNilRegistryDefault verifies a nil registry argument yields a
// usable fresh registry and that registry() reports it.
func TestCoordinatorNilRegistryDefault(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	require.NotNil(t, coord.registry())
	ctx := context.Background()
	ext := newStubExt("def")
	require.NoError(t, coord.initExtension(ctx, ext))
	assert.Same(t, coord.registry(), ext.Registry())
}

// TestCoordinatorStateForUnknown verifies state returns Pending for extensions
// that were never initialized.
func TestCoordinatorStateForUnknown(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	assert.Equal(t, extensionStatePending, coord.state("never-initialized"))
}

// TestCoordinatorInitFailureNotMarkedRunning verifies an Init error surfaces and
// the extension is not recorded as Running.
func TestCoordinatorInitFailureNotMarkedRunning(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	ctx := context.Background()
	ext := newStubExt("bad")
	ext.SetInitError(errors.New("init error"))
	err := coord.initExtension(ctx, ext)
	require.Error(t, err)
	assert.Equal(t, extensionStatePending, coord.state("bad"))
}

// TestCoordinatorShutdownUnknown no-ops gracefully for extensions that were
// never initialized (there is nothing to transition).
func TestCoordinatorShutdownUnknown(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	ext := newStubExt("ghost")
	require.NoError(t, coord.shutdownExtension(context.Background(), ext))
	assert.Equal(t, extensionStatePending, coord.state("ghost"))
}

// TestCoordinatorReinitSameName verifies re-initializing an extension with the
// same name overwrites the tracked entry and returns to Running.
func TestCoordinatorReinitSameName(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	ctx := context.Background()
	e1 := newStubExt("s")
	require.NoError(t, coord.initExtension(ctx, e1))
	require.NoError(t, coord.shutdownExtension(ctx, e1))
	assert.Equal(t, extensionStateStopped, coord.state("s"))

	e2 := newStubExt("s")
	require.NoError(t, coord.initExtension(ctx, e2))
	assert.Equal(t, extensionStateRunning, coord.state("s"))
	require.NoError(t, coord.shutdownExtension(ctx, e2))
	assert.Equal(t, extensionStateStopped, coord.state("s"))
}

// TestCoordinatorRunHookPassThrough verifies runHook relays a pass result with
// an empty reason and no replacement.
func TestCoordinatorRunHookPassThrough(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	hook := newStubHook("p")
	res := coord.runHook(context.Background(), hook, HookEvent{Name: "e"})
	assert.Equal(t, HookActionPass, res.Action)
	assert.Equal(t, 1, hook.CallCount())
}

// TestCoordinatorRunHookTerminate verifies runHook relays a terminate action.
func TestCoordinatorRunHookTerminate(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	hook := newStubHook("term")
	hook.SetResult(HookResult{Action: hookActionTerminate, Reason: "stop all"})
	res := coord.runHook(context.Background(), hook, HookEvent{Name: "e"})
	assert.Equal(t, hookActionTerminate, res.Action)
	assert.Equal(t, "stop all", res.Reason)
}

// TestCoordinatorConcurrentLifecycle verifies concurrent Init/Shutdown/State
// calls across distinct extensions are safe under the -race detector.
func TestCoordinatorConcurrentLifecycle(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('e' + i))
			ext := newStubExt(name)
			require.NoError(t, coord.initExtension(ctx, ext))
			assert.Equal(t, extensionStateRunning, coord.state(name))
			require.NoError(t, coord.shutdownExtension(ctx, ext))
			assert.Equal(t, extensionStateStopped, coord.state(name))
		}(i)
	}
	wg.Wait()
}

// TestCoordinatorStateTransitionsFull verifies the full pending->running->stopped
// transition table for a single extension.
func TestCoordinatorStateTransitionsFull(t *testing.T) {
	coord := newExtensionCoordinator(nil)
	ctx := context.Background()
	ext := newStubExt("cycle")
	require.Equal(t, extensionStatePending, coord.state("cycle"))
	require.NoError(t, coord.initExtension(ctx, ext))
	require.Equal(t, extensionStateRunning, coord.state("cycle"))
	require.NoError(t, coord.shutdownExtension(ctx, ext))
	require.Equal(t, extensionStateStopped, coord.state("cycle"))
	assert.Equal(t, 1, ext.ShutdownCount())
}
