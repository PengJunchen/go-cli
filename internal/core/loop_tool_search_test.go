package core

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/llm"
	"github.com/pengjunchen/go-cli/internal/mock"
	"github.com/pengjunchen/go-cli/internal/tools"
)

// nameDescTool is a minimal ToolDefinition with configurable name and
// description, used to populate tool registries in dynamic filtering tests.
type nameDescTool struct {
	name        string
	description string
}

func (t *nameDescTool) Name() string        { return t.name }
func (t *nameDescTool) Description() string { return t.description }
func (t *nameDescTool) Execute(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "ok"}, nil
}

var _ tools.ToolDefinition = (*nameDescTool)(nil)

// topToolsProxy models a non-concrete ToolSearch wrapper that preserves the
// ranked-search capability without exposing the concrete ToolSearchTool type.
type topToolsProxy struct {
	tools.ToolDefinition
	provider tools.TopToolsProvider
}

var _ tools.TopToolsProvider = (*topToolsProxy)(nil)

func (p *topToolsProxy) TopTools(ctx context.Context, query string, k int) ([]tools.ToolDefinition, error) {
	return p.provider.TopTools(ctx, query, k)
}

type topToolsProxyRegistry struct {
	tools.ToolRegistry
}

func (r *topToolsProxyRegistry) List(ctx context.Context) ([]tools.ToolDefinition, error) {
	defs, err := r.ToolRegistry.List(ctx)
	if err != nil {
		return nil, err
	}
	for i, def := range defs {
		provider, ok := def.(tools.TopToolsProvider)
		if ok {
			defs[i] = &topToolsProxy{ToolDefinition: def, provider: provider}
		}
	}
	return defs, nil
}

// toolsFromOpts extracts the tool definitions from LLM generation options.
func toolsFromOpts(opts []llm.Option) []llm.ToolDefinition {
	var genOpts llm.GenerationOptions
	for _, opt := range opts {
		opt(&genOpts)
	}
	return genOpts.Tools
}

func TestDynamicToolInjection(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()

	// Register 20 tools with varied names/descriptions.
	for i := 0; i < 20; i++ {
		require.NoError(t, reg.Register(context.Background(), &nameDescTool{
			name:        fmt.Sprintf("tool_%d", i),
			description: fmt.Sprintf("performs operation %d on data", i),
		}))
	}

	// Register a ToolSearchTool so the loop can use it for filtering.
	searchTool := tools.NewToolSearchTool(reg)
	require.NoError(t, reg.Register(context.Background(), searchTool))

	// Mock LLM that returns a single turn (no tool calls).
	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-DS", "dynamic-search",
		mock.ConversationTurn{AssistantContent: "done"},
	))

	loop := NewLoopAgent(WithLLM(model), WithTools(reg))
	loop.WithToolSearchThreshold(5) // 21 tools > 5, filtering kicks in.

	_, err := loop.Run(context.Background(), Submission{Content: "use tool_3"})
	require.NoError(t, err)

	callLog := model.CallLog()
	require.NotEmpty(t, callLog)

	passedTools := toolsFromOpts(callLog[0].Options)
	assert.LessOrEqual(t, len(passedTools), 5, "should pass at most threshold tools")

	// The most relevant tool (tool_3) should be in the filtered set.
	var names []string
	for _, td := range passedTools {
		names = append(names, td.Name)
	}
	assert.Contains(t, names, "tool_3")
}

func TestDynamicToolFilteringUsesTopToolsProviderProxy(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()
	for i := 0; i < 20; i++ {
		require.NoError(t, reg.Register(context.Background(), &nameDescTool{
			name:        fmt.Sprintf("tool_%d", i),
			description: fmt.Sprintf("performs operation %d on data", i),
		}))
	}
	searchTool := tools.NewToolSearchTool(reg)
	require.NoError(t, reg.Register(context.Background(), searchTool))

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-DSP", "dynamic-search-proxy",
		mock.ConversationTurn{AssistantContent: "done"},
	))
	loop := NewLoopAgent(WithLLM(model), WithTools(&topToolsProxyRegistry{ToolRegistry: reg}))
	loop.WithToolSearchThreshold(5)

	_, err := loop.Run(context.Background(), Submission{Content: "use tool_3"})
	require.NoError(t, err)

	callLog := model.CallLog()
	require.NotEmpty(t, callLog)
	passedTools := toolsFromOpts(callLog[0].Options)
	assert.LessOrEqual(t, len(passedTools), 5, "proxy capability should preserve dynamic filtering")

	var names []string
	for _, td := range passedTools {
		names = append(names, td.Name)
	}
	assert.Contains(t, names, "tool_3")
}

func TestBelowThresholdNoSearch(t *testing.T) {
	reg := tools.NewDefaultToolRegistry()

	// Register just 3 tools (below threshold).
	for i := 0; i < 3; i++ {
		require.NoError(t, reg.Register(context.Background(), &nameDescTool{
			name:        fmt.Sprintf("tool_%d", i),
			description: fmt.Sprintf("performs operation %d", i),
		}))
	}

	searchTool := tools.NewToolSearchTool(reg)
	require.NoError(t, reg.Register(context.Background(), searchTool))

	model := mock.NewMockLLMServer(mock.NewConversationTemplate(
		"L-BT", "below-threshold",
		mock.ConversationTurn{AssistantContent: "done"},
	))

	loop := NewLoopAgent(WithLLM(model), WithTools(reg))
	loop.WithToolSearchThreshold(10) // 4 tools < 10, no filtering.

	_, err := loop.Run(context.Background(), Submission{Content: "use tool_1"})
	require.NoError(t, err)

	callLog := model.CallLog()
	require.NotEmpty(t, callLog)

	passedTools := toolsFromOpts(callLog[0].Options)
	// All 4 tools should be passed (no filtering).
	assert.Len(t, passedTools, 4)
}
