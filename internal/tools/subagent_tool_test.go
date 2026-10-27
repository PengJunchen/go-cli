package tools

import (
	"context"
	"errors"
	"strings"
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

	// Conditional prompt/tasks requirements are enforced by Execute because the
	// prepared schema supports no union keywords.
	required, ok := m["required"].([]string)
	require.True(t, ok)
	assert.Empty(t, required)
}

func TestSubagentToolPromptGuidelines(t *testing.T) {
	tool := NewSubagentTool(nil)
	guidelines := tool.PromptGuidelines()
	assert.NotEmpty(t, guidelines)
	assert.Contains(t, tool.Description(), "physical_session_id")
	assert.Contains(t, tool.Description(), "--resume-session")
	assert.Contains(t, strings.Join(guidelines, "\n"), "physical_session_id")
	assert.Contains(t, strings.Join(guidelines, "\n"), "--resume-session")
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

func TestSubagentToolOutputExposesResumableChildSessions(t *testing.T) {
	for _, parallel := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			dispatcher := &fakeToolDispatcher{results: []SubagentResult{{TaskID: "first", Content: "first answer", PhysicalSessionID: "s_first"}}}
			args := map[string]any{"prompt": "task"}
			if parallel {
				args = map[string]any{"tasks": []any{map[string]any{"prompt": "first"}, map[string]any{"prompt": "second"}}}
				dispatcher.results = append(dispatcher.results, SubagentResult{TaskID: "second", Content: "second answer", PhysicalSessionID: "s_second"})
			}
			if failed {
				dispatcher.err = errors.New("child failure")
			}
			result, err := NewSubagentTool(dispatcher).Execute(t.Context(), ToolCall{Args: args})
			require.NotNil(t, result)
			require.Contains(t, result.Output, "s_first")
			if parallel {
				require.Contains(t, result.Output, "s_second")
				if !failed {
					require.Contains(t, result.Output, "Task 1 (default): first answer\nChild physical session ID (physical_session_id): s_first")
					require.Contains(t, result.Output, "Task 2 (default): second answer\nChild physical session ID (physical_session_id): s_second")
				}
			}
			if failed {
				require.ErrorContains(t, err, "s_first")
				if parallel {
					require.ErrorContains(t, err, "s_second")
				}
			} else {
				require.NoError(t, err)
			}
		}
	}
}
