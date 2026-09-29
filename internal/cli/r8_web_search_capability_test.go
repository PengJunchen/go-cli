package cli

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestAssembleAgent_UnconfiguredWebSearchIsHiddenFromCapabilitySurfaces(t *testing.T) {
	ctx := context.Background()
	assembly, err := AssembleAgent(ctx, newAssembleTestConfig(t), "openai", "web-search-fixture", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	_, err = assembly.ToolRegistry.Get(ctx, "web_search")
	require.ErrorIs(t, err, tools.ErrToolNotFound)

	var toolsOutput strings.Builder
	_, err = (&ToolsHandler{}).Handle(ctx, nil, &slashContext{toolRegistry: assembly.ToolRegistry, out: &toolsOutput})
	require.NoError(t, err)
	assert.Contains(t, toolsOutput.String(), "web_fetch", "configured default network tools remain visible")
	assert.NotContains(t, toolsOutput.String(), "  web_search:")

	toolSearch, err := assembly.ToolRegistry.Get(ctx, "tool_search")
	require.NoError(t, err)
	searchResult, err := toolSearch.Execute(ctx, tools.ToolCall{
		Name: "tool_search",
		Args: map[string]any{"query": "web_search"},
	})
	require.NoError(t, err)
	assert.NotContains(t, searchResult.Output, "web_search\t")

	topTools, ok := toolSearch.(tools.TopToolsProvider)
	require.True(t, ok)
	ranked, err := topTools.TopTools(ctx, "web_search", 1000)
	require.NoError(t, err)
	for _, definition := range ranked {
		assert.NotEqual(t, "web_search", definition.Name())
	}
}

func TestAssembleAgent_ConfiguredWebSearchRemainsDiscoverable(t *testing.T) {
	ctx := context.Background()
	cfg := newAssembleTestConfig(t)
	cfg.WebSearch.Provider = "fetch"
	var request *http.Request
	withAssembleDefaultTransport(t, assembleRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		request = req
		return searchResponse(req, `<div class="result"><a class="result__a" href="https://example.org">Result</a><a class="result__snippet">Snippet</a></div>`), nil
	}))
	assembly, err := AssembleAgent(ctx, cfg, "openai", "web-search-fixture", io.Discard, WithApproveMode(ApproveAuto))
	require.NoError(t, err)
	t.Cleanup(assembly.Cleanup)

	search, err := assembly.ToolRegistry.Get(ctx, "web_search")
	require.NoError(t, err)

	var toolsOutput strings.Builder
	_, err = (&ToolsHandler{}).Handle(ctx, nil, &slashContext{toolRegistry: assembly.ToolRegistry, out: &toolsOutput})
	require.NoError(t, err)
	assert.Contains(t, toolsOutput.String(), "  web_search:")

	toolSearch, err := assembly.ToolRegistry.Get(ctx, "tool_search")
	require.NoError(t, err)
	searchResult, err := toolSearch.Execute(ctx, tools.ToolCall{
		Name: "tool_search",
		Args: map[string]any{"query": "web_search"},
	})
	require.NoError(t, err)
	assert.Contains(t, searchResult.Output, "web_search\t")

	topTools, ok := toolSearch.(tools.TopToolsProvider)
	require.True(t, ok)
	ranked, err := topTools.TopTools(ctx, "web_search", 1000)
	require.NoError(t, err)
	require.Contains(t, toolNames(ranked), "web_search")

	result, err := search.Execute(ctx, tools.ToolCall{
		Name: "web_search",
		Args: map[string]any{"query": "provider capability"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Contains(t, result.Output, "provider capability")
	require.NotNil(t, request, "configured fetch provider must execute through the assembled tool")
	assert.Equal(t, "html.duckduckgo.com", request.URL.Host)
}

func toolNames(definitions []tools.ToolDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name())
	}
	return names
}
