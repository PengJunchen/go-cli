package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// WebSearchTool performs a web search and returns results. Because no real
// search API is wired in, it returns deterministic mock results derived from
// the query. It implements the ToolDefinition interface.
type WebSearchTool struct{}

var _ ToolDefinition = (*WebSearchTool)(nil)

// NewWebSearchTool returns a WebSearchTool.
func NewWebSearchTool() *WebSearchTool {
	return &WebSearchTool{}
}

// Name returns the tool name.
func (t *WebSearchTool) Name() string { return "web_search" }

// Description returns a brief description of the tool.
func (t *WebSearchTool) Description() string {
	return "web_search: searches the web for a query and returns results. Args: query (string)."
}

// Execute takes args["query"] and returns simulated search results as a
// formatted string. The results are deterministic and derived from the query.
func (t *WebSearchTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	query, ok := call.Args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		slog.Debug("web_search.missing_query")
		return nil, errors.New("web_search: missing string argument 'query'")
	}

	slog.Debug("web_search.start", "query", query)

	// Build deterministic mock results derived from the query.
	const numResults = 3
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Search results for: %q\n\n", query))
	for i := 1; i <= numResults; i++ {
		sb.WriteString(fmt.Sprintf("%d. Example Result %s\n", i, ordinal(i)))
		sb.WriteString(fmt.Sprintf("   https://example.com/result%d\n", i))
		sb.WriteString(fmt.Sprintf("   A relevant page about %s\n\n", query))
	}
	output := strings.TrimSuffix(sb.String(), "\n")

	slog.Debug("web_search.done", "query", query, "results", numResults, "mock", true)

	return &ToolResult{
		Output:     output,
		ToolCallID: call.ID,
		Metadata: map[string]any{
			"query":   query,
			"results": numResults,
			"mock":    true,
		},
	}, nil
}

// ordinal returns the ordinal suffix for a positive integer (1st, 2nd, 3rd, ...).
func ordinal(n int) string {
	switch n {
	case 1:
		return "One"
	case 2:
		return "Two"
	case 3:
		return "Three"
	case 4:
		return "Four"
	case 5:
		return "Five"
	default:
		return fmt.Sprintf("%d", n)
	}
}
