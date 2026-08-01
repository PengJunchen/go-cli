package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// testTool is a minimal tools.ToolDefinition used in extension tests.
type testTool struct{ name string }

func (t testTool) Name() string        { return t.name }
func (t testTool) Description() string { return "a test tool" }
func (t testTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "ok"}, nil
}

var _ tools.ToolDefinition = (*testTool)(nil)

func TestExtensionRegistryRegisterAndGet(t *testing.T) {
	ctx := context.Background()
	reg := NewExtensionRegistry()

	require.NoError(t, reg.RegisterTool(ctx, testTool{name: "read"}))

	got := reg.Tool("read")
	require.NotNil(t, got)
	assert.Equal(t, "read", got.Name())
	assert.Nil(t, reg.Tool("unknown"))

	called := false
	fn := func(args []string) error {
		called = true
		return nil
	}
	require.NoError(t, reg.RegisterCommand("greet", fn))
	cmd := reg.Command("greet")
	require.NotNil(t, cmd)
	assert.NoError(t, cmd([]string{"world"}))
	assert.True(t, called)

	require.NoError(t, reg.RegisterProvider(DefaultModelProvider{}))
	assert.NotNil(t, reg.Provider("default"))

	hook := &HookImpl{name: "h1"}
	require.NoError(t, reg.RegisterHook(ctx, hook))
	assert.Equal(t, "h1", reg.Hook("h1").Name())

	require.NoError(t, reg.RegisterMiddleware(ctx, &MiddlewareImpl{name: "m1"}))
	assert.NotNil(t, reg.Middleware("m1"))
}

func TestExtensionRegistryDuplicateOverwrites(t *testing.T) {
	ctx := context.Background()
	reg := NewExtensionRegistry()

	require.NoError(t, reg.RegisterTool(ctx, testTool{name: "read"}))
	require.NoError(t, reg.RegisterTool(ctx, testTool{name: "read"}))
	assert.Equal(t, "read", reg.Tool("read").Name())

	// Re-registering a command with the same name replaces it.
	calledA, calledB := false, false
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { calledA = true; return nil }))
	require.NoError(t, reg.RegisterCommand("run", func([]string) error { calledB = true; return nil }))
	require.NoError(t, reg.Command("run")(nil))
	assert.False(t, calledA)
	assert.True(t, calledB)
}

func TestExtensionRegistryCommandErrorPropagates(t *testing.T) {
	reg := NewExtensionRegistry()
	sentinel := errors.New("boom")
	require.NoError(t, reg.RegisterCommand("fail", func([]string) error { return sentinel }))
	assert.ErrorIs(t, reg.Command("fail")(nil), sentinel)
}
