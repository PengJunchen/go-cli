package extension_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// TestCoordinatorNilRegistryDefault verifies a nil registry argument yields a
// usable fresh registry and that Registry() reports it.
func TestCoordinatorNilRegistryDefault(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	require.NotNil(t, coord.Registry())
	ctx := context.Background()
	ext := mock.NewMockExtension("def")
	require.NoError(t, coord.InitExtension(ctx, ext))
	assert.Same(t, coord.Registry(), ext.Registry())
}

// TestCoordinatorStateForUnknown verifies State returns Pending for extensions
// that were never initialized.
func TestCoordinatorStateForUnknown(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	assert.Equal(t, extension.ExtensionStatePending, coord.State("never-initialized"))
}

// TestCoordinatorInitFailureNotMarkedRunning verifies an Init error surfaces and
// the extension is not recorded as Running.
func TestCoordinatorInitFailureNotMarkedRunning(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	ctx := context.Background()
	ext := mock.NewMockExtension("bad")
	ext.SetInitError(errors.New("init error"))
	err := coord.InitExtension(ctx, ext)
	require.Error(t, err)
	assert.Equal(t, extension.ExtensionStatePending, coord.State("bad"))
}

// TestCoordinatorShutdownUnknown no-ops gracefully for extensions that were
// never initialized (there is nothing to transition).
func TestCoordinatorShutdownUnknown(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	ext := mock.NewMockExtension("ghost")
	require.NoError(t, coord.ShutdownExtension(context.Background(), ext))
	assert.Equal(t, extension.ExtensionStatePending, coord.State("ghost"))
}

// TestCoordinatorReinitSameName verifies re-initializing an extension with the
// same name overwrites the tracked entry and returns to Running.
func TestCoordinatorReinitSameName(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	ctx := context.Background()
	e1 := mock.NewMockExtension("s")
	require.NoError(t, coord.InitExtension(ctx, e1))
	require.NoError(t, coord.ShutdownExtension(ctx, e1))
	assert.Equal(t, extension.ExtensionStateStopped, coord.State("s"))

	e2 := mock.NewMockExtension("s")
	require.NoError(t, coord.InitExtension(ctx, e2))
	assert.Equal(t, extension.ExtensionStateRunning, coord.State("s"))
	require.NoError(t, coord.ShutdownExtension(ctx, e2))
	assert.Equal(t, extension.ExtensionStateStopped, coord.State("s"))
}

// TestCoordinatorRunHookPassThrough verifies RunHook relays a pass result with
// an empty reason and no replacement.
func TestCoordinatorRunHookPassThrough(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	hook := mock.NewMockHook("p")
	res := coord.RunHook(context.Background(), hook, extension.HookEvent{Name: "e"})
	assert.Equal(t, extension.HookActionPass, res.Action)
	assert.Equal(t, 1, hook.CallCount())
}

// TestCoordinatorRunHookTerminate verifies RunHook relays a terminate action.
func TestCoordinatorRunHookTerminate(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	hook := mock.NewMockHook("term")
	hook.SetResult(extension.HookResult{Action: extension.HookActionTerminate, Reason: "stop all"})
	res := coord.RunHook(context.Background(), hook, extension.HookEvent{Name: "e"})
	assert.Equal(t, extension.HookActionTerminate, res.Action)
	assert.Equal(t, "stop all", res.Reason)
}

// TestCoordinatorConcurrentLifecycle verifies concurrent Init/Shutdown/State
// calls across distinct extensions are safe under the -race detector.
func TestCoordinatorConcurrentLifecycle(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('e' + i))
			ext := mock.NewMockExtension(name)
			require.NoError(t, coord.InitExtension(ctx, ext))
			assert.Equal(t, extension.ExtensionStateRunning, coord.State(name))
			require.NoError(t, coord.ShutdownExtension(ctx, ext))
			assert.Equal(t, extension.ExtensionStateStopped, coord.State(name))
		}(i)
	}
	wg.Wait()
}

// TestCoordinatorStateTransitionsFull verifies the full pending->running->stopped
// transition table for a single extension.
func TestCoordinatorStateTransitionsFull(t *testing.T) {
	coord := extension.NewExtensionCoordinator(nil)
	ctx := context.Background()
	ext := mock.NewMockExtension("cycle")
	require.Equal(t, extension.ExtensionStatePending, coord.State("cycle"))
	require.NoError(t, coord.InitExtension(ctx, ext))
	require.Equal(t, extension.ExtensionStateRunning, coord.State("cycle"))
	require.NoError(t, coord.ShutdownExtension(ctx, ext))
	require.Equal(t, extension.ExtensionStateStopped, coord.State("cycle"))
	assert.Equal(t, 1, ext.ShutdownCount())
}
