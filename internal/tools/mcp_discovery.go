package tools

import (
	"context"
	"errors"
)

// MCPDiscoveryController owns the configured MCP source and activation lifecycle.
// DiscoverMCP returns a local action-scoped join signal; it must include failed
// candidate cleanup, while successfully published clients belong to the assembly.
// A missing or open signal never proves activation completed.
type MCPDiscoveryController interface {
	DiscoverMCP(context.Context, string, string) (string, <-chan struct{}, error)
}

// MCPDiscoveryTool activates one configured server without granting trusted
// effects. Its concrete request contract verifies the controller's join signal.
type MCPDiscoveryTool struct {
	controller MCPDiscoveryController
	owned      *ownedMCPDiscoveryExecutor
}

type ownedMCPDiscoveryExecutor struct {
	discover func(context.Context, string, string) (string, <-chan struct{}, error)
}

// NewMCPDiscoveryTool binds a generic controller without completion authority.
func NewMCPDiscoveryTool(controller MCPDiscoveryController) *MCPDiscoveryTool {
	return &MCPDiscoveryTool{controller: controller}
}

// NewOwnedMCPDiscoveryTool explicitly captures the host-owned activation
// controller's exact executor and reviewed action-local join contract. A generic
// controller or a closed channel alone never grants this authority.
func NewOwnedMCPDiscoveryTool(controller MCPDiscoveryController) *MCPDiscoveryTool {
	tool := NewMCPDiscoveryTool(controller)
	if controller != nil {
		tool.owned = &ownedMCPDiscoveryExecutor{discover: controller.DiscoverMCP}
	}
	return tool
}

func (*MCPDiscoveryTool) Name() string { return "mcp_discover" }
func (*MCPDiscoveryTool) Description() string {
	return "Activate one configured MCP server by setting server, exposing its tool schemas for the next model request. Use mcp_list for local metadata without connecting. Calls to discovered tools still require normal approval."
}
func (*MCPDiscoveryTool) Parameters() any {
	return map[string]any{"type": "object", "properties": map[string]any{"server": map[string]any{"type": "string"}, "query": map[string]any{"type": "string"}}, "additionalProperties": false}
}
func (t *MCPDiscoveryTool) Execute(ctx context.Context, call ToolCall) (*ToolResult, error) {
	if t == nil || t.controller == nil {
		return nil, errors.New("tools: MCP discovery controller unavailable")
	}
	server, _ := call.Args["server"].(string)
	query, _ := call.Args["query"].(string)
	discover := t.controller.DiscoverMCP
	if t.owned != nil {
		discover = t.owned.discover
	}
	output, done, err := discover(ctx, server, query)
	if done != nil {
		select {
		case <-done:
			return &ToolResult{Output: output, ToolCallID: call.ID}, err
		default:
		}
	}
	return nil, &mcpDiscoveryPendingError{cause: err, done: done}
}

type mcpDiscoveryPendingError struct {
	cause error
	done  <-chan struct{}
}

func (*mcpDiscoveryPendingError) Error() string {
	return "tools: MCP activation resources have not joined"
}
func (e *mcpDiscoveryPendingError) Unwrap() error                  { return e.cause }
func (e *mcpDiscoveryPendingError) ExecutionDone() <-chan struct{} { return e.done }
func (*mcpDiscoveryPendingError) OutcomeUnknown() bool             { return true }
