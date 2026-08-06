package mock

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestMockToolServerRegisterAndExecute(t *testing.T) {
	server := NewMockToolServer()

	def, err := server.RegisterMockTool("echo", func(_ context.Context, call tools.ToolCall) (*tools.ToolResult, error) {
		return &tools.ToolResult{Output: fmt.Sprint(call.Args["text"])}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, "echo", def.Name())

	result, err := server.Execute(context.Background(), tools.ToolCall{
		Name: "echo",
		Args: map[string]any{"text": "hello"},
	})
	require.NoError(t, err)
	assert.Contains(t, result.Output, "hello")
}

func TestMockToolServerReadFileConvenience(t *testing.T) {
	server := NewMockToolServer()
	def, err := server.RegisterReadFileTool("package main")
	require.NoError(t, err)
	assert.Equal(t, "read_file", def.Name())

	result, err := server.Execute(context.Background(), tools.ToolCall{Name: "read_file"})
	require.NoError(t, err)
	assert.Equal(t, "package main", result.Output)
}

func TestMockToolServerBashConvenience(t *testing.T) {
	server := NewMockToolServer()
	def, err := server.RegisterBashTool("PASS", 0)
	require.NoError(t, err)
	assert.Equal(t, "bash", def.Name())

	result, err := server.Execute(context.Background(), tools.ToolCall{Name: "bash"})
	require.NoError(t, err)
	assert.Equal(t, "PASS", result.Output)
	assert.Equal(t, 0, result.Metadata["exit_code"])
}

func TestMockToolServerRegistryContract(t *testing.T) {
	server := NewMockToolServer()

	// Exercise the ToolRegistry interface directly.
	err := server.Register(context.Background(), &simpleToolDef{name: "reg", description: "registered"})
	require.NoError(t, err)

	got, err := server.Get(context.Background(), "reg")
	require.NoError(t, err)
	assert.Equal(t, "reg", got.Name())

	list, err := server.List(context.Background())
	require.NoError(t, err)
	assert.NotEmpty(t, list)
}

func TestMockToolServerCallLog(t *testing.T) {
	server := NewMockToolServer()
	_, err := server.RegisterReadFileTool("data")
	require.NoError(t, err)

	args := map[string]any{"path": "/file"}
	_, err = server.Execute(context.Background(), tools.ToolCall{Name: "read_file", Args: args})
	require.NoError(t, err)

	log := server.CallLog()
	require.Len(t, log, 1)
	assert.Equal(t, "read_file", log[0].ToolName)
	assert.Equal(t, args, log[0].Args)
	assert.NotNil(t, log[0].Result)
	assert.GreaterOrEqual(t, log[0].Duration.Nanoseconds(), int64(0))
}

func TestMockToolServerUnregisteredTool(t *testing.T) {
	server := NewMockToolServer()

	_, err := server.Execute(context.Background(), tools.ToolCall{Name: "missing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")

	_, err = server.Get(context.Background(), "missing")
	require.Error(t, err)
}

func TestMockToolServerNoPanic(t *testing.T) {
	server := NewMockToolServer()
	_, err := server.RegisterReadFileTool("content")
	require.NoError(t, err)

	// Execute an unregistered tool must not panic.
	assert.NotPanics(t, func() {
		_, err := server.Execute(context.Background(), tools.ToolCall{Name: "nope"})
		if err == nil {
			t.Error("expected error for unregistered tool")
		}
	})
}

func TestMockToolServerNilHandlerRejected(t *testing.T) {
	server := NewMockToolServer()
	_, err := server.RegisterMockTool("bad", nil)
	require.Error(t, err)
}
