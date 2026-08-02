package mcp

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

// fakeClient is a minimal in-process MCPClient used to exercise MCPToolAdapter
// end-to-end. It mirrors the behavior of the mock server (server/tool names,
// argument-aware results) without importing internal/mock.
type fakeClient struct {
	name    string
	mu      sync.Mutex
	last    string
	errored bool
}

func (f *fakeClient) Connect(context.Context) error    { return nil }
func (f *fakeClient) Disconnect(context.Context) error { return nil }
func (f *fakeClient) ListTools(context.Context) ([]MCPTool, error) {
	return []MCPTool{{Name: "echo", Description: "echoes a message"}}, nil
}

func (f *fakeClient) CallTool(_ context.Context, name string, args map[string]any) (*MCPToolResult, error) {
	f.mu.Lock()
	f.last = name
	f.mu.Unlock()
	if f.errored {
		return nil, assert.AnError
	}
	return &MCPToolResult{Content: "echo:" + stringOf(args["msg"])}, nil
}

func (f *fakeClient) Name() string { return f.name }

func TestMCPToolAdapterNameNormalized(t *testing.T) {
	client := &fakeClient{name: "srv"}
	adapter := NewMCPToolAdapter(client, MCPTool{Name: "echo", Description: "echoes a message"})

	assert.Equal(t, "mcp__srv__echo", adapter.Name())
	assert.Equal(t, "echoes a message", adapter.Description())
}

func TestMCPToolAdapterExecuteEndToEnd(t *testing.T) {
	client := &fakeClient{name: "srv"}
	adapter := NewMCPToolAdapter(client, MCPTool{Name: "echo", Description: "echoes a message"})

	result, err := adapter.Execute(context.Background(), tools.ToolCall{
		ID:   "call-1",
		Name: "mcp__srv__echo",
		Args: map[string]any{"msg": "hi"},
	})
	require.NoError(t, err)
	assert.Equal(t, "echo:hi", result.Output)
	assert.Equal(t, "call-1", result.ToolCallID)

	client.mu.Lock()
	assert.Equal(t, "echo", client.last)
	client.mu.Unlock()
}

func TestMCPToolAdapterExecutePropagatesError(t *testing.T) {
	client := &fakeClient{name: "srv", errored: true}
	adapter := NewMCPToolAdapter(client, MCPTool{Name: "echo", Description: "echoes a message"})

	_, err := adapter.Execute(context.Background(), tools.ToolCall{
		ID:   "call-2",
		Name: "mcp__srv__echo",
		Args: map[string]any{"msg": "hi"},
	})
	require.Error(t, err)
}

func TestMCPToolAdapterThroughRegistry(t *testing.T) {
	client := &fakeClient{name: "srv"}
	adapter := NewMCPToolAdapter(client, MCPTool{Name: "echo", Description: "echoes a message"})

	reg, ok := tools.NewDefaultToolRegistry().(*tools.DefaultToolRegistry)
	require.True(t, ok)
	ctx := context.Background()
	require.NoError(t, reg.Register(ctx, adapter))

	// The adapter is findable by its normalized name.
	def, err := reg.Get(ctx, "mcp__srv__echo")
	require.NoError(t, err)
	assert.Equal(t, adapter, def)

	// Executing through the registry returns the correct result.
	call := tools.ToolCall{ID: "call-3", Name: "mcp__srv__echo", Args: map[string]any{"msg": "world"}}
	result, err := reg.Execute(ctx, call)
	require.NoError(t, err)
	assert.Equal(t, "echo:world", result.Output)
	assert.Equal(t, "call-3", result.ToolCallID)
}
