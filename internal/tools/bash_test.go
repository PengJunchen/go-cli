package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pengjunchen/go-cli/internal/verify"
)

func TestBashEcho(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewBashTool()
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo hi"},
	})
	require.NoError(t, err)
	assert.Equal(t, "hi\n", res.Output)
	assert.Equal(t, 0, res.Metadata["exit_code"])
}

func TestBashEnvPassed(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewBashTool(WithEnv(map[string]string{"FOO": "bar"}))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo $FOO"},
	})
	require.NoError(t, err)
	assert.Equal(t, "bar\n", res.Output)
}

func TestBashWorkdir(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewBashTool(WithBashWorkdir(dir))
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "pwd"},
	})
	require.NoError(t, err)
	assert.Contains(t, strings.TrimSpace(res.Output), dir)
}

func TestBashMissingCommand(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewBashTool()
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	assert.Error(t, err)

	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"command": 5}})
	assert.Error(t, err)
}

func TestBashCommandNotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewBashTool()
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "definitely_not_a_real_command_xyz"},
	})
	assert.Error(t, err)
	assert.NotNil(t, res) // partial output/metadata still returned
}

func TestBashContextCancel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	tool := NewBashTool(WithTimeout(10 * time.Second))

	cancel()

	_, err := tool.Execute(ctx, ToolCall{
		Args: map[string]any{"command": "echo should not run"},
	})
	assert.Error(t, err)
}

func TestBashTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewBashTool(WithTimeout(200 * time.Millisecond))
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "sleep 2"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "timed out")
}

func TestBashName(t *testing.T) {
	tool := NewBashTool()
	assert.Equal(t, "bash", tool.Name())
}
