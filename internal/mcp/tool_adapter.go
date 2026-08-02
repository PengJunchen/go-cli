package mcp

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pengjunchen/go-cli/internal/tools"
	"github.com/pengjunchen/go-cli/internal/tracing"
)

// MCPToolAdapter adapts an MCPTool and its owning MCPClient into a
// tools.ToolDefinition so MCP tools can be registered in a tools registry and
// executed through the normal agent loop. The tool's registry name is
// normalized to mcp__{server}__{tool}.
type MCPToolAdapter struct {
	client MCPClient
	tool   MCPTool
}

var _ tools.ToolDefinition = (*MCPToolAdapter)(nil)

// NewMCPToolAdapter returns a ToolDefinition adapter for the given tool served
// by client.
func NewMCPToolAdapter(client MCPClient, tool MCPTool) *MCPToolAdapter {
	return &MCPToolAdapter{client: client, tool: tool}
}

// Name returns the normalized registry name mcp__{server}__{tool}.
func (a *MCPToolAdapter) Name() string {
	return NormalizeToolName(a.client.Name(), a.tool.Name)
}

// Description returns the MCP tool's description.
func (a *MCPToolAdapter) Description() string {
	return a.tool.Description
}

// Parameters returns the MCP tool's input schema so the LLM knows what
// arguments it can pass. Implements tools.Parameterized.
func (a *MCPToolAdapter) Parameters() any {
	if len(a.tool.ArgsSchema) == 0 {
		return nil
	}
	return a.tool.ArgsSchema
}

// Execute forwards the call to the remote MCP client and maps its result into
// a tools.ToolResult. ToolCallID is set so the result can be matched back to
// the originating call.
func (a *MCPToolAdapter) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	span, _ := tracing.SpanFromContext(ctx, "tool.call", tracing.SpanKindClient)
	span.SetAttributes(tracing.Attribute{Key: "tool_name", Value: call.Name})

	result, err := a.client.CallTool(ctx, a.tool.Name, call.Args)
	if err != nil {
		span.SetAttributes(tracing.Attribute{Key: "success", Value: false})
		slog.Error("mcp.tool_adapter.failed", "tool", a.tool.Name, "err", err)
		return nil, err
	}

	span.SetAttributes(tracing.Attribute{Key: "success", Value: result != nil && !result.IsError})

	output, ok := result.Content.(string)
	if !ok {
		output = fmt.Sprintf("%v", result.Content)
	}

	return &tools.ToolResult{
		Output:     output,
		ToolCallID: call.ID,
	}, nil
}
