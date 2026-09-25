package approval

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pengjunchen/go-cli/internal/core"
	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type preparedNetworkRoundTripper struct{}

func (preparedNetworkRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader("local network response")),
		Request:    req,
	}, nil
}

func newPreparedNetworkRuntime(t *testing.T, classifier ApprovalClassifier, def tools.ToolDefinition, trusted bool) (*tools.MiddlewareToolRegistry, *core.DefaultPlanModeController) {
	t.Helper()
	ctx := context.Background()
	base := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry) //nolint:errcheck
	if trusted {
		registrar, ok := any(base).(tools.TrustedNetworkReadRegistrar)
		require.True(t, ok, "DefaultToolRegistry exposes the concrete network-read registrar")
		require.NoError(t, registrar.RegisterTrustedNetworkRead(ctx, def))
	} else {
		require.NoError(t, base.Register(ctx, def))
	}
	fence := base.DispatchFence()
	controller := core.NewDefaultPlanModeController(fence)
	middleware := NewApprovalMiddleware(classifier, NewInMemoryApprovalStore(),
		WithDispatchFence(fence), WithPlanModeController(controller))
	registry := tools.NewMiddlewareToolRegistry(base)
	require.NoError(t, registry.SetPreparedToolPolicy(middleware.PreparedPolicy))
	require.NoError(t, registry.SetPreparedToolStages(tools.PreparedToolStages{}))
	return registry, controller
}

func TestPreparedPlanAllowsOnlyTrustedConcreteNetworkRead(t *testing.T) {
	ctx := context.Background()
	for _, testCase := range []struct {
		name string
		def  tools.ToolDefinition
		args map[string]any
		want string
	}{
		{name: "web_search", def: tools.NewWebSearchTool(), args: map[string]any{"query": "local mock"}, want: "Search results for: \"local mock\""},
		{name: "web_fetch", def: tools.NewWebFetchTool(tools.WithWebFetchClient(&http.Client{Transport: preparedNetworkRoundTripper{}})), args: map[string]any{"url": "https://example.test/document"}, want: "local network response"},
	} {
		t.Run("trusted/"+testCase.name, func(t *testing.T) {
			registry, controller := newPreparedNetworkRuntime(t, &DenyAllClassifier{}, testCase.def, true)
			require.NoError(t, controller.Enter(ctx, "allow read-only network tool in plan mode"))
			invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: testCase.name, Args: testCase.args})
			require.NoError(t, err)
			result, err := invocation.Dispatch(ctx)
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Contains(t, result.Output, testCase.want, "the concrete trusted network-read tool should dispatch")
		})

		t.Run("ordinary/"+testCase.name, func(t *testing.T) {
			registry, controller := newPreparedNetworkRuntime(t, &DenyAllClassifier{}, testCase.def, false)
			require.NoError(t, controller.Enter(ctx, "reject ordinary network tool in plan mode"))
			_, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: testCase.name, Args: testCase.args})
			require.ErrorIs(t, err, ErrToolDenied)
		})
	}
}

func TestPreparedTrustedNetworkClassifierAllowIsPreserved(t *testing.T) {
	ctx := context.Background()
	registry, _ := newPreparedNetworkRuntime(t, &AllowAllClassifier{}, tools.NewWebSearchTool(), true)
	invocation, err := registry.PrepareToolCall(ctx, tools.ToolCall{Name: "web_search", Args: map[string]any{"query": "local mock"}})
	require.NoError(t, err, "trusted concrete network reads must honor an explicit classifier Allow")
	result, err := invocation.Dispatch(ctx)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Contains(t, result.Output, "Search results for: \"local mock\"")
}
