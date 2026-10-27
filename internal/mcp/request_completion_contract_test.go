package mcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

type wrappedRequestClient struct{ MCPClient }

func TestMCPRequestCompletionRequiresExactOwnedAdapter(t *testing.T) {
	httpClient := NewHTTPClientAdapter(MCPServerConfig{Name: "srv"})
	for _, test := range []struct {
		name      string
		client    MCPClient
		supported bool
	}{
		{"http", httpClient, true},
		{"stdio", NewOfficialSDKAdapter(MCPServerConfig{Name: "srv"}), true},
		{"mark3labs", NewMark3labsAdapter(MCPServerConfig{Name: "srv"}), true},
		{"custom", &fakeClient{name: "srv"}, false},
		{"wrapped", &wrappedRequestClient{MCPClient: httpClient}, false},
		{"external-stdio", NewOfficialSDKAdapter(MCPServerConfig{Name: "srv"}, WithConnection(nil)), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			generation, err := NewMCPClientGeneration(test.client)
			require.NoError(t, err)
			candidate, err := generation.NewToolAdapter(MCPTool{Name: "lookup", ArgsSchema: map[string]any{"type": "object", "properties": map[string]any{}}}).MCPGenerationCandidate()
			require.NoError(t, err)
			registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
			update, err := registry.BeginMCPGenerationUpdate(context.Background(), "source", 1, []tools.MCPToolCandidate{candidate})
			require.NoError(t, err)
			require.NoError(t, update.PublishMaterialized())
			wrapped := tools.NewMiddlewareToolRegistry(registry)
			require.NoError(t, wrapped.SetPreparedToolStages(tools.PreparedToolStages{}))
			require.NoError(t, wrapped.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
				return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
			}))
			invocation, err := wrapped.PrepareToolCall(context.Background(), tools.ToolCall{Name: "mcp__srv__lookup", Args: map[string]any{}})
			require.NoError(t, err)
			supported, err := tools.PreparedCompletionSupported(context.Background(), invocation, nil)
			require.NoError(t, err)
			require.Equal(t, test.supported, supported)
			require.Equal(t, []tools.TrustedToolEffect{tools.EffectUnknown}, invocation.EffectSet().Values())
			require.Nil(t, invocation.CompletionSignal().Done(), "preflight may not dispatch")
		})
	}
}

func TestOwnedStdioPreparedRequestCompletionAndUnknownBoundary(t *testing.T) {
	for _, mode := range []string{"lifetime", "silent"} {
		t.Run(mode, func(t *testing.T) {
			adapter, _, wireFile := newOwnedStdioHelperAdapter(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, adapter.Connect(ctx))
			generation, err := NewMCPClientGeneration(adapter)
			require.NoError(t, err)
			candidate, err := generation.NewToolAdapter(MCPTool{Name: "alive", ArgsSchema: map[string]any{"type": "object", "properties": map[string]any{}}}).MCPGenerationCandidate()
			require.NoError(t, err)
			registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
			update, err := registry.BeginMCPGenerationUpdate(ctx, "stdio-source", 1, []tools.MCPToolCandidate{candidate})
			require.NoError(t, err)
			require.NoError(t, update.PublishMaterialized())
			preparedRegistry := tools.NewMiddlewareToolRegistry(registry)
			require.NoError(t, preparedRegistry.SetPreparedToolStages(tools.PreparedToolStages{}))
			require.NoError(t, preparedRegistry.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
				return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
			}))
			invocation, err := preparedRegistry.PrepareToolCall(ctx, tools.ToolCall{Name: "mcp__stdio-helper__alive", Args: map[string]any{}})
			require.NoError(t, err)
			supported, err := tools.PreparedCompletionSupported(ctx, invocation, nil)
			require.NoError(t, err)
			require.True(t, supported)
			callCtx := ctx
			if mode == "silent" {
				var stop context.CancelFunc
				callCtx, stop = context.WithTimeout(ctx, 150*time.Millisecond)
				defer stop()
			}
			result, err := invocation.Dispatch(callCtx)
			if mode == "lifetime" {
				require.NoError(t, err)
				require.Contains(t, result.Output, "alive")
				require.Equal(t, tools.CompletionMCPRequestJoined, invocation.CompletionSignal().Proof())
			} else {
				require.Error(t, err)
				var unknown OutcomeUnknown
				require.ErrorAs(t, err, &unknown)
				require.True(t, unknown.OutcomeUnknown())
				require.Empty(t, invocation.CompletionSignal().Proof())
			}
			require.NoError(t, adapter.Disconnect(ctx))
			wire, err := os.ReadFile(wireFile)
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(wire), "tools/call"), "no failed request may be replayed")
		})
	}
}

type overridingOwnedGeneration struct{ *MCPClientGeneration }

func (*overridingOwnedGeneration) ExecuteMCPTool(context.Context, string, tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "counterfeit"}, nil
}
func (*overridingOwnedGeneration) MCPRequestCompletionSupported() bool { return true }

func TestGenericHandleEmbeddingOwnedGenerationCannotReplaceExecutorAndKeepProof(t *testing.T) {
	generation, err := NewMCPClientGeneration(NewHTTPClientAdapter(MCPServerConfig{Name: "srv"}))
	require.NoError(t, err)
	handle, err := tools.NewMCPGenerationClientHandle(&overridingOwnedGeneration{MCPClientGeneration: generation})
	require.NoError(t, err)
	candidate, err := tools.NewMCPToolCandidate("mcp__srv__counterfeit", "counterfeit", "unreviewed", map[string]any{"type": "object", "properties": map[string]any{}}, handle, generation)
	require.NoError(t, err)
	registry := tools.NewDeferredToolRegistryAdapter(tools.NewDefaultToolRegistry())
	update, err := registry.BeginMCPGenerationUpdate(context.Background(), "counterfeit", 1, []tools.MCPToolCandidate{candidate})
	require.NoError(t, err)
	require.NoError(t, update.PublishMaterialized())
	prepared := tools.NewMiddlewareToolRegistry(registry)
	require.NoError(t, prepared.SetPreparedToolStages(tools.PreparedToolStages{}))
	require.NoError(t, prepared.SetPreparedToolPolicy(func(_ context.Context, request tools.PreparedPolicyRequest) (tools.PreparedPolicyDecision, error) {
		return tools.PreparedPolicyDecision{Allowed: true, Generations: request.Generations}, nil
	}))
	invocation, err := prepared.PrepareToolCall(context.Background(), tools.ToolCall{Name: "mcp__srv__counterfeit", Args: map[string]any{}})
	require.NoError(t, err)
	supported, err := tools.PreparedCompletionSupported(context.Background(), invocation, nil)
	require.NoError(t, err)
	require.False(t, supported)
}
