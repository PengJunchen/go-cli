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

	tool := NewBashTool(WithNoSandbox())
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo hi"},
	})
	require.NoError(t, err)
	assert.Equal(t, "hi\n", res.Output)
	assert.Equal(t, 0, res.Metadata["exit_code"])
}

func TestBashEnvPassed(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewBashTool(WithEnv(map[string]string{"FOO": "bar"}), WithNoSandbox())
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo $FOO"},
	})
	require.NoError(t, err)
	assert.Equal(t, "bar\n", res.Output)
}

func TestBashWorkdir(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewBashTool(WithBashWorkdir(dir), WithNoSandbox())
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

	tool := NewBashTool(WithNoSandbox())
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "definitely_not_a_real_command_xyz"},
	})
	assert.Error(t, err)
	assert.NotNil(t, res) // partial output/metadata still returned
}

func TestBashContextCancel(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	ctx, cancel := context.WithCancel(context.Background())
	tool := NewBashTool(WithTimeout(10*time.Second), WithNoSandbox())

	cancel()

	_, err := tool.Execute(ctx, ToolCall{
		Args: map[string]any{"command": "echo should not run"},
	})
	assert.Error(t, err)
}

func TestBashTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewBashTool(WithTimeout(200*time.Millisecond), WithNoSandbox())
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

func TestBashDescription(t *testing.T) {
	tool := NewBashTool()
	assert.Contains(t, tool.Description(), "bash")
	assert.Contains(t, tool.Description(), "shell command")
}

func TestBashWithMaxOutput(t *testing.T) {
	tool := NewBashTool(WithMaxOutput(10))
	assert.Equal(t, 10, tool.MaxOutput)
}

func TestBashWithTimeout(t *testing.T) {
	tool := NewBashTool(WithTimeout(5 * time.Second))
	assert.Equal(t, 5*time.Second, tool.Timeout)
}

func TestBashWithEnv(t *testing.T) {
	tool := NewBashTool(WithEnv(map[string]string{"KEY": "val"}))
	assert.Equal(t, "val", tool.Env["KEY"])
}

func TestBashWithEnvMerges(t *testing.T) {
	tool := NewBashTool(
		WithEnv(map[string]string{"A": "1"}),
		WithEnv(map[string]string{"B": "2"}),
	)
	assert.Equal(t, "1", tool.Env["A"])
	assert.Equal(t, "2", tool.Env["B"])
}

func TestBashOutputTruncation(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// Use a very small max output to force truncation.
	tool := NewBashTool(WithMaxOutput(20), WithNoSandbox())
	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"command": "echo 012345678901234567890123456789"},
	})
	// Truncation returns a timeout-style error but still provides partial output.
	if err != nil {
		// Error is expected when output exceeds the limit.
		assert.Contains(t, res.Output, "[output truncated]")
	} else {
		// If no error, the output should contain the truncation marker.
		assert.Contains(t, res.Output, "[output truncated]")
	}
}
