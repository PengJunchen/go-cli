package tools

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSubagentDispatcher is a tools.SubagentDispatcher stub.
type fakeSubagentDispatcher struct {
	task    SubagentTask
	res     SubagentResult
	err     error
	listing []SubagentTask
}

func (f *fakeSubagentDispatcher) Dispatch(_ context.Context, task SubagentTask) (SubagentResult, error) {
	f.task = task
	return f.res, f.err
}
func (f *fakeSubagentDispatcher) ListRunning() []SubagentTask { return f.listing }

func TestSubagentToolImplementsToolDefinition(t *testing.T) {
	var _ ToolDefinition = (*SubagentTool)(nil)
}

func TestSubagentToolNameAndDescription(t *testing.T) {
	tool := NewSubagentTool(&fakeSubagentDispatcher{})
	assert.Equal(t, "dispatch_subagent", tool.Name())
	assert.NotEmpty(t, tool.Description())
}

func TestSubagentToolExecuteSuccess(t *testing.T) {
	d := &fakeSubagentDispatcher{
		res: SubagentResult{TaskID: "t1", Content: "final answer", Duration: 3 * time.Millisecond},
	}
	tool := NewSubagentTool(d)

	res, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"prompt":    "summarize",
			"id":        "t1",
			"tools":     []string{"bash"},
			"max_turns": 4,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "final answer", res.Output)
	assert.Equal(t, "t1", res.Metadata["task_id"])
	assert.Equal(t, 3*time.Millisecond, res.Metadata["duration"])

	assert.Equal(t, "t1", d.task.ID)
	assert.Equal(t, "summarize", d.task.Prompt)
	assert.Equal(t, []string{"bash"}, d.task.Tools)
	assert.Equal(t, 4, d.task.MaxTurns)
}

func TestSubagentToolExecuteParsesJSONShapes(t *testing.T) {
	d := &fakeSubagentDispatcher{res: SubagentResult{Content: "ok"}}
	tool := NewSubagentTool(d)

	// JSON-decoded shapes: tools as []any, max_turns as float64.
	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"prompt":    "hi",
			"tools":     []any{"a", "b"},
			"max_turns": float64(7),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, d.task.Tools)
	assert.Equal(t, 7, d.task.MaxTurns)
	assert.NotEmpty(t, d.task.ID, "id should be generated when omitted")
}

func TestSubagentToolExecuteMissingPrompt(t *testing.T) {
	tool := NewSubagentTool(&fakeSubagentDispatcher{})
	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{}})
	require.Error(t, err)

	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"prompt": ""}})
	require.Error(t, err)

	// non-string prompt is rejected too
	_, err = tool.Execute(context.Background(), ToolCall{Args: map[string]any{"prompt": 123}})
	require.Error(t, err)
}

func TestSubagentToolExecuteDispatchError(t *testing.T) {
	d := &fakeSubagentDispatcher{err: errors.New("boom")}
	tool := NewSubagentTool(d)

	_, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"prompt": "x"}})
	require.Error(t, err)
}

func TestSubagentToolExecuteResultError(t *testing.T) {
	d := &fakeSubagentDispatcher{
		res: SubagentResult{Content: "partial", Error: errors.New("sub failed")},
	}
	tool := NewSubagentTool(d)

	res, err := tool.Execute(context.Background(), ToolCall{Args: map[string]any{"prompt": "x"}})
	require.Error(t, err)
	assert.Equal(t, "partial", res.Output)
	assert.Equal(t, "sub failed", res.Metadata["error"])
}

func TestSubagentToolListRunningPassThrough(t *testing.T) {
	d := &fakeSubagentDispatcher{listing: []SubagentTask{{ID: "a"}, {ID: "b"}}}
	tool := NewSubagentTool(d)
	assert.Len(t, tool.dispatcher.ListRunning(), 2)
}

func TestSubagentToolExecuteParsesSystemPrompt(t *testing.T) {
	d := &fakeSubagentDispatcher{res: SubagentResult{Content: "ok"}}
	tool := NewSubagentTool(d)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"prompt":        "do research",
			"system_prompt": "you are a researcher",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "you are a researcher", d.task.SystemPrompt)
	assert.Empty(t, d.task.Role, "role must not be set when only system_prompt is given")
}

func TestSubagentToolExecuteParsesRole(t *testing.T) {
	d := &fakeSubagentDispatcher{res: SubagentResult{Content: "ok"}}
	tool := NewSubagentTool(d)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"prompt": "review this",
			"role":   "reviewer",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "reviewer", d.task.Role)
	assert.Empty(t, d.task.SystemPrompt, "system_prompt must remain empty so the dispatcher applies the role template")
}

func TestSubagentToolExecuteSystemPromptAndRoleBothParsed(t *testing.T) {
	d := &fakeSubagentDispatcher{res: SubagentResult{Content: "ok"}}
	tool := NewSubagentTool(d)

	_, err := tool.Execute(context.Background(), ToolCall{
		Args: map[string]any{
			"prompt":        "go",
			"system_prompt": "explicit",
			"role":          "tester",
		},
	})
	require.NoError(t, err)

	assert.Equal(t, "explicit", d.task.SystemPrompt)
	assert.Equal(t, "tester", d.task.Role)
}

func TestSubagentToolDescriptionIncludesGuidance(t *testing.T) {
	tool := NewSubagentTool(&fakeSubagentDispatcher{})
	desc := tool.Description()

	assert.Contains(t, desc, "sub-agent")
	assert.Contains(t, desc, "system_prompt")
	assert.Contains(t, desc, "role")
	assert.Contains(t, desc, "researcher, implementer, reviewer, tester")
	assert.Contains(t, desc, "return its result")
}
