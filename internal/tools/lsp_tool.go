package tools

import (
	"context"
	"fmt"
)

// lspTool wraps an LSPClient as a ToolDefinition, exposing semantic code
// queries (definition, references, hover, diagnostics) to the agent.
type lspTool struct {
	client LSPClient
}

var _ ToolDefinition = (*lspTool)(nil)
var _ Parameterized = (*lspTool)(nil)

// NewLSPTool returns a ToolDefinition backed by the given LSPClient.
func NewLSPTool(client LSPClient) ToolDefinition {
	return &lspTool{client: client}
}

// Name returns the tool name.
func (t *lspTool) Name() string { return "lsp_query" }

// Description returns a brief description of the tool.
func (t *lspTool) Description() string {
	return "lsp_query: Query a Language Server for semantic code information. " +
		"Supports operations: definition, references, hover, diagnostics."
}

// Parameters returns the JSON Schema describing the tool's input parameters.
func (t *lspTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"enum":        []string{"definition", "references", "hover", "diagnostics"},
				"description": "The LSP operation to perform.",
			},
			"uri": map[string]any{
				"type":        "string",
				"description": "File URI (e.g. file:///path/to/file.go).",
			},
			"line": map[string]any{
				"type":        "integer",
				"description": "0-based line number.",
			},
			"character": map[string]any{
				"type":        "integer",
				"description": "0-based character offset.",
			},
		},
		"required": []string{"operation", "uri"},
	}
}

// Execute runs the LSP query for the given call.
func (t *lspTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	return nil, fmt.Errorf("lsp_query: not implemented")
}
