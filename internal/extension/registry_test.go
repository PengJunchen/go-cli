package extension

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// --- local stubs (avoid importing internal/mock which creates a cycle) ---

// regTestHook2 records Handle calls.
type regTestHook2 struct {
	mu     sync.Mutex
	name   string
	calls  int
	result HookResult
}

var _ Hook = (*regTestHook2)(nil)

func newRegTestHook2(name string) *regTestHook2 {
	return &regTestHook2{name: name, result: HookResult{Action: HookActionPass}}
}

func (h *regTestHook2) Name() string { return h.name }

func (h *regTestHook2) Handle(_ context.Context, _ HookEvent) HookResult {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls++
	return h.result
}

// regTestMiddleware2 wraps an AgentFunc, recording wrap counts.
type regTestMiddleware2 struct {
	mu        sync.Mutex
	name      string
	wrapCount int
}

var _ Middleware = (*regTestMiddleware2)(nil)

func newRegTestMiddleware2(name string) *regTestMiddleware2 {
	return &regTestMiddleware2{name: name}
}

func (m *regTestMiddleware2) Name() string { return m.name }

func (m *regTestMiddleware2) WrapAgent(next AgentFunc) AgentFunc {
	m.mu.Lock()
	m.wrapCount++
	m.mu.Unlock()
	return func(ctx context.Context, input AgentInput) (AgentOutput, error) {
		return next(ctx, input)
	}
}

// testTool is a minimal tools.ToolDefinition used in registry tests.
type testTool struct{ name string }

func (t testTool) Name() string        { return t.name }
func (t testTool) Description() string { return "a test tool" }
func (t testTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "ok"}, nil
}

var _ tools.ToolDefinition = (*testTool)(nil)

// testProvider is a minimal llm.ModelProvider used in registry tests.
type testProvider struct{ name string }

func (p testProvider) Name() string { return p.name }
func (p testProvider) Build(context.Context, llm.ModelConfig) (llm.BaseChatModel, func(), error) {
	return nil, func() {}, nil
}
func (p testProvider) Models() []llm.ModelInfo { return nil }

var _ llm.ModelProvider = (*testProvider)(nil)

// ExtensionRegistry exposes the five registration methods and the default
// implementation stores by name (last writer wins) with getters.
func TestExtensionRegistryRegisterAndGet(t *testing.T) {
	ctx := context.Background()
	reg := NewExtensionRegistry()

	require.NoError(t, reg.RegisterTool(ctx, testTool{name: "read"}))
	got := reg.tool("read")
	require.NotNil(t, got)
	assert.Equal(t, "read", got.Name())
	assert.Nil(t, reg.tool("unknown"))

	called := false
	fn := func(args []string) error { called = true; return nil }
	require.NoError(t, reg.RegisterCommand("greet", fn))
	assert.NoError(t, reg.command("greet")([]string{"world"}))
	assert.True(t, called)

	require.NoError(t, reg.RegisterProvider(testProvider{name: "vanilla"}))
	assert.NotNil(t, reg.provider("vanilla"))

	hook := newRegTestHook2("h1")
	require.NoError(t, reg.RegisterHook(ctx, hook))
	assert.Equal(t, "h1", reg.Hook("h1").Name())

	mw := newRegTestMiddleware2("m1")
	require.NoError(t, reg.RegisterMiddleware(ctx, mw))
	assert.Equal(t, "m1", reg.Middleware("m1").Name())
}

// duplicate registrations overwrite (last writer wins) and command errors
// propagate.
func TestExtensionRegistryDuplicatesAndErrors(t *testing.T) {
	ctx := context.Background()
	reg := NewExtensionRegistry()

	require.NoError(t, reg.RegisterTool(ctx, testTool{name: "read"}))
	require.NoError(t, reg.RegisterTool(ctx, testTool{name: "read"}))
	assert.Equal(t, "read", reg.tool("read").Name())

	calledA, calledB := false, false
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { calledA = true; return nil }))
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { calledB = true; return nil }))
	require.NoError(t, reg.command("run")(nil))
	assert.False(t, calledA)
	assert.True(t, calledB)

	sentinel := errors.New("boom")
	require.NoError(t, reg.RegisterCommand("fail", func([]string) error { return sentinel }))
	assert.ErrorIs(t, reg.command("fail")(nil), sentinel)
}
