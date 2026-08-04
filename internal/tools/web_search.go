package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// WebSearchToolOption configures a WebSearchTool.
type WebSearchToolOption func(*WebSearchTool)

// WithSearchProvider sets the search backend provider. When not set, a
// MockSearchProvider is used by default for backward compatibility.
func WithSearchProvider(p WebSearchProvider) WebSearchToolOption {
	return func(t *WebSearchTool) { t.provider = p }
}

// WebSearchTool performs a web search and returns results. It delegates to a
// WebSearchProvider; by default a MockSearchProvider is used for backward
// compatibility. It implements the ToolDefinition interface.
type WebSearchTool struct {
	provider WebSearchProvider
}

var _ ToolDefinition = (*WebSearchTool)(nil)

// NewWebSearchTool returns a WebSearchTool configured with the given options.
// When no provider option is supplied, a MockSearchProvider is used.
func NewWebSearchTool(opts ...WebSearchToolOption) *WebSearchTool {
	t := &WebSearchTool{}
	for _, opt := range opts {
		opt(t)
	}
	if t.provider == nil {
		t.provider = NewMockSearchProvider()
	}
	return t
}

// Name returns the tool name.
func (t *WebSearchTool) Name() string { return "web_search" }

// Description returns a brief description of the tool.
func (t *WebSearchTool) Description() string {
	return "web_search: searches the web for a query and returns results. Args: query (string)."
}

// Execute takes args["query"] and returns formatted search results from the
// configured provider.
func (t *WebSearchTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	query, ok := call.Args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return nil, errors.New("web_search: missing string argument 'query'")
	}

	results, err := t.provider.Search(ctx, query, SearchOptions{
		MaxResults: defaultFetchSearchMaxResults,
	})
	if err != nil {
		return nil, fmt.Errorf("web_search: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for: %q\n\n", query))
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, r.Title))
		sb.WriteString(fmt.Sprintf("   %s\n", r.URL))
		sb.WriteString(fmt.Sprintf("   %s\n\n", r.Snippet))
	}
	output := strings.TrimSuffix(sb.String(), "\n")

	_, isMock := t.provider.(*MockSearchProvider)

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"query":   query,
			"results": len(results),
			"mock":    isMock,
		},
	}, nil
}
