package core

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAgentLoop is a test AgentLoop that records whether Run was called.
type fakeAgentLoop struct {
	called  atomic.Int32
	events  []AgentEvent
	runErr  error
}

func (f *fakeAgentLoop) Run(_ context.Context, _ Submission, _ ...EventStream) ([]AgentEvent, error) {
	f.called.Add(1)
	if f.runErr != nil {
		return nil, f.runErr
	}
	return f.events, nil
}

func TestPlanModeMiddleware_SatisfiesInterface(t *testing.T) {
	var _ Middleware = (*PlanModeMiddleware)(nil)
}

func TestPlanModeMiddleware_Name(t *testing.T) {
	m := NewPlanModeMiddleware(NewDefaultPlanModeController())
	assert.Equal(t, "plan-mode", m.Name())
}

func TestPlanModeMiddleware_PassThroughWhenInactive(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	loop := &fakeAgentLoop{events: []AgentEvent{{Kind: "message", Content: "ok"}}}
	mw := NewPlanModeMiddleware(ctrl)
	wrapped := mw.Wrap(loop)

	events, err := wrapped.Run(context.Background(), Submission{
		Type:    SubmissionUserMessage,
		Content: "hello",
		Metadata: map[string]any{
			"tool_calls": []string{"write"},
		},
	})
	require.NoError(t, err)
	assert.Len(t, events, 1)
	assert.Equal(t, 1, int(loop.called.Load()), "wrapped loop should be called when plan mode inactive")
}

func TestPlanModeMiddleware_BlocksWriteToolCalls(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	loop := &fakeAgentLoop{events: []AgentEvent{{Kind: "message", Content: "ok"}}}
	mw := NewPlanModeMiddleware(ctrl)
	wrapped := mw.Wrap(loop)

	events, err := wrapped.Run(context.Background(), Submission{
		Type:    SubmissionUserMessage,
		Content: "write a file",
		Metadata: map[string]any{
			"tool_calls": []string{"write"},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "blocked")
	require.Len(t, events, 1)
	assert.Equal(t, "error", events[0].Kind)
	assert.Contains(t, events[0].Content, "write")
	assert.Equal(t, 0, int(loop.called.Load()), "wrapped loop should NOT be called when write is blocked")
}

func TestPlanModeMiddleware_BlocksMultipleToolCalls(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	loop := &fakeAgentLoop{}
	mw := NewPlanModeMiddleware(ctrl)
	wrapped := mw.Wrap(loop)

	// "edit" appears in the list alongside "read"; the first blocked tool
	// ("edit") should cause the block.
	_, err := wrapped.Run(context.Background(), Submission{
		Metadata: map[string]any{
			"tool_calls": []string{"read", "edit", "grep"},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "edit")
}

func TestPlanModeMiddleware_AllowsReadToolsWhenActive(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	loop := &fakeAgentLoop{events: []AgentEvent{{Kind: "message", Content: "planning"}}}
	mw := NewPlanModeMiddleware(ctrl)
	wrapped := mw.Wrap(loop)

	events, err := wrapped.Run(context.Background(), Submission{
		Metadata: map[string]any{
			"tool_calls": []string{"read", "grep", "ls"},
		},
	})
	require.NoError(t, err)
	assert.Len(t, events, 1)
	assert.Equal(t, 1, int(loop.called.Load()), "wrapped loop should be called for read-only tools")
}

func TestPlanModeMiddleware_BlocksToolNameSingle(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	loop := &fakeAgentLoop{}
	mw := NewPlanModeMiddleware(ctrl)
	wrapped := mw.Wrap(loop)

	_, err := wrapped.Run(context.Background(), Submission{
		Metadata: map[string]any{
			"tool_name": "bash",
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bash")
	assert.Equal(t, 0, int(loop.called.Load()))
}

func TestPlanModeMiddleware_NoMetadata(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	loop := &fakeAgentLoop{events: []AgentEvent{{Kind: "message"}}}
	mw := NewPlanModeMiddleware(ctrl)
	wrapped := mw.Wrap(loop)

	events, err := wrapped.Run(context.Background(), Submission{
		Content: "just thinking",
	})
	require.NoError(t, err)
	assert.Len(t, events, 1)
	assert.Equal(t, 1, int(loop.called.Load()), "no metadata means no tool calls to block")
}

func TestPlanModeMiddleware_ToolCallsAsAnySlice(t *testing.T) {
	ctrl := NewDefaultPlanModeController()
	require.NoError(t, ctrl.Enter(context.Background(), "planning"))
	loop := &fakeAgentLoop{}
	mw := NewPlanModeMiddleware(ctrl)
	wrapped := mw.Wrap(loop)

	_, err := wrapped.Run(context.Background(), Submission{
		Metadata: map[string]any{
			"tool_calls": []any{"read", "mutation"},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mutation")
}
