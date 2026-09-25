package core

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type runIDCapturingAgent struct {
	mu  sync.Mutex
	ids []string
}

type inertRunCommitSink struct{}

func (inertRunCommitSink) CommitInput(context.Context, RunCommitInput) error { return nil }
func (inertRunCommitSink) BeginIntentBatch(context.Context, RunIntentBatch) error {
	return nil
}
func (inertRunCommitSink) RecordExecution(context.Context, ActionExecution) error { return nil }
func (inertRunCommitSink) RecordReceipt(context.Context, RunToolReceipt) error    { return nil }
func (inertRunCommitSink) ResolveExecution(context.Context, ExecutionResolution) error {
	return nil
}
func (inertRunCommitSink) CommitCompaction(context.Context, CompactionProjection) error {
	return nil
}
func (inertRunCommitSink) CommitTerminal(context.Context, RunTerminal) error { return nil }

func (*runIDCapturingAgent) Name() string { return "run-id-capturing-agent" }

func (a *runIDCapturingAgent) Run(ctx context.Context, _ Submission, _ ...EventStream) (Result, error) {
	runID := RunIDFromContext(ctx)
	a.mu.Lock()
	a.ids = append(a.ids, runID)
	a.mu.Unlock()
	return Result{Message: runID}, nil
}

func (a *runIDCapturingAgent) captured() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.ids...)
}

func TestRunCommitSinkContextLookupAndMasking(t *testing.T) {
	require.Empty(t, RunIDFromContext(nil))
	missing, ok := RunCommitSinkFromContext(nil)
	require.False(t, ok)
	require.Nil(t, missing)
	sink := inertRunCommitSink{}
	parent := WithRunCommitSink(context.Background(), sink)
	got, ok := RunCommitSinkFromContext(parent)
	require.True(t, ok)
	require.Equal(t, sink, got)

	masked := WithRunCommitSink(parent, nil)
	got, ok = RunCommitSinkFromContext(masked)
	require.False(t, ok)
	require.Nil(t, got)
}

func TestHarnessRunContextUsesOwnedRunID(t *testing.T) {
	agent := &runIDCapturingAgent{}
	harness := NewHarnessImpl(agent)
	parentCtx := withRunID(context.Background(), "parent-run")
	var resultIDs []string
	var statusIDs []string

	for _, prompt := range []string{"first", "second"} {
		stream, err := harness.Submit(parentCtx, prompt)
		require.NoError(t, err)
		for range stream.Events() {
		}
		message, err := stream.Result()
		require.NoError(t, err)
		require.NotEmpty(t, message.Content)
		resultIDs = append(resultIDs, message.Content)
		statusSource, ok := stream.(RunStatusSource)
		require.True(t, ok)
		handle := statusSource.RunStatusHandle()
		require.NotNil(t, handle)
		statusIDs = append(statusIDs, handle.Snapshot().RunID)
	}

	captured := agent.captured()
	require.Len(t, captured, 2)
	require.Len(t, resultIDs, 2)
	require.Len(t, statusIDs, 2)
	require.NotEqual(t, "parent-run", captured[0])
	require.NotEqual(t, "parent-run", captured[1])
	require.Equal(t, resultIDs, captured, "callback context and final stream result must use the same RunID")
	require.Equal(t, statusIDs, captured, "callback context and owner result RunID must match")
	require.NotEqual(t, captured[0], captured[1], "separate runs must have separate identifiers")
}

func TestTurnRunnerRunContextUsesOwnedRunID(t *testing.T) {
	agent := &runIDCapturingAgent{}
	runner := NewEinoTurnRunner(&stubLoop{})
	runner.SetAgent(agent)
	parentCtx := withRunID(context.Background(), "parent-run")
	var resultIDs []string

	for _, prompt := range []string{"first", "second"} {
		result, err := runner.RunTurn(parentCtx, Submission{Content: prompt})
		require.NoError(t, err)
		require.NotEmpty(t, result.RunID)
		resultIDs = append(resultIDs, result.RunID)
	}

	captured := agent.captured()
	require.Len(t, captured, 2)
	require.NotEqual(t, "parent-run", captured[0])
	require.NotEqual(t, "parent-run", captured[1])
	require.Equal(t, resultIDs, captured, "callback context and owned result must use the same RunID")
	require.NotEqual(t, captured[0], captured[1], "separate runs must have separate identifiers")
}
