// Package mcp implements the MCP (Model Context Protocol) integration layer.
// It defines the MCPClient contract, the transport enum, the tool-name
// normalization helpers (mcp__{server}__{tool}) and the shared types used by
// the SDK adapters and the tool registry adapter. The concrete wire adapters
// live in adapter.go and the MCP tool adapter in tool_adapter.go.
package mcp

import (
	"context"
	"strings"
)

// MCPTool describes a single tool exposed by an MCP server. ArgsSchema is a
// JSON-schema shaped map describing the accepted arguments.
type MCPTool struct {
	// Name is the tool's name as known by the remote server.
	Name string `json:"name"`
	// Description is a human-readable description of the tool.
	Description string `json:"description"`
	// ArgsSchema is a JSON-schema shaped argument specification.
	ArgsSchema map[string]any `json:"args_schema,omitempty"`
}

// MCPToolResult is the outcome of calling a single MCP tool.
type MCPToolResult struct {
	// Content is the tool's output. It may be a string, a structured value,
	// or a slice of content blocks depending on the server.
	Content any `json:"content"`
	// IsError reports whether the server signaled an execution error while
	// still returning a (possibly partial) result.
	IsError bool `json:"is_error,omitempty"`
}

// MCPTransport enumerates the supported MCP transport modes.
type MCPTransport string

const (
	// MCPTransportStdio speaks JSON-RPC over a subprocess stdin/stdout pair.
	MCPTransportStdio MCPTransport = "stdio"
	// MCPTransportSSE speaks JSON-RPC over a Server-Sent-Events endpoint.
	MCPTransportSSE MCPTransport = "sse"
	// MCPTransportStreamableHTTP speaks JSON-RPC over an HTTP stream.
	MCPTransportStreamableHTTP MCPTransport = "streamable-http"
)

// String returns the canonical string form of the transport.
func (t MCPTransport) String() string { return string(t) }

// MCPServerConfig configures a connection to an MCP server.
type MCPServerConfig struct {
	// Name is the logical name of the server, used in tool-name normalization.
	Name string `json:"name"`
	// Command is the executable to spawn for the Stdio transport.
	Command string `json:"command,omitempty"`
	// Transport selects the transport mode.
	Transport MCPTransport `json:"transport"`
	// Env holds extra environment variables (KEY=VALUE) for the Stdio child.
	Env []string `json:"env,omitempty"`
	// Args holds the command-line arguments for the Stdio child.
	Args []string `json:"args,omitempty"`
	// URL is the server endpoint for SSE/HTTP transports.
	URL string `json:"url,omitempty"`
}

// MCPClient is the contract every MCP client adapter satisfies. Both SDK
// adapters (OfficialSDKAdapter and Mark3labsAdapter) and the test mock
// (internal/mock) implement it so they are interchangeable.
type MCPClient interface {
	// Connect establishes the session with the MCP server.
	Connect(ctx context.Context) error
	// Disconnect tears down the session with the MCP server.
	Disconnect(ctx context.Context) error
	// ListTools returns the tools the server declares.
	ListTools(ctx context.Context) ([]MCPTool, error)
	// CallTool invokes the named tool with the given arguments.
	CallTool(ctx context.Context, name string, args map[string]any) (*MCPToolResult, error)
	// Name returns the logical name of the connected server.
	Name() string
}

// NormalizeToolName returns the canonical registry name for a server tool:
// mcp__{server}__{tool}.
func NormalizeToolName(serverName, toolName string) string {
	return "mcp__" + serverName + "__" + toolName
}

// ParseToolName reverses NormalizeToolName. It returns isMCP=false when the
// name is not an MCP-prefixed tool name. When a server or tool name itself
// contains "__", the FIRST segment after "mcp__" is treated as the server and
// the remainder (re-joined with "__") as the tool name.
func ParseToolName(name string) (server, tool string, isMCP bool) {
	if !strings.HasPrefix(name, "mcp__") {
		return "", "", false
	}

	rest := strings.TrimPrefix(name, "mcp__")
	server, tool, ok := strings.Cut(rest, "__")
	if !ok || server == "" || tool == "" {
		return "", "", false
	}
	return server, tool, true
}
