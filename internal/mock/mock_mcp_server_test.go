package mock

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMockMCPServerName_Empty(t *testing.T) {
	s := NewMockMCPServer("")
	assert.Equal(t, "mock", s.Name())
}

func TestMockMCPServerName_Custom(t *testing.T) {
	s := NewMockMCPServer("github")
	assert.Equal(t, "github", s.Name())
}

func TestMockMCPServerStart(t *testing.T) {
	s := NewMockMCPServer("test")
	err := s.Start(context.Background())
	require.NoError(t, err)
}

func TestMockMCPServerStop(t *testing.T) {
	s := NewMockMCPServer("test")
	err := s.Stop(context.Background())
	require.NoError(t, err)
}

func TestMockMCPServerConnect(t *testing.T) {
	s := NewMockMCPServer("test")
	err := s.Connect(context.Background())
	require.NoError(t, err)
}

func TestMockMCPServerDisconnect(t *testing.T) {
	s := NewMockMCPServer("test")
	err := s.Disconnect(context.Background())
	require.NoError(t, err)
}

func TestMockMCPServerRegisterTool_NilHandler(t *testing.T) {
	s := NewMockMCPServer("test")
	s.RegisterTool("echo", "echoes", nil)
	// Should not panic; nil handler is replaced with a no-op.
	tools, err := s.ListTools(context.Background())
	require.NoError(t, err)
	require.Len(t, tools, 1)
	assert.Equal(t, "echo", tools[0].Name)
}

func TestMockMCPServerCallTool_NotFound(t *testing.T) {
	s := NewMockMCPServer("test")
	_, err := s.CallTool(context.Background(), "missing", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestMockMCPServerCallTool_Success(t *testing.T) {
	s := NewMockMCPServer("test")
	s.RegisterTool("echo", "echoes", func(args map[string]any) (any, error) {
		msg := fmt.Sprintf("%v", args["msg"]) //nolint:errcheck
		return "echo:" + msg, nil
	})
	result, err := s.CallTool(context.Background(), "echo", map[string]any{"msg": "hi"})
	require.NoError(t, err)
	assert.Equal(t, "echo:hi", result.Content)
	assert.False(t, result.IsError)
}

func TestMockMCPServerCallTool_HandlerError(t *testing.T) {
	s := NewMockMCPServer("test")
	s.RegisterTool("fail", "always fails", func(args map[string]any) (any, error) {
		return nil, assert.AnError
	})
	_, err := s.CallTool(context.Background(), "fail", nil)
	require.Error(t, err)
}

func TestMockMCPServerCallLog(t *testing.T) {
	s := NewMockMCPServer("test")
	s.RegisterTool("echo", "echoes", func(args map[string]any) (any, error) {
		return "ok", nil
	})

	// Initially empty.
	log := s.CallLog()
	assert.Len(t, log, 0)

	_, err := s.CallTool(context.Background(), "echo", map[string]any{"x": 1})
	require.NoError(t, err)

	log = s.CallLog()
	require.Len(t, log, 1)
	assert.Equal(t, "echo", log[0].ToolName)
	assert.Equal(t, map[string]any{"x": 1}, log[0].Args)
	assert.Equal(t, "ok", log[0].Result)
	assert.False(t, log[0].Timestamp.IsZero())
}

func TestMockMCPServerListTools(t *testing.T) {
	s := NewMockMCPServer("test")
	s.RegisterTool("a", "tool a", nil)
	s.RegisterTool("b", "tool b", nil)

	tools, err := s.ListTools(context.Background())
	require.NoError(t, err)
	assert.Len(t, tools, 2)
	names := map[string]bool{}
	for _, t := range tools {
		names[t.Name] = true
	}
	assert.True(t, names["a"])
	assert.True(t, names["b"])
}
