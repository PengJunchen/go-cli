package extension

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultExtensionNameFallback verifies DefaultExtension reports the stable
// default name for a zero-value instance.
func TestDefaultExtensionNameFallback(t *testing.T) {
	def := &defaultExtension{}
	assert.Equal(t, "default-extension", def.Name())
}

// TestDefaultExtensionLifecycleNoop verifies the default stub satisfies the
// lifecycle contract without registering anything or failing.
func TestDefaultExtensionLifecycleNoop(t *testing.T) {
	ctx := context.Background()
	reg := NewExtensionRegistry()
	def := &defaultExtension{}
	require.NoError(t, def.Init(ctx, reg))
	require.NoError(t, def.Shutdown(ctx))
	assert.Equal(t, "default-extension", def.Name())
}

// TestDefaultHookNameAndPass verifies the default hook always returns
// HookActionPass and logs without changing the payload.
func TestDefaultHookNameAndPass(t *testing.T) {
	h := &defaultHook{}
	event := HookEvent{Name: "agent.before_run", Data: "payload", Source: "src"}
	res := h.Handle(context.Background(), event)
	assert.Equal(t, HookActionPass, res.Action)
	assert.Empty(t, res.Reason)
	assert.Nil(t, res.Replacement)
}

// TestHookEventFields verifies the bare HookEvent value carries all its
// conjunctive fields through Dispatch untouched.
func TestHookEventFields(t *testing.T) {
	ev := HookEvent{Name: "e", Data: map[string]int{"k": 1}, Source: "src", Timestamp: time.Now()}
	assert.Equal(t, "e", ev.Name)
	assert.Equal(t, map[string]int{"k": 1}, ev.Data)
	assert.Equal(t, "src", ev.Source)
	assert.False(t, ev.Timestamp.IsZero())
}

// TestHookResultActions verifies the four HookAction values serialize to their
// documented strings.
func TestHookResultActions(t *testing.T) {
	actions := map[HookAction]string{
		HookActionPass:      "pass",
		hookActionBlock:     "block",
		hookActionTerminate: "terminate",
		hookActionReplace:   "replace",
	}
	for action, want := range actions {
		assert.Equal(t, want, string(action))
	}
}

// TestHookResultCarriesReasonAndReplacement verifies HookResult relays the
// decision plus optional metadata for block/replace outcomes.
func TestHookResultCarriesReasonAndReplacement(t *testing.T) {
	block := HookResult{Action: hookActionBlock, Reason: "denied"}
	assert.Equal(t, hookActionBlock, block.Action)
	assert.Equal(t, "denied", block.Reason)

	replace := HookResult{Action: hookActionReplace, Replacement: "sub"}
	assert.Equal(t, hookActionReplace, replace.Action)
	assert.Equal(t, "sub", replace.Replacement)
}

// TestMiddlewareOnionOrder verifies wrapping multiple middleware layers composes
// in onion order: the first wrapper runs outermost and the base runs innermost.
func TestMiddlewareOnionOrder(t *testing.T) {
	ctx := context.Background()
	var order []string

	// Base AgentFunc records its position.
	base := func(_ context.Context, input AgentInput) (AgentOutput, error) {
		order = append(order, "base")
		return AgentOutput{Text: "done:" + input.Message}, nil
	}

	// outmost returns a wrapper that records before calling next.
	makeLayer := func(name string) Middleware {
		return &defaultMW{name: name, order: &order}
	}

	wrapped := makeLayer("outer").WrapAgent(
		makeLayer("inner").WrapAgent(base),
	)
	out, err := wrapped(ctx, AgentInput{Message: "m"})
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

var _ Middleware = (*defaultMW)(nil)

func (m *defaultMW) Name() string { return m.name }

func (m *defaultMW) WrapAgent(next AgentFunc) AgentFunc {
	return func(ctx context.Context, input AgentInput) (AgentOutput, error) {
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
	base := func(_ context.Context, _ AgentInput) (AgentOutput, error) {
		return AgentOutput{}, sentinel
	}
	var order []string
	mw := &defaultMW{name: "x", order: &order}
	out, err := mw.WrapAgent(base)(ctx, AgentInput{})
	assert.ErrorIs(t, err, sentinel)
	assert.Empty(t, out.Text)
}

// TestModelMiddlewarePassThrough verifies DefaultModelMiddleware delegates to
// the underlying ModelFunc and preserves the response.
func TestModelMiddlewarePassThrough(t *testing.T) {
	ctx := context.Background()
	mmw := &defaultModelMiddleware{}
	var called bool
	fn := func(_ context.Context, req ModelRequest) (ModelResponse, error) {
		called = true
		return ModelResponse{Text: req.Prompt + "|" + req.Model}, nil
	}
	out, err := mmw.WrapModel(fn)(ctx, ModelRequest{Prompt: "hi", Model: "gpt"})
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, "hi|gpt", out.Text)
}

// TestToolMiddlewarePassThrough verifies DefaultToolMiddleware delegates to the
// underlying ToolFunc, preserving name, input and output.
func TestToolMiddlewarePassThrough(t *testing.T) {
	ctx := context.Background()
	tmw := &defaultToolMiddleware{}
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
	assert.Equal(t, "", (&defaultMiddleware{}).Name())
	assert.Equal(t, "", (&defaultModelMiddleware{}).Name())
	assert.Equal(t, "", (&defaultToolMiddleware{}).Name())
}
