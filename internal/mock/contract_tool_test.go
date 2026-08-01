package mock

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/tools"
)

func TestToolContractNoPanic(t *testing.T) {
	server := NewMockToolServer()
	def, err := server.RegisterReadFileTool("content")
	require.NoError(t, err)

	call := tools.ToolCall{Name: def.Name(), Args: map[string]any{"path": "/test/file.go"}}

	assert.NotPanics(t, func() {
		result, err := server.Execute(context.Background(), call)
		if err == nil && result == nil {
			t.Error("unexpected: neither result nor error returned")
		}
	})
}

func TestToolContractResultOrError(t *testing.T) {
	server := NewMockToolServer()
	_, err := server.RegisterBashTool("PASS", 0)
	require.NoError(t, err)

	result, err := server.Execute(context.Background(), tools.ToolCall{Name: "bash"})
	require.NoError(t, err)
	// Contract: Output or Error must be non-empty/non-nil.
	assert.True(t, result.Output != "" || err != nil,
		"expected non-empty output or error")
}

func TestToolContractUnregisteredNoPanic(t *testing.T) {
	server := NewMockToolServer()
	assert.NotPanics(t, func() {
		_, err := server.Execute(context.Background(), tools.ToolCall{Name: "missing"})
		if err == nil {
			t.Error("expected an error for an unknown tool")
		}
	})
}
