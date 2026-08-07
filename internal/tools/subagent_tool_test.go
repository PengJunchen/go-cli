package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubagentToolImplementsParameterized(t *testing.T) {
	tool := NewSubagentTool(nil)
	_, ok := any(tool).(Parameterized)
	assert.True(t, ok, "SubagentTool must implement Parameterized")
}

func TestSubagentToolImplementsPromptGuideliner(t *testing.T) {
	tool := NewSubagentTool(nil)
	_, ok := any(tool).(PromptGuideliner)
	assert.True(t, ok, "SubagentTool must implement PromptGuideliner")
}

func TestSubagentToolParametersSchema(t *testing.T) {
	tool := NewSubagentTool(nil)
	schema := tool.Parameters()

	m, ok := schema.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "object", m["type"])

	props, ok := m["properties"].(map[string]any)
	require.True(t, ok)

	// Verify all expected properties exist.
	expectedProps := []string{"prompt", "role", "tools", "model", "max_turns", "parallel", "tasks"}
	for _, p := range expectedProps {
		assert.Contains(t, props, p, "schema should include property %q", p)
	}

	// Verify role enum.
	roleProp, ok := props["role"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []string{"researcher", "implementer", "reviewer", "tester"}, roleProp["enum"])

	// Verify required includes prompt.
	required, ok := m["required"].([]string)
	require.True(t, ok)
	assert.Contains(t, required, "prompt")
}

func TestSubagentToolPromptGuidelines(t *testing.T) {
	tool := NewSubagentTool(nil)
	guidelines := tool.PromptGuidelines()
	assert.NotEmpty(t, guidelines)
	// Each guideline should mention dispatch_subagent or sub-agent.
	for _, g := range guidelines {
		assert.NotEmpty(t, g)
	}
}

// fakeDispatcher is a tools.SubagentDispatcher stub for testing.
type fakeToolDispatcher struct {
	parallelDispatched bool
	singleDispatched   bool
	tasks              []SubagentTask
	results            []SubagentResult
	err                error
}

func (f *fakeToolDispatcher) Dispatch(_ context.Context, task SubagentTask) (SubagentResult, error) {
	f.singleDispatched = true
	f.tasks = []SubagentTask{task}
	if len(f.results) > 0 {
		return f.results[0], f.err
	}
	return SubagentResult{TaskID: task.ID, Content: "single-result"}, f.err
}

func (f *fakeToolDispatcher) ParallelDispatch(_ context.Context, tasks []SubagentTask) ([]SubagentResult, error) {
	f.parallelDispatched = true
	f.tasks = tasks
	if len(f.results) > 0 {
		return f.results, f.err
	}
	results := make([]SubagentResult, len(tasks))
	for i, task := range tasks {
		results[i] = SubagentResult{TaskID: task.ID, Content: "parallel-result-" + task.ID}
	}
	return results, f.err
}

func (f *fakeToolDispatcher) ListRunning() []SubagentTask { return nil }

func TestSubagentToolParallelDispatchWithTasksArray(t *testing.T) {
	fd := &fakeToolDispatcher{}
	tool := NewSubagentTool(fd)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"tasks": []any{
				map[string]any{"prompt": "task1", "role": "researcher"},
				map[string]any{"prompt": "task2", "role": "implementer"},
			},
		},
	})
	require.NoError(t, err)
	assert.True(t, fd.parallelDispatched)
	assert.False(t, fd.singleDispatched)
	assert.Contains(t, res.Output, "Task 1 (researcher):")
	assert.Contains(t, res.Output, "Task 2 (implementer):")
	parallel, ok := res.Metadata["parallel"].(bool)
	assert.True(t, ok && parallel)
}

func TestSubagentToolParallelDispatchWithParallelFlag(t *testing.T) {
	fd := &fakeToolDispatcher{}
	tool := NewSubagentTool(fd)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"prompt":   "do something",
			"parallel": true,
		},
	})
	require.NoError(t, err)
	assert.True(t, fd.parallelDispatched)
	assert.Contains(t, res.Output, "Task 1")
}

func TestSubagentToolSingleDispatchWhenNotParallel(t *testing.T) {
	fd := &fakeToolDispatcher{}
	tool := NewSubagentTool(fd)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"prompt": "do something",
		},
	})
	require.NoError(t, err)
	assert.True(t, fd.singleDispatched)
	assert.False(t, fd.parallelDispatched)
	assert.Equal(t, "single-result", res.Output)
}

func TestSubagentToolParallelDispatchErrorPropagation(t *testing.T) {
	fd := &fakeToolDispatcher{err: errors.New("dispatch failed")}
	tool := NewSubagentTool(fd)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"tasks": []any{
				map[string]any{"prompt": "task1"},
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parallel dispatch")
}

func TestSubagentToolParallelDispatchEmptyTasks(t *testing.T) {
	fd := &fakeToolDispatcher{}
	tool := NewSubagentTool(fd)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"parallel": true,
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one task")
}
