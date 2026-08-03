package tools

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebSearchName(t *testing.T) {
	tool := NewWebSearchTool()
	assert.Equal(t, "web_search", tool.Name())
}

func TestWebSearchDescription(t *testing.T) {
	tool := NewWebSearchTool()
	assert.Contains(t, tool.Description(), "web_search")
	assert.Contains(t, tool.Description(), "query")
}

func TestWebSearchReturnsResults(t *testing.T) {
	tool := NewWebSearchTool()
	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "call-1",
		Name: "web_search",
		Args: map[string]any{"query": "golang testing"},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Output)
	assert.Contains(t, res.Output, "golang testing")
	assert.Equal(t, "call-1", res.ToolCallID)
	assert.Equal(t, 3, res.Metadata["results"])
	assert.True(t, res.Metadata["mock"].(bool))
}

func TestWebSearchMissingQuery(t *testing.T) {
	tool := NewWebSearchTool()
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing string argument 'query'")
}

func TestWebSearchEmptyQuery(t *testing.T) {
	tool := NewWebSearchTool()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "  "},
	})
	assert.Error(t, err)
}

func TestWebSearchContainsURLs(t *testing.T) {
	tool := NewWebSearchTool()
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "test"},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "https://example.com/result1")
	assert.Contains(t, res.Output, "https://example.com/result2")
	assert.Contains(t, res.Output, "https://example.com/result3")
}

func TestWebSearchDeterministic(t *testing.T) {
	tool := NewWebSearchTool()
	res1, err1 := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "deterministic"},
	})
	require.NoError(t, err1)

	res2, err2 := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"query": "deterministic"},
	})
	require.NoError(t, err2)

	assert.Equal(t, res1.Output, res2.Output)
}
