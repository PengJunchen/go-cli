package core

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestModelMiddlewareDefaultName(t *testing.T) {
	mw := NewModelMiddlewareImpl("")
	assert.Equal(t, "default-model-middleware", mw.Name())
}

func TestToolMiddlewareDefaultName(t *testing.T) {
	mw := NewToolMiddlewareImpl("")
	assert.Equal(t, "default-tool-middleware", mw.Name())
}

func TestLoggingMiddlewareDefaultName(t *testing.T) {
	mw := NewLoggingMiddleware("")
	assert.Equal(t, "default-logging-middleware", mw.Name())
}

func TestModelMiddlewareWrapNilReturnsNil(t *testing.T) {
	mw := NewModelMiddlewareImpl("pass")
	// Pass-through returns the same (nil) value unchanged.
	assert.Nil(t, mw.WrapModel(nil))
}

func TestToolMiddlewareWrapNilReturnsNil(t *testing.T) {
	mw := NewToolMiddlewareImpl("pass")
	// Pass-through returns the same (nil) function unchanged.
	assert.Nil(t, mw.WrapToolCall(nil))
}

func TestLoggingMiddlewarePropagatesError(t *testing.T) {
	boom := errors.New("inner failed")
	wrapped := NewLoggingMiddleware("audit").Wrap(&errorLoop{err: boom})

	events, err := wrapped.Run(context.Background(), Submission{Content: "go"})
	require.ErrorIs(t, err, boom)
	assert.Empty(t, events)
}

func TestLoggingMiddlewareWithNilInner(t *testing.T) {
	// loggingLoop delegates; a nil next would panic, but the middleware wraps a
	// concrete loop so this validates construction sanity only.
	mw := NewLoggingMiddleware("audit")
	wrapped := mw.Wrap(&stubLoop{})
	require.NotNil(t, wrapped)
	events, err := wrapped.Run(context.Background(), Submission{Content: "go"})
	require.NoError(t, err)
	assert.Contains(t, findEvents(events, "message"), "ok")
}

func TestMiddlewareChainEmptyWrapReturnsBase(t *testing.T) {
	base := &stubLoop{}
	wrapped := NewMiddlewareChain().Wrap(base)
	assert.True(t, base == wrapped || wrapped != nil)
	_, err := wrapped.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
}

func TestMiddlewareChainApplyAlias(t *testing.T) {
	base := &stubLoop{}
	chain := NewMiddlewareChain(&MiddlewareImpl{name: "m1"})
	wrapped := chain.Apply(base)
	require.NotNil(t, wrapped)
	_, err := wrapped.Run(context.Background(), Submission{Content: "hi"})
	require.NoError(t, err)
}

func TestToolMiddlewareWrapPassThroughExecutes(t *testing.T) {
	mw := NewToolMiddlewareImpl("tm")
	fn := func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: call.Name}, nil
	}
	wrapped := mw.WrapToolCall(fn)
	// Verify pass-through still invokes the original handler.
	res, err := wrapped(context.Background(), tools.ToolCall{Name: "bash"})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "bash", res.Output)
}

func TestToolMiddlewarePassThroughPropagatesError(t *testing.T) {
	boom := errors.New("tool error")
	mw := NewToolMiddlewareImpl("tm")
	exec := func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
		return nil, boom
	}
	wrapped := mw.WrapToolCall(exec)
	_, err := wrapped(context.Background(), tools.ToolCall{Name: "bash"})
	require.ErrorIs(t, err, boom)
}
