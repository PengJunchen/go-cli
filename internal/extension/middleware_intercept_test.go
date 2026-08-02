package extension_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/extension"
)

// interceptModelMiddleware decorates a ModelFunc to mutate the request prompt
// before delegating and to post-process the response text after.
type interceptModelMiddleware struct{}

var _ extension.ModelMiddleware = interceptModelMiddleware{}

func (interceptModelMiddleware) Name() string { return "intercept-model" }

func (interceptModelMiddleware) WrapModel(next extension.ModelFunc) extension.ModelFunc {
	return func(ctx context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		req.Prompt = "PRE:" + req.Prompt
		resp, err := next(ctx, req)
		if err != nil {
			return resp, err
		}
		resp.Text += ":POST"
		return resp, nil
	}
}

// interceptToolMiddleware decorates a ToolFunc to rewrite the tool name before
// delegation.
type interceptToolMiddleware struct{}

var _ extension.ToolMiddleware = interceptToolMiddleware{}

func (interceptToolMiddleware) Name() string { return "intercept-tool" }

func (interceptToolMiddleware) WrapTool(next extension.ToolFunc) extension.ToolFunc {
	return func(ctx context.Context, name string, input any) (any, error) {
		return next(ctx, "wrapped:"+name, input)
	}
}

// TestModelMiddlewareIntercepts verifies a middleware can mutate both the
// request and the response (true interception rather than pass-through).
func TestModelMiddlewareIntercepts(t *testing.T) {
	ctx := context.Background()
	mw := interceptModelMiddleware{}
	base := func(_ context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: "echo(" + req.Prompt + ")"}, nil
	}
	out, err := mw.WrapModel(base)(ctx, extension.ModelRequest{Prompt: "hi"})
	require.NoError(t, err)
	assert.Equal(t, "echo(PRE:hi):POST", out.Text)
}

// TestToolMiddlewareIntercepts verifies a middleware can rewrite the tool name
// before the underlying tool executes.
func TestToolMiddlewareIntercepts(t *testing.T) {
	ctx := context.Background()
	mw := interceptToolMiddleware{}
	var seen string
	base := func(_ context.Context, name string, _ any) (any, error) {
		seen = name
		return "ok", nil
	}
	out, err := mw.WrapTool(base)(ctx, "read", nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", out)
	assert.Equal(t, "wrapped:read", seen)
}

// TestModelMiddlewareUnwrapChain verifies composing generic model middleware
// preserves the original semantics end to end.
func TestModelMiddlewareUnwrapChain(t *testing.T) {
	ctx := context.Background()
	mmw := &extension.DefaultModelMiddleware{}
	base := func(_ context.Context, req extension.ModelRequest) (extension.ModelResponse, error) {
		return extension.ModelResponse{Text: req.Prompt}, nil
	}
	out, err := mmw.WrapModel(base)(ctx, extension.ModelRequest{Prompt: "p"})
	require.NoError(t, err)
	assert.Equal(t, "p", out.Text)
}

// TestMiddlewareWrapIdempotent verifies reusing the same middleware instance to
// wrap produce a fresh callable each time without shared state.
func TestMiddlewareWrapIdempotent(t *testing.T) {
	ctx := context.Background()
	mw := &extension.DefaultMiddleware{}
	count := 0
	base := func(_ context.Context, _ extension.AgentInput) (extension.AgentOutput, error) {
		count++
		return extension.AgentOutput{Text: "x"}, nil
	}
	f1 := mw.WrapAgent(base)
	f2 := mw.WrapAgent(base)
	_, err := f1(ctx, extension.AgentInput{})
	require.NoError(t, err)
	_, err = f2(ctx, extension.AgentInput{})
	require.NoError(t, err)
	assert.Equal(t, 2, count, "each wrapped function should reach the base")
}
