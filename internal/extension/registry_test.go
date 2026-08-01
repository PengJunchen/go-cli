package extension_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

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

// AC-6: ExtensionRegistry exposes the five registration methods and the default
// implementation stores by name (last writer wins) with getters.
func TestExtensionRegistryRegisterAndGet(t *testing.T) {
	ctx := context.Background()
	reg := extension.NewExtensionRegistry()

	require.NoError(t, reg.RegisterTool(ctx, testTool{name: "read"}))
	got := reg.Tool("read")
	require.NotNil(t, got)
	assert.Equal(t, "read", got.Name())
	assert.Nil(t, reg.Tool("unknown"))

	called := false
	fn := func(args []string) error { called = true; return nil }
	require.NoError(t, reg.RegisterCommand("greet", fn))
	assert.NoError(t, reg.Command("greet")([]string{"world"}))
	assert.True(t, called)

	require.NoError(t, reg.RegisterProvider(testProvider{name: "vanilla"}))
	assert.NotNil(t, reg.Provider("vanilla"))

	hook := mock.NewMockHook("h1")
	require.NoError(t, reg.RegisterHook(ctx, hook))
	assert.Equal(t, "h1", reg.Hook("h1").Name())

	mw := mock.NewMockMiddleware("m1")
	require.NoError(t, reg.RegisterMiddleware(ctx, mw))
	assert.Equal(t, "m1", reg.Middleware("m1").Name())
}

// AC-6: duplicate registrations overwrite (last writer wins) and command errors
// propagate.
func TestExtensionRegistryDuplicatesAndErrors(t *testing.T) {
	ctx := context.Background()
	reg := extension.NewExtensionRegistry()

	require.NoError(t, reg.RegisterTool(ctx, testTool{name: "read"}))
	require.NoError(t, reg.RegisterTool(ctx, testTool{name: "read"}))
	assert.Equal(t, "read", reg.Tool("read").Name())

	calledA, calledB := false, false
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { calledA = true; return nil }))
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { calledB = true; return nil }))
	require.NoError(t, reg.Command("run")(nil))
	assert.False(t, calledA)
	assert.True(t, calledB)

	sentinel := errors.New("boom")
	require.NoError(t, reg.RegisterCommand("fail", func([]string) error { return sentinel }))
	assert.ErrorIs(t, reg.Command("fail")(nil), sentinel)
}
