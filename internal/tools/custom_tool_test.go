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

// TestCustomCommandToolNameDescription verifies the configured name and
// description are surfaced, and that the tool satisfies the Parameterized
// interface with a single "input" string parameter.
func TestCustomCommandToolNameDescription(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewCustomCommandTool("my_tool", "does a thing", []string{"echo"}, nil, nil, 0, "")

	assert.Equal(t, "my_tool", tool.Name())
	assert.Equal(t, "does a thing", tool.Description())

	var _ Parameterized = tool
	params, ok := tool.Parameters().(map[string]any)
	require.True(t, ok)
	props, ok := params["properties"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, props, "input")
}

// TestCustomCommandToolEcho runs a simple echo command with a dynamic input
// argument and asserts the captured stdout is returned.
func TestCustomCommandToolEcho(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewCustomCommandTool("echoer", "", []string{"echo"}, nil, nil, 0, "")

	res, err := tool.Execute(context.Background(), ToolCall{
		ID:   "c1",
		Args: map[string]any{"input": "hello"},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Equal(t, "hello\n", res.Output)
	assert.Equal(t, "c1", res.ToolCallID)
	assert.Equal(t, 0, res.Metadata["exit_code"])
}

// TestCustomCommandToolStaticArgs verifies that static args are appended
// after the base command args and before the dynamic input.
func TestCustomCommandToolStaticArgs(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	// command[1:] = ["first"] (base args), staticArgs = ["second"], input = "third".
	tool := NewCustomCommandTool("baseargs", "", []string{"echo", "first"}, []string{"second"}, nil, 0, "")

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"input": "third"},
	})
	require.NoError(t, err)
	assert.Equal(t, "first second third\n", res.Output)
}

// TestCustomCommandToolEnv verifies configured environment variables are
// exposed to the spawned command.
func TestCustomCommandToolEnv(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewCustomCommandTool("envtool", "", []string{"printenv"}, nil,
		map[string]string{"CUSTOM_TOOL_TEST_VAR": "env-value-123"}, 0, "")

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"input": "CUSTOM_TOOL_TEST_VAR"},
	})
	require.NoError(t, err)
	assert.Equal(t, "env-value-123\n", res.Output)
}

// TestCustomCommandToolTimeout verifies that a configured timeout cancels a
// long-running command and returns an error.
func TestCustomCommandToolTimeout(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewCustomCommandTool("sleeper", "", []string{"sleep"}, nil, nil, 1*time.Second, "")

	start := time.Now()
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"input": "5"},
	})
	elapsed := time.Since(start)
	assert.Error(t, err)
	// The command should have been cancelled well before the 5s sleep finished.
	assert.Less(t, elapsed, 4*time.Second)
}

// TestCustomCommandToolWorkingDir verifies the command runs in the configured
// working directory.
func TestCustomCommandToolWorkingDir(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	dir := t.TempDir()
	tool := NewCustomCommandTool("pwdtool", "", []string{"pwd"}, nil, nil, 0, dir)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{},
	})
	require.NoError(t, err)
	assert.Contains(t, strings.TrimSpace(res.Output), dir)
}

// TestCustomCommandToolNotFound verifies that a non-existent executable returns
// an error.
func TestCustomCommandToolNotFound(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewCustomCommandTool("missing", "", []string{"definitely_not_a_real_command_xyz_12345"}, nil, nil, 0, "")

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"input": "foo"},
	})
	assert.Error(t, err)
}

// TestCustomCommandToolOutputLimit verifies that output beyond the configured
// limit is truncated with a marker.
func TestCustomCommandToolOutputLimit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewCustomCommandTool("echolong", "", []string{"echo"}, nil, nil, 0, "")
	// White-box override of the internal output cap.
	tool.maxOutput = 10

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"input": strings.Repeat("x", 100)},
	})
	require.NoError(t, err)
	assert.Contains(t, res.Output, "[output truncated]")
	assert.Less(t, len(res.Output), 100)
}

// TestCustomCommandToolNonZeroExit verifies that a command exiting non-zero
// returns an error while still surfacing captured output and the exit code.
func TestCustomCommandToolNonZeroExit(t *testing.T) {
	defer verify.AssertNoGoroutineLeak(t)()

	tool := NewCustomCommandTool("failer", "", []string{"sh", "-c"}, nil, nil, 0, "")

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{"input": "echo oops; exit 3"},
	})
	require.Error(t, err)
	require.NotNil(t, res)
	assert.Contains(t, res.Output, "oops")
	assert.Equal(t, 3, res.Metadata["exit_code"])
}
