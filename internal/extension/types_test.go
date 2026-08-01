package extension_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
	"github.com/pengjunchen/go-cli/internal/mock"
)

// AC-1: the Extension interface exposes Name/Init/Shutdown, implemented by the
// default stub and recorded by the mock.
func TestExtensionInterface(t *testing.T) {
	ctx := context.Background()
	var def extension.Extension = &extension.DefaultExtension{}

	assert.Equal(t, "default-extension", def.Name())
	require.NoError(t, def.Init(ctx, extension.NewExtensionRegistry()))
	require.NoError(t, def.Shutdown(ctx))

	// The mock satisfies the same contract and records invocations.
	ext := mock.NewMockExtension("recorded")
	reg := extension.NewExtensionRegistry()
	require.NoError(t, ext.Init(ctx, reg))
	assert.True(t, ext.InitCalled())
	require.NoError(t, ext.Shutdown(ctx))
	assert.Equal(t, 1, ext.ShutdownCount())
}

// AC-2/AC-3: the Hook interface (Name/Handle) and HookEvent/HookResult model
// with the four actions.
func TestHookAndHookActions(t *testing.T) {
	ctx := context.Background()

	hook := mock.NewMockHook("h1")
	event := extension.HookEvent{
		Name:      "agent.before_run",
		Data:      "payload",
		Source:    "test-extension",
		Timestamp: time.Now(),
	}
	result := hook.Handle(ctx, event)
	assert.Equal(t, extension.HookActionPass, result.Action)
	assert.Equal(t, 1, hook.CallCount())

	// All four actions are representable.
	actions := []extension.HookAction{
		extension.HookActionPass,
		extension.HookActionBlock,
		extension.HookActionTerminate,
		extension.HookActionReplace,
	}
	expected := []string{"pass", "block", "terminate", "replace"}
	for i, a := range actions {
		assert.Equal(t, expected[i], string(a))
	}

	// Replace carries a substitution value.
	hook.SetResult(extension.HookResult{Action: extension.HookActionReplace, Replacement: "new-payload"})
	res := hook.Handle(ctx, event)
	assert.Equal(t, extension.HookActionReplace, res.Action)
	assert.Equal(t, "new-payload", res.Replacement)

	// Default hook is a pass-through.
	defHook := &extension.DefaultHook{}
	assert.Equal(t, extension.HookActionPass, defHook.Handle(ctx, event).Action)
}

// AC-4: Middleware, ModelMiddleware and ToolMiddleware interfaces and their
// pass-through default implementations actually wrap (unwrap) correctly.
func TestMiddlewareInterfaces(t *testing.T) {
	ctx := context.Background()

	// Agent-level middleware chain.
	mw := mock.NewMockMiddleware("mw")
	var innerCalled bool
	base := func(_ context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		innerCalled = true
		return extension.AgentOutput{Text: "echo:" + input.Message}, nil
	}
	wrapped := mw.WrapAgent(base)
	out, err := wrapped(ctx, extension.AgentInput{Message: "hi"})
	require.NoError(t, err)
	assert.True(t, innerCalled)
	assert.Equal(t, "echo:hi", out.Text)
	assert.Equal(t, 1, mw.WrapCount())

	// Model middleware chain.
	mmw := mock.NewMockModelMiddleware("mmw")
	modelBase := func(_ context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: req.Prompt + "!"}, nil
	}
	mOut, err := mmw.WrapModel(modelBase)(ctx, extension.ModelRequest{Prompt: "p", Model: "m", Temperature: 0.5})
	require.NoError(t, err)
	assert.Equal(t, "p!", mOut.Text)
	assert.Equal(t, 1, mmw.WrapCount())

	// Tool middleware chain.
	tmw := mock.NewMockToolMiddleware("tmw")
	toolBase := func(_ context.Context, name string, input any) (any, error) {
		s, ok := input.(string)
		if !ok {
			s = ""
		}
		return name + ":" + s, nil
	}
	tOut, err := tmw.WrapTool(toolBase)(ctx, "read", "file")
	require.NoError(t, err)
	assert.Equal(t, "read:file", tOut)
	assert.Equal(t, 1, tmw.WrapCount())

	// Default middlewares are pass-through.
	defMw := &extension.DefaultMiddleware{}
	_, outErr := defMw.WrapAgent(func(_ context.Context, i extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{}, nil
	})(ctx, extension.AgentInput{Message: "x"})
	require.NoError(t, outErr)
}
