package mock

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// MockTool implements a single registered mock tool. Its handler produces the
// canned result for a call.
type MockTool struct {
	// Definition describes the tool (name/description).
	Definition tools.ToolDefinition
	// Handler produces the tool result for a call.
	Handler func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)
}

// ToolCallRecord logs one tool execution.
type ToolCallRecord struct {
	// ToolName is the name of the invoked tool.
	ToolName string
	// Args are the arguments supplied to the call.
	Args map[string]any
	// Result is the returned result, or nil on error.
	Result *tools.ToolResult
	// Error records a handler error, if any.
	Error error
	// Duration is how long the handler took.
	Duration time.Duration
}

// MockToolServer simulates a tool registry. It implements the
// tools.ToolRegistry contract and additionally exposes an Execute method so
// tests can drive tool calls directly. Every execution is logged for
// assertions.
type MockToolServer struct {
	mu      sync.Mutex
	tools   map[string]*MockTool
	callLog []ToolCallRecord
}

// Compile-time assertion that the mock server satisfies the tool contract.
var _ tools.ToolRegistry = (*MockToolServer)(nil)

// NewMockToolServer creates an empty mock tool server.
func NewMockToolServer() *MockToolServer {
	return &MockToolServer{tools: make(map[string]*MockTool)}
}

// Register implements tools.ToolRegistry by wrapping def as a mock tool whose
// handler returns a canned result.
func (s *MockToolServer) Register(_ context.Context, def tools.ToolDefinition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[def.Name()] = &MockTool{
		Definition: def,
		Handler: func(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
			return &tools.ToolResult{Output: "mock result"}, nil
		},
	}
	return nil
}

// RegisterMockTool registers a named tool backed by the given handler. It
// returns the ToolDefinition so tools.ToolRegistry.Register can be called
// with it as well.
func (s *MockToolServer) RegisterMockTool(name string, handler func(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error)) (tools.ToolDefinition, error) {
	if handler == nil {
		return nil, fmt.Errorf("tool %q: nil handler", name)
	}
	def := &simpleToolDef{name: name, description: "mock tool: " + name}
	s.mu.Lock()
	s.tools[name] = &MockTool{Definition: def, Handler: handler}
	s.mu.Unlock()
	return def, nil
}

// RegisterReadFileTool registers a built-in read_file mock tool returning the
// given content. It returns the ToolDefinition so the registry contract can be
// exercised.
func (s *MockToolServer) RegisterReadFileTool(content string) (tools.ToolDefinition, error) {
	return s.RegisterMockTool("read_file", func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: content}, nil
	})
}

// RegisterBashTool registers a built-in bash mock tool returning the given
// output and exit code. It returns the ToolDefinition so the registry contract
// can be exercised.
func (s *MockToolServer) RegisterBashTool(output string, exitCode int) (tools.ToolDefinition, error) {
	return s.RegisterMockTool("bash", func(_ context.Context, _ tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{
			Output:   output,
			Metadata: map[string]any{"exit_code": exitCode},
		}, nil
	})
}

// Get implements tools.ToolRegistry.
func (s *MockToolServer) Get(_ context.Context, name string) (tools.ToolDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tool, ok := s.tools[name]
	if !ok {
		return nil, fmt.Errorf("tool not found: %s", name)
	}
	return tool.Definition, nil
}

// List implements tools.ToolRegistry.
func (s *MockToolServer) List(_ context.Context) ([]tools.ToolDefinition, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]tools.ToolDefinition, 0, len(s.tools))
	for _, tool := range s.tools {
		result = append(result, tool.Definition)
	}
	return result, nil
}

// Execute runs a tool call against the registered tool and logs it. This
// method is provided for tests driving tool calls directly; it is not part of
// the ToolRegistry interface.
func (s *MockToolServer) Execute(ctx context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
	start := time.Now()

	s.mu.Lock()
	tool, ok := s.tools[call.Name]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("tool not found: %s", call.Name)
	}

	result, err := tool.Handler(ctx, call)
	duration := time.Since(start)

	s.mu.Lock()
	s.callLog = append(s.callLog, ToolCallRecord{
		ToolName: call.Name,
		Args:     call.Args,
		Result:   result,
		Error:    err,
		Duration: duration,
	})
	s.mu.Unlock()

	slog.Info("mock_tool_execute",
		"op", "tool.execute",
		"provider", "mock",
		"tool_name", call.Name,
		"duration_ms", duration.Milliseconds(),
		"err", err != nil,
	)
	return result, err
}

// CallLog returns a copy of all recorded tool calls.
func (s *MockToolServer) CallLog() []ToolCallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]ToolCallRecord, len(s.callLog))
	copy(result, s.callLog)
	return result
}

// simpleToolDef is the minimal tools.ToolDefinition implementation used for
// mock tools.
type simpleToolDef struct {
	name        string
	description string
}

func (d *simpleToolDef) Name() string        { return d.name }
func (d *simpleToolDef) Description() string { return d.description }
func (d *simpleToolDef) Execute(context.Context, tools.ToolCall) (*tools.ToolResult, error) {
	return &tools.ToolResult{Output: "mock"}, nil
}

var _ tools.ToolDefinition = (*simpleToolDef)(nil)
