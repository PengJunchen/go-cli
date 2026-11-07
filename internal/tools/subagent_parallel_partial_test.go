package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type parallelPartialUnknownError struct{ cause error }

func (e *parallelPartialUnknownError) Error() string      { return e.cause.Error() }
func (e *parallelPartialUnknownError) Unwrap() error      { return e.cause }
func (*parallelPartialUnknownError) OutcomeUnknown() bool { return true }

func TestSubagentParallelFailurePreservesCompletedResults(t *testing.T) {
	cause := &parallelPartialUnknownError{cause: context.Canceled}
	dispatcher := &fakeToolDispatcher{
		err: cause,
		results: []SubagentResult{
			{TaskID: "done", Content: "completed-sibling-result", PhysicalSessionID: "s_done"},
			{TaskID: "failed", Content: "failed-child-output", PhysicalSessionID: "s_failed", Error: cause},
		},
	}
	result, err := NewSubagentTool(dispatcher).Execute(context.Background(), ToolCall{Args: map[string]any{
		"tasks": []any{
			map[string]any{"id": "done", "prompt": "first"},
			map[string]any{"id": "failed", "prompt": "second"},
		},
	}})

	require.ErrorIs(t, err, context.Canceled)
	var unknown interface{ OutcomeUnknown() bool }
	require.ErrorAs(t, err, &unknown)
	require.True(t, unknown.OutcomeUnknown())
	require.NotNil(t, result)
	require.Contains(t, result.Output, "s_done")
	require.Contains(t, result.Output, "s_failed")
	require.Contains(t, err.Error(), "completed-sibling-result")
	require.NotContains(t, err.Error(), "failed-child-output")
	contentAt := strings.Index(err.Error(), "completed-sibling-result")
	require.Less(t, strings.Index(err.Error(), "s_done"), contentAt)
	require.Less(t, strings.Index(err.Error(), "s_failed"), contentAt)
}

func TestSubagentParallelFailureOmitsUncorrelatedAndIncompleteResults(t *testing.T) {
	cause := errors.New("synthetic dispatch failure")
	dispatcher := &fakeToolDispatcher{err: cause, results: []SubagentResult{
		{TaskID: "kept", Content: "trusted-completed"},
		{TaskID: "unexpected", Content: "wrong-task-output"},
		{TaskID: "empty", Content: "   "},
		{Content: "unstarted-output"},
		{TaskID: "duplicate", Content: "duplicate-first"},
		{TaskID: "duplicate", Content: "duplicate-second"},
		{TaskID: "extra", Content: "extra-output"},
	}}
	_, err := NewSubagentTool(dispatcher).Execute(context.Background(), ToolCall{Args: map[string]any{
		"tasks": []any{
			map[string]any{"id": "kept", "prompt": "one"},
			map[string]any{"id": "expected", "prompt": "two"},
			map[string]any{"id": "empty", "prompt": "three"},
			map[string]any{"id": "unstarted", "prompt": "four"},
			map[string]any{"id": "duplicate", "prompt": "five"},
			map[string]any{"id": "duplicate", "prompt": "six"},
		},
	}})

	require.ErrorIs(t, err, cause)
	require.Contains(t, err.Error(), "trusted-completed")
	for _, forbidden := range []string{"wrong-task-output", "unstarted-output", "duplicate-first", "duplicate-second", "extra-output"} {
		require.NotContains(t, err.Error(), forbidden)
	}
}
