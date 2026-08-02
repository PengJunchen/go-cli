package extension_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// TestRegistryMissingGetters verifies getters return nil/zero for unknown keys
// without panicking.
func TestRegistryMissingGetters(t *testing.T) {
	reg := extension.NewExtensionRegistry()
	assert.Nil(t, reg.Tool("nope"))
	assert.Nil(t, reg.Command("nope"))
	assert.Nil(t, reg.Provider("nope"))
	assert.Nil(t, reg.Hook("nope"))
	assert.Nil(t, reg.Middleware("nope"))
}

// TestRegistryHookAndProviderOverwrite verifies last-writer-wins for hooks and
// providers.
func TestRegistryHookAndProviderOverwrite(t *testing.T) {
	ctx := context.Background()
	reg := extension.NewExtensionRegistry()

	h1 := mock.NewMockHook("h")
	h2 := mock.NewMockHook("h")
	require.NoError(t, reg.RegisterHook(ctx, h1))
	require.NoError(t, reg.RegisterHook(ctx, h2))
	assert.Same(t, h2, reg.Hook("h"), "second hook should overwrite the first")

	require.NoError(t, reg.RegisterProvider(testProvider{name: "p"}))
	require.NoError(t, reg.RegisterProvider(testProvider{name: "p"}))
	assert.Equal(t, "p", reg.Provider("p").Name())
}

// TestRegistryMiddlewareOverwrite verifies last-writer-wins for middleware.
func TestRegistryMiddlewareOverwrite(t *testing.T) {
	ctx := context.Background()
	reg := extension.NewExtensionRegistry()
	m1 := mock.NewMockMiddleware("m")
	m2 := mock.NewMockMiddleware("m")
	require.NoError(t, reg.RegisterMiddleware(ctx, m1))
	require.NoError(t, reg.RegisterMiddleware(ctx, m2))
	assert.Same(t, m2, reg.Middleware("m"))
}

// TestRegistryCommandOverwrite verifies the last registered command runs.
func TestRegistryCommandOverwrite(t *testing.T) {
	reg := extension.NewExtensionRegistry()
	first, second := false, false
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { first = true; return nil }))
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { second = true; return nil }))
	require.NoError(t, reg.Command("run")(nil))
	assert.False(t, first)
	assert.True(t, second)
}

// TestRegistryConcurrentConcurrentRW exercises concurrent registration and
// reads across all five building-block types under the -race detector.
func TestRegistryConcurrentConcurrentRW(t *testing.T) {
	reg := extension.NewExtensionRegistry()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a' + i%26))
			_ = reg.RegisterTool(ctx, testTool{name: name})                         //nolint:errcheck // registration returns nil error
			_ = reg.RegisterProvider(testProvider{name: name})                      //nolint:errcheck // registration returns nil error
			_ = reg.RegisterHook(ctx, mock.NewMockHook(name))                       //nolint:errcheck // registration returns nil error
			_ = reg.RegisterMiddleware(ctx, mock.NewMockMiddleware(name))           //nolint:errcheck // registration returns nil error
			_ = reg.RegisterCommand(name, func(args []string) error { return nil }) //nolint:errcheck // registration returns nil error
			_ = reg.Tool(name)
			_ = reg.Provider(name)
			_ = reg.Hook(name)
			_ = reg.Middleware(name)
			_ = reg.Command(name)
		}(i)
	}
	wg.Wait()
}

// TestRegistryEmptyFresh asserts a freshly constructed registry starts empty.
func TestRegistryEmptyFresh(t *testing.T) {
	reg := extension.NewExtensionRegistry()
	assert.Nil(t, reg.Tool("x"))
	assert.Nil(t, reg.Command("x"))
	assert.Nil(t, reg.Provider("x"))
	assert.Nil(t, reg.Hook("x"))
	assert.Nil(t, reg.Middleware("x"))
}

// TestDefaultExtensionRegistryImplementsInterface asserts the default registry
// satisfies the ExtensionRegistry contract.
func TestDefaultExtensionRegistryImplementsInterface(t *testing.T) {
	var _ extension.ExtensionRegistry = extension.NewExtensionRegistry()
	assert.NotNil(t, extension.NewExtensionRegistry())
}
