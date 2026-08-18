package mock

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/mcp"
)

// MCPCallRecord logs one MCP tool invocation made through the mock server.
type MCPCallRecord struct {
	// ToolName is the name of the invoked tool.
	ToolName string
	// Args are the arguments supplied to the call.
	Args map[string]any
	// Timestamp records when the call happened.
	Timestamp time.Time
	// Result is the value returned by the registered handler.
	Result any
}

// mockTool is a single registered MCP tool and its handler.
type mockTool struct {
	Name        string
	Description string
	Handler     func(args map[string]any) (any, error)
}

// MockMCPServer simulates an MCP protocol server. It can Start/Stop and
// register tools whose handers produce canned results. Every invocation is
// logged for assertions.
type MockMCPServer interface {
	// Start boots the simulated server.
	Start(ctx context.Context) error
	// Stop shuts the simulated server down.
	Stop(ctx context.Context) error
	// RegisterTool registers a tool backed by the given handler.
	RegisterTool(name, description string, handler func(args map[string]any) (any, error))
	// CallLog returns a copy of all recorded invocations.
	CallLog() []MCPCallRecord
	// Name returns the logical server name.
	Name() string
}

// MockMCPServerImpl is a concrete MockMCPServer. It also satisfies
// mcp.MCPClient so it can be used both as the simulated server and, directly,
// as the client driving a suite of contract tests.
type MockMCPServerImpl struct {
	name string
	mu   sync.Mutex
	// tools stores registered tools by name.
	tools map[string]*mockTool
	// callLog records every invocation.
	callLog []MCPCallRecord
	// running tracks the Start/Stop lifecycle.
	running bool
}

var _ MockMCPServer = (*MockMCPServerImpl)(nil)
var _ mcp.MCPClient = (*MockMCPServerImpl)(nil)

// NewMockMCPServer creates a simulated MCP server with the given logical name.
// When name is empty the client-facing Name() falls back to "mock".
func NewMockMCPServer(name string) *MockMCPServerImpl {
	return &MockMCPServerImpl{
		name:  name,
		tools: map[string]*mockTool{},
	}
}

// Name returns the logical server name, defaulting to "mock".
func (s *MockMCPServerImpl) Name() string {
	if s.name == "" {
		return "mock"
	}
	return s.name
}

// Start marks the server as running.
func (s *MockMCPServerImpl) Start(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = true
	return nil
}

// Stop marks the server as stopped.
func (s *MockMCPServerImpl) Stop(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	return nil
}

// RegisterTool registers a tool backed by the given handler. The handler
// receives the call arguments and returns the result value (and an error).
func (s *MockMCPServerImpl) RegisterTool(name, description string, handler func(args map[string]any) (any, error)) {
	if handler == nil {
		handler = func(map[string]any) (any, error) { return nil, nil }
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[name] = &mockTool{Name: name, Description: description, Handler: handler}
}

// Connect implements mcp.MCPClient. In-process mock used both as server and
// client, connecting is a lightweight no-op (Start is the lifecycle entry).
func (s *MockMCPServerImpl) Connect(_ context.Context) error {
	return nil
}

// Disconnect implements mcp.MCPClient. It is a lightweight no-op.
func (s *MockMCPServerImpl) Disconnect(_ context.Context) error {
	return nil
}

// ListTools implements mcp.MCPClient by returning the registered tools.
func (s *MockMCPServerImpl) ListTools(_ context.Context) ([]mcp.MCPTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tools := make([]mcp.MCPTool, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, mcp.MCPTool{
			Name:        t.Name,
			Description: t.Description,
		})
	}
	return tools, nil
}

// CallTool implements mcp.MCPClient by invoking the registered handler and
// recording the invocation. It also satisfies the MCPToolAdapter path so the
// mock can be wrapped and registered into a tools registry.
func (s *MockMCPServerImpl) CallTool(_ context.Context, name string, args map[string]any) (*mcp.MCPToolResult, error) {
	s.mu.Lock()
	tool, ok := s.tools[name]
	s.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("mock mcp: tool not found: %s", name)
	}

	result, err := tool.Handler(args)

	s.mu.Lock()
	s.callLog = append(s.callLog, MCPCallRecord{
		ToolName:  name,
		Args:      args,
		Timestamp: time.Now(),
		Result:    result,
	})
	s.mu.Unlock()

	slog.Info("mock_mcp_execute",
		"op", "mcp.tool.execute",
		"server", s.Name(),
		"tool", name,
		"err", err != nil)

	if err != nil {
		return nil, err
	}
	return &mcp.MCPToolResult{Content: result}, nil
}

// ProtocolVersion implements mcp.MCPClient by returning the latest supported
// protocol version. The mock does not perform a real handshake.
func (s *MockMCPServerImpl) ProtocolVersion() string {
	return mcp.LatestProtocolVersion
}

// CallLog returns a copy of all recorded invocations.
func (s *MockMCPServerImpl) CallLog() []MCPCallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make([]MCPCallRecord, len(s.callLog))
	copy(result, s.callLog)
	return result
}
