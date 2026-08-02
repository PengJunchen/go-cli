package extension

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- local stubs (avoid importing internal/mock which creates a cycle) ---

// regTestHook records Handle calls.
type regTestHook struct {
	mu     sync.Mutex
	name   string
	calls  int
	result HookResult
}

var _ Hook = (*regTestHook)(nil)

func newRegTestHook(name string) *regTestHook {
	return &regTestHook{name: name, result: HookResult{Action: HookActionPass}}
}

func (h *regTestHook) Name() string { return h.name }

func (h *regTestHook) Handle(_ context.Context, _ HookEvent) HookResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.result
}

func (h *regTestHook) CallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// regTestMiddleware wraps an AgentFunc, recording wrap counts.
type regTestMiddleware struct {
	mu        sync.Mutex
	name      string
	wrapCount int
}

var _ Middleware = (*regTestMiddleware)(nil)

func newRegTestMiddleware(name string) *regTestMiddleware {
	return &regTestMiddleware{name: name}
}

func (m *regTestMiddleware) Name() string { return m.name }

func (m *regTestMiddleware) WrapAgent(next AgentFunc) AgentFunc {
	m.mu.Lock()
	m.wrapCount++
	m.mu.Unlock()
	return func(ctx context.Context, input AgentInput) (AgentOutput, error) {
		return next(ctx, input)
	}
}

func (m *regTestMiddleware) WrapCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wrapCount
}

// newConcreteRegistry returns the concrete *DefaultExtensionRegistry for tests
// that need access to unexported helper methods (tool, command, provider, etc.)
// not exposed on the ExtensionRegistry interface.
func newConcreteRegistry() *DefaultExtensionRegistry {
	reg, ok := NewExtensionRegistry().(*DefaultExtensionRegistry)
	if !ok {
		panic("NewExtensionRegistry() should return *DefaultExtensionRegistry")
	}
	return reg
}

// --- tests ---

// TestRegistryMissingGetters verifies getters return nil/zero for unknown keys
// without panicking.
func TestRegistryMissingGetters(t *testing.T) {
	reg := newConcreteRegistry()
	assert.Nil(t, reg.tool("nope"))
	assert.Nil(t, reg.command("nope"))
	assert.Nil(t, reg.provider("nope"))
	assert.Nil(t, reg.Hook("nope"))
	assert.Nil(t, reg.Middleware("nope"))
}

// TestRegistryHookAndProviderOverwrite verifies last-writer-wins for hooks and
// providers.
func TestRegistryHookAndProviderOverwrite(t *testing.T) {
	ctx := context.Background()
	reg := newConcreteRegistry()

	h1 := newRegTestHook("h")
	h2 := newRegTestHook("h")
	require.NoError(t, reg.RegisterHook(ctx, h1))
	require.NoError(t, reg.RegisterHook(ctx, h2))
	assert.Same(t, h2, reg.Hook("h"), "second hook should overwrite the first")

	require.NoError(t, reg.RegisterProvider(testProvider{name: "p"}))
	require.NoError(t, reg.RegisterProvider(testProvider{name: "p"}))
	assert.Equal(t, "p", reg.provider("p").Name())
}

// TestRegistryMiddlewareOverwrite verifies last-writer-wins for middleware.
func TestRegistryMiddlewareOverwrite(t *testing.T) {
	ctx := context.Background()
	reg := newConcreteRegistry()
	m1 := newRegTestMiddleware("m")
	m2 := newRegTestMiddleware("m")
	require.NoError(t, reg.RegisterMiddleware(ctx, m1))
	require.NoError(t, reg.RegisterMiddleware(ctx, m2))
	assert.Same(t, m2, reg.Middleware("m"))
}

// TestRegistryCommandOverwrite verifies the last registered command runs.
func TestRegistryCommandOverwrite(t *testing.T) {
	reg := newConcreteRegistry()
	first, second := false, false
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { first = true; return nil }))
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { second = true; return nil }))
	require.NoError(t, reg.command("run")(nil))
	assert.False(t, first)
	assert.True(t, second)
}

// TestRegistryConcurrentConcurrentRW exercises concurrent registration and
// reads across all five building-block types under the -race detector.
func TestRegistryConcurrentConcurrentRW(t *testing.T) {
	reg := newConcreteRegistry()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a' + i%26))
			_ = reg.RegisterTool(ctx, testTool{name: name})                         //nolint:errcheck // registration returns nil error
			_ = reg.RegisterProvider(testProvider{name: name})                      //nolint:errcheck // registration returns nil error
			_ = reg.RegisterHook(ctx, newRegTestHook(name))                         //nolint:errcheck // registration returns nil error
			_ = reg.RegisterMiddleware(ctx, newRegTestMiddleware(name))             //nolint:errcheck // registration returns nil error
			_ = reg.RegisterCommand(name, func(args []string) error { return nil }) //nolint:errcheck // registration returns nil error
			_ = reg.tool(name)
			_ = reg.provider(name)
			_ = reg.Hook(name)
			_ = reg.Middleware(name)
			_ = reg.command(name)
		}(i)
	}
	wg.Wait()
}

// TestRegistryEmptyFresh asserts a freshly constructed registry starts empty.
func TestRegistryEmptyFresh(t *testing.T) {
	reg := newConcreteRegistry()
	assert.Nil(t, reg.tool("x"))
	assert.Nil(t, reg.command("x"))
	assert.Nil(t, reg.provider("x"))
	assert.Nil(t, reg.Hook("x"))
	assert.Nil(t, reg.Middleware("x"))
}

// TestDefaultExtensionRegistryImplementsInterface asserts the default registry
// satisfies the ExtensionRegistry contract.
func TestDefaultExtensionRegistryImplementsInterface(t *testing.T) {
	var _ ExtensionRegistry = NewExtensionRegistry()
	assert.NotNil(t, NewExtensionRegistry())
}
