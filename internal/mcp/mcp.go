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

// SupportedProtocolVersions lists the MCP protocol versions this client can
// negotiate, ordered from oldest to newest.
var SupportedProtocolVersions = []string{
	"2024-11-05",
	"2025-03-26",
	"2025-06-18",
}

// LatestProtocolVersion is the newest protocol version the client advertises
// in the initialize request.
const LatestProtocolVersion = "2025-06-18"

// OAuthConfig holds the parameters for the OAuth 2.1 Authorization Code flow
// triggered when the server responds with 401 + WWW-Authenticate.
type OAuthConfig struct {
	// AuthorizationURL is the OAuth 2.1 authorization endpoint.
	AuthorizationURL string `json:"authorization_url,omitempty"`
	// TokenURL is the OAuth 2.1 token endpoint.
	TokenURL string `json:"token_url,omitempty"`
	// ClientID is the OAuth 2.1 client identifier.
	ClientID string `json:"client_id,omitempty"`
	// ClientSecret is the optional OAuth 2.1 client secret.
	ClientSecret string `json:"client_secret,omitempty"`
	// RedirectURL is the local redirect URI for the authorization code flow.
	RedirectURL string `json:"redirect_url,omitempty"`
	// Scopes is the list of OAuth 2.1 scopes to request.
	Scopes []string `json:"scopes,omitempty"`
}

// IsSupportedProtocolVersion reports whether version is one of the
// SupportedProtocolVersions.
func IsSupportedProtocolVersion(version string) bool {
	for _, v := range SupportedProtocolVersions {
		if v == version {
			return true
		}
	}
	return false
}

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
	// Headers holds custom HTTP headers for SSE/HTTP transports.
	Headers map[string]string `json:"headers,omitempty"`
	// BearerToken is sent as the Authorization: Bearer <token> header for
	// SSE/HTTP transports.
	BearerToken string `json:"bearer_token,omitempty"`
	// OAuthConfig configures the OAuth 2.1 authorization code flow used when
	// the server responds with 401 + WWW-Authenticate.
	OAuthConfig *OAuthConfig `json:"oauth_config,omitempty"`
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
	// ProtocolVersion returns the protocol version negotiated during the
	// initialize handshake, or "" before Connect.
	ProtocolVersion() string
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
