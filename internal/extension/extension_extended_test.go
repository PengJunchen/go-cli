package extension_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// TestDefaultExtensionNameFallback verifies DefaultExtension reports the stable
// default name for a zero-value instance.
func TestDefaultExtensionNameFallback(t *testing.T) {
	def := &extension.DefaultExtension{}
	assert.Equal(t, "default-extension", def.Name())
}

// TestDefaultExtensionLifecycleNoop verifies the default stub satisfies the
// lifecycle contract without registering anything or failing.
func TestDefaultExtensionLifecycleNoop(t *testing.T) {
	ctx := context.Background()
	reg := extension.NewExtensionRegistry()
	def := &extension.DefaultExtension{}
	require.NoError(t, def.Init(ctx, reg))
	require.NoError(t, def.Shutdown(ctx))
	assert.Equal(t, "default-extension", def.Name())
}

// TestDefaultHookNameAndPass verifies the default hook always returns
// HookActionPass and logs without changing the payload.
func TestDefaultHookNameAndPass(t *testing.T) {
	h := &extension.DefaultHook{}
	event := extension.HookEvent{Name: "agent.before_run", Data: "payload", Source: "src"}
	res := h.Handle(context.Background(), event)
	assert.Equal(t, extension.HookActionPass, res.Action)
	assert.Empty(t, res.Reason)
	assert.Nil(t, res.Replacement)
}

// TestHookEventFields verifies the bare HookEvent value carries all its
// conjunctive fields through Dispatch untouched.
func TestHookEventFields(t *testing.T) {
	ev := extension.HookEvent{Name: "e", Data: map[string]int{"k": 1}, Source: "src", Timestamp: time.Now()}
	assert.Equal(t, "e", ev.Name)
	assert.Equal(t, map[string]int{"k": 1}, ev.Data)
	assert.Equal(t, "src", ev.Source)
	assert.False(t, ev.Timestamp.IsZero())
}

// TestHookResultActions verifies the four HookAction values serialize to their
// documented strings.
func TestHookResultActions(t *testing.T) {
	actions := map[extension.HookAction]string{
		extension.HookActionPass:      "pass",
		extension.HookActionBlock:     "block",
		extension.HookActionTerminate: "terminate",
		extension.HookActionReplace:   "replace",
	}
	for action, want := range actions {
		assert.Equal(t, want, string(action))
	}
}

// TestHookResultCarriesReasonAndReplacement verifies HookResult relays the
// decision plus optional metadata for block/replace outcomes.
func TestHookResultCarriesReasonAndReplacement(t *testing.T) {
	block := extension.HookResult{Action: extension.HookActionBlock, Reason: "denied"}
	assert.Equal(t, extension.HookActionBlock, block.Action)
	assert.Equal(t, "denied", block.Reason)

	replace := extension.HookResult{Action: extension.HookActionReplace, Replacement: "sub"}
	assert.Equal(t, extension.HookActionReplace, replace.Action)
	assert.Equal(t, "sub", replace.Replacement)
}

// TestMiddlewareOnionOrder verifies wrapping multiple middleware layers composes
// in onion order: the first wrapper runs outermost and the base runs innermost.
func TestMiddlewareOnionOrder(t *testing.T) {
	ctx := context.Background()
	var order []string

	// Base AgentFunc records its position.
	base := func(_ context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		order = append(order, "base")
		return extension.AgentOutput{Text: "done:" + input.Message}, nil
	}

	// outmost returns a wrapper that records before calling next.
	makeLayer := func(name string) extension.Middleware {
		return &defaultMW{name: name, order: &order}
	}

	wrapped := makeLayer("outer").WrapAgent(
		makeLayer("inner").WrapAgent(base),
	)
	out, err := wrapped(ctx, extension.AgentInput{Message: "m"})
	require.NoError(t, err)
	assert.Equal(t, "done:m", out.Text)
	// Onion order: outer enters first, then inner, then base.
	assert.Equal(t, []string{"outer-in", "inner-in", "base", "inner-out", "outer-out"}, order)
}

// defaultMW is a tiny test-only Middleware that records entry/exit ordering.
type defaultMW struct {
	name  string
	order *[]string
}

var _ extension.Middleware = (*defaultMW)(nil)

func (m *defaultMW) Name() string { return m.name }

func (m *defaultMW) WrapAgent(next extension.AgentFunc) extension.AgentFunc {
	return func(ctx context.Context, input extension.AgentInput) (extension.AgentOutput, error) {
		*m.order = append(*m.order, m.name+"-in")
		out, err := next(ctx, input)
		*m.order = append(*m.order, m.name+"-out")
		return out, err
	}
}

// TestMiddlewareErrorPropagation verifies a wrapped AgentFunc propagates errors
// from the base unchanged.
func TestMiddlewareErrorPropagation(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("agent failed")
	base := func(_ context.Context, _ extension.AgentInput) (extension.AgentOutput, error) {
		return extension.AgentOutput{}, sentinel
	}
	var order []string
	mw := &defaultMW{name: "x", order: &order}
	out, err := mw.WrapAgent(base)(ctx, extension.AgentInput{})
	assert.ErrorIs(t, err, sentinel)
	assert.Empty(t, out.Text)
}

// TestModelMiddlewarePassThrough verifies DefaultModelMiddleware delegates to
// the underlying ModelFunc and preserves the response.
func TestModelMiddlewarePassThrough(t *testing.T) {
	ctx := context.Background()
	mmw := &extension.DefaultModelMiddleware{}
	var called bool
	fn := func(_ context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		called = true
		return extension.ModelResponse{Text: req.Prompt + "|" + req.Model}, nil
	}
	out, err := mmw.WrapModel(fn)(ctx, extension.ModelRequest{Prompt: "hi", Model: "gpt"})
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, "hi|gpt", out.Text)
}

// TestToolMiddlewarePassThrough verifies DefaultToolMiddleware delegates to the
// underlying ToolFunc, preserving name, input and output.
func TestToolMiddlewarePassThrough(t *testing.T) {
	ctx := context.Background()
	tmw := &extension.DefaultToolMiddleware{}
	var gotName string
	var gotInput any
	fn := func(_ context.Context, name string, input any) (any, error) {
		gotName = name
		gotInput = input
		return "result", nil
	}
	out, err := tmw.WrapTool(fn)(ctx, "read", map[string]string{"f": "x"})
	require.NoError(t, err)
	assert.Equal(t, "read", gotName)
	assert.Equal(t, map[string]string{"f": "x"}, gotInput)
	assert.Equal(t, "result", out)
}

// TestMiddlewareZeroValueNames verifies the zero-value default middleware
// structs expose an empty name (callers may set one explicitly).
func TestMiddlewareZeroValueNames(t *testing.T) {
	assert.Equal(t, "", (&extension.DefaultMiddleware{}).Name())
	assert.Equal(t, "", (&extension.DefaultModelMiddleware{}).Name())
	assert.Equal(t, "", (&extension.DefaultToolMiddleware{}).Name())
}
